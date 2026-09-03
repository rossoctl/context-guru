package offload

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("collapse", newCollapse) }

// Collapse is the content-agnostic fallback for an oversized tool output that no
// more specific component handled: it keeps a head + tail window, stashes the
// full original, and leaves an expand marker. Runs late in the pipeline (after
// cmdfilter/format), and skips anything already carrying a marker so it never
// double-collapses.
//
// The window is cut by LINES when that actually shrinks the output, and by CHARACTERS when
// it does not. The character path is not a nicety. Being the last-resort catch-all,
// collapse declining a payload means nothing caps it, and the line-only version
// declined every output of head_lines+tail_lines lines or fewer — which includes the
// single most common shape of an enormous tool result: a database or HTTP API response
// serialised as ONE line of JSON. Measured on a live 128k-band arm: 16 upstream 400s
// ("prompt is too long") in 3,347 requests on outgoing bodies of 2.6 MB to 14.8 MB,
// and 17 of 75 runs errored. Nothing else caught them either — extract acted 0 times
// (no_obvious_noise 16,891, it only strips KNOWN noise patterns), cmdfilter 0
// (no_filter_match, only known command families), toon 21 (not_uniform_object_array
// 233,501), dedup 774 (exact duplicates only), extract_llm declines by design when the
// output exceeds the compaction model's own window (over_model_context), and
// summarize protects keep_last, which is exactly where a FRESH oversized output sits.
// linecap's per-line cap does not rescue this shape either: its neverTruncate
// allow-list exempts any line matching `^\S+:\d+`, and a one-line JSON body
// (`{"items":[{"id":1,...`) matches that by accident.
//
// The choice between the two windows is made on BYTES, not on the line count: an output
// with more than head_lines+tail_lines lines whose kept head or tail is itself
// multi-megabyte takes the CHARACTER window too. Gating on the line count alone left the
// motivating shape alive above 40 lines — measured at 4,195,111 B in, 4,194,922 B out, and
// worse than declining, because dropping the small filler lines in the middle counts as
// acting and stamps a marker, which then makes linecap decline (marker_or_kept_verbatim)
// and forwards 1.57 M tokens. linecap could not have capped it even without the marker:
// its neverTruncate allow-list exempts any line matching `^\S+:\d+`, which one-line JSON
// matches by accident (issue #151), so the mitigation this comment used to name is void
// rather than conditional.
//
// ponytail: []rune(content) on the character path copies the whole output — ~59 MB for the
// 14.8 MB payloads that motivated this component — once per oversized message per turn, on
// top of the strings.Split above it. Fine at the measured rate (16 bodies in 3,347
// requests) but it is the obvious thing to make streaming if that rate rises.
type Collapse struct {
	maxTokens int
	maxFrac   float64
	headLines int
	tailLines int
	mode      markerMode
	coldCache bool
}

type collapseConfig struct {
	MaxTokens  int     `yaml:"max_tokens"`
	MaxFrac    float64 `yaml:"max_frac"` // optional: threshold as a fraction of the model window (wins when window known)
	HeadLines  int     `yaml:"head_lines"`
	TailLines  int     `yaml:"tail_lines"`
	MarkerMode string  `yaml:"marker_mode"` // full (default) | summary | off
	// ColdCache lets a NEW collapse act at any depth on a turn whose prompt cache has
	// provably expired (see components.Ctx.TailOnlyCold). ON by default; see
	// coldCacheDefault.
	ColdCache *bool `yaml:"cold_cache"`
}

func newCollapse(raw []byte) (components.Component, error) {
	cfg := collapseConfig{MaxTokens: 2000, HeadLines: 20, TailLines: 20}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	// Clamp here rather than at each use: a negative head_lines used to panic on the line
	// path (`1 > -10` sends even a one-liner there, then lines[:-10] slices out of range),
	// which fail-open contained but which also made splitWindow's own guard unreachable for
	// the config it looked like it defended.
	return &Collapse{maxTokens: cfg.MaxTokens, maxFrac: cfg.MaxFrac, headLines: max(cfg.HeadLines, 0),
		tailLines: max(cfg.TailLines, 0), mode: parseMarkerMode(cfg.MarkerMode), coldCache: coldCacheDefault(cfg.ColdCache)}, nil
}

func (Collapse) Name() string                 { return "collapse" }
func (Collapse) Enabled(*components.Ctx) bool { return true }

func (cl *Collapse) Offload(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	maxTokens := resolveBudget(cl.maxTokens, cl.maxFrac, c.CtxWindow) // frac of window wins when known
	var keys []string
	changed := 0
	for i := range req.Input {
		m := &req.Input[i]
		if m.Role != schemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*m) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*m)
		if content == "" {
			continue
		}
		// Replay a previously-frozen collapse on EVERY turn, regardless of depth and BEFORE
		// the size test: the agent re-sends the original each turn, so the only way the
		// representation stays stable is to re-derive the same bytes. Ahead of the size test
		// on purpose — with max_frac set, CtxWindow can resolve differently mid-session
		// (model swap, refreshed modelinfo), and a threshold that drifts above this output
		// would otherwise flip it collapsed→full inside the cached prefix.
		if fk, _, ok := reapplyFrozen(c, rep, cl.Name(), m); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		if schema.TextTokens(content) <= maxTokens {
			rep.Gate("below_max_tokens")
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim") // already offloaded, or expanded by the agent
			continue
		}
		// Pick the window BEFORE the depth gate so a message that has no usable window is
		// not charged as a cache decline. assemble places the marker token; it is a pure
		// function of (content, config, maxTokens), which is what lets freeze replay it.
		assemble, byChars, ok := cl.window(content, maxTokens, rep)
		if !ok {
			continue // window() named the reason
		}
		// A NEW collapse only in the uncached tail. This component used to carry no depth
		// restriction at all, contradicting the contract in components/component.go: it
		// rewrote the whole transcript on every turn and survived only because the rewrite is
		// deterministic. It is not quite: `max_tokens` is resolved through resolveBudget, so a
		// max_frac config plus a mid-session CtxWindow change silently re-thresholds messages
		// inside the cached prefix. Tail gate + freeze is the same pair mask and failed_run
		// use, and it decides each output once, on the turn it arrives.
		if !c.TailOnlyCold(i, cl.coldCache) && !repairLostFreeze(c, cl.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		newText, key, eff, ok := tryMark(c, cl.mode, content, " [full output: call "+expand.ToolName+"]", assemble)
		if !ok {
			rep.Gate("marker_no_win") // head/tail window+marker wouldn't shrink this output
			continue
		}
		commitMark(c, rep, eff, key, content)
		schema.SetMessageText(m, newText)
		freeze(c, cl.Name(), content, newText) // freeze so later turns replay it (no churn)
		if byChars {
			// Counted as an EVENT, not a gate: this population used to be the
			// `too_few_lines` DECLINE, and exporting "handled" under the old decline name
			// would make the series fall as the component works better.
			rep.Event("char_window")
		}
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// collapseCharsPerToken converts the token threshold into a CHARACTER budget for the character
// window. 4 is not a fresh magic number: it is the same ratio internal/tokens.Count
// itself falls back to when the BPE codec is unavailable, so the two places in the tree
// that have to relate bytes to tokens agree on the exchange rate. It is only a starting
// estimate — the window is then MEASURED and tightened (see collapseFitPasses), because dense
// JSON tokenizes closer to 2.5 chars/token and a flat chars/4 window would overshoot by
// half the budget it exists to enforce.
const collapseCharsPerToken = 4

// collapseFitPasses bounds the corrective re-measure of the character window. Each pass scales
// the budget by the measured token ratio, so one pass is normally enough; the bound is
// there so a pathological tokenization cannot spin.
const collapseFitPasses = 3

// collapseMinWindowChars floors the character window. Without a floor, a max_tokens of 0 (or a
// tiny max_frac against a small window) collapses an output to nothing but a marker,
// which is a legal but useless rewrite: the model then has no cue about WHAT to expand.
const collapseMinWindowChars = 200

// window chooses how to cut content and returns the layout closure tryMark will
// size-check, plus whether the CHARACTER path was used. It names its own decline reason.
//
// Line window first, and byte-for-byte as it always was: anything the line path already
// handled must keep producing identical bytes, because a message's collapsed form is
// frozen and replayed at depth — a changed layout would re-anchor every cached position
// after it in every live session mid-flight.
func (cl *Collapse) window(content string, maxTokens int, rep *components.Report) (assemble func(string) string, byChars, ok bool) {
	lines := strings.Split(content, "\n")
	if cl.headLines+cl.tailLines > 0 && len(lines) > cl.headLines+cl.tailLines {
		omitted := len(lines) - cl.headLines - cl.tailLines
		head := strings.Join(lines[:cl.headLines], "\n")
		tail := strings.Join(lines[len(lines)-cl.tailLines:], "\n")
		asm := func(tok string) string {
			return fmt.Sprintf("%s\n... (%d lines omitted) %s\n%s", head, omitted, tok, tail)
		}
		// Take the line window only if it actually CUT: having more lines than the window
		// says nothing about their size, and a multi-megabyte line inside the kept head or
		// tail survives it untouched. Halving is the test because the line window is the
		// byte-identical path and every shape it handles today clears it by orders of
		// magnitude; the shapes that do not are the ones this fall-through exists for.
		//
		// BYTES, deliberately, and not a token comparison against maxTokens: maxTokens comes
		// from resolveBudget(..., c.CtxWindow), CtxWindow can resolve differently mid-session,
		// and repairLostFreeze re-derives at depth (see Offload). A token gate could therefore
		// classify the same content line-window on one turn and character-window on the next
		// and emit different bytes, trading a savings bug for a cache bug. This form depends
		// only on content, head_lines and tail_lines, so it is replay-safe unconditionally.
		if len(asm(""))*2 <= len(content) {
			return asm, false, true
		}
	}
	asm, ok := cl.runeWindow(content, maxTokens)
	if !ok {
		// The only genuine decline left: no usable line window AND fewer characters than the
		// smallest character one. `too_few_lines` is deliberately NOT reused — most of what
		// it counted is now HANDLED, and exporting handled work under a decline's name yields
		// a series that falls as the component works better.
		rep.Gate("too_few_lines_and_chars")
		return nil, false, false
	}
	return asm, true, true
}

// runeWindow is the head+tail window measured in RUNES rather than lines, for content the
// line window cannot cut. Runes, not bytes, so a multibyte character is never split into
// invalid UTF-8 (the text is spliced back into the request body verbatim).
//
// The budget is the component's own token threshold expressed in characters, split
// between head and tail in the head_lines:tail_lines ratio, so the existing knobs keep
// meaning what they say ("how much of the start / the end is kept") without a second set
// of char knobs to keep in sync. The assembled window is then measured against maxTokens and
// the budget scaled down if it overshoots, which brings an output above max_tokens back to
// roughly it — not exactly it, on two counts worth naming rather than implying otherwise.
// The collapseMinWindowChars floor returns before any measurement, so a very small
// max_tokens buys a 200-character window regardless (max_tokens: 100 measures 118 tokens
// out). And `got` measures asm("") — an EMPTY marker — while the shipped text carries the
// ~15 tokens of `<<cg:HASH>> [full output: ...]`, which only the 10% margin below absorbs.
// tryMark's never-worse guard is what makes the residue harmless: it measures the real
// marker and declines rather than growing the message.
func (cl *Collapse) runeWindow(content string, maxTokens int) (func(string) string, bool) {
	r := []rune(content)
	// Never seed above the content. chars/4 OVERESTIMATES on dense JSON (~2.5 chars/token),
	// so bailing out below on the un-measured seed declined content the fit loop would have
	// cut: at max_tokens 3000 — what the shipped codesafe preset sets — one-line JSON from
	// 3,006 to 4,491 tokens was oversized and left entirely uncut. marker_no_win already
	// protects the downside of seeding low.
	budget := min(maxTokens*collapseCharsPerToken, len(r)-1)
	for pass := 0; ; pass++ {
		if budget < collapseMinWindowChars {
			budget = collapseMinWindowChars
		}
		head, tail := cl.splitWindow(budget)
		if head+tail >= len(r) {
			return nil, false // the window is not smaller than the content; nothing to cut
		}
		h, t := head, tail // snapshot: the closure must not see a later pass's budget
		asm := func(tok string) string {
			return fmt.Sprintf("%s\n... (%d characters omitted) %s\n%s",
				string(r[:h]), len(r)-h-t, tok, string(r[len(r)-t:]))
		}
		if pass == collapseFitPasses || budget == collapseMinWindowChars {
			return asm, true
		}
		got := schema.TextTokens(asm(""))
		if got <= maxTokens {
			return asm, true
		}
		// Overshoot: scale by the measured ratio, with a 10% margin so a second pass is
		// rarely needed. Never grow the budget — this loop only ever tightens.
		next := budget * maxTokens / got * 9 / 10
		if next >= budget {
			return asm, true
		}
		budget = next
	}
}

// splitWindow divides a character budget between head and tail in the head_lines:tail_lines
// ratio, so `head_lines: 40, tail_lines: 10` keeps four fifths of the window at the start on
// the character path too. Both zero means an even split rather than an empty window, since a
// component configured with no line window still has to cap the output — and with both zero
// window() routes here rather than to the line path, which would otherwise emit a bare
// marker with no cue about what to expand (a 64 KB output became 87 bytes). Negatives are
// clamped in newCollapse, so this only has to handle zero.
func (cl *Collapse) splitWindow(budget int) (head, tail int) {
	hl, tl := cl.headLines, cl.tailLines
	if hl+tl == 0 {
		hl, tl = 1, 1
	}
	head = budget * hl / (hl + tl)
	return head, budget - head
}

func init() {
	components.RegisterFields("collapse", collapseConfig{}, []components.Field{
		{Key: "max_tokens", Type: components.FieldInt, Default: 2000,
			Hint: "Collapse any output above this many tokens to its head and tail. 0 leaves the threshold to max_frac."},
		{Key: "max_frac", Type: components.FieldFloat,
			Hint: "The same threshold as a fraction of the model's context window; wins when the window is known. 0 = unset."},
		{Key: "head_lines", Type: components.FieldInt, Default: 20,
			Hint: "Lines kept from the start of a collapsed output. When a line window would not shrink it (a one-line JSON payload, or a huge line inside the kept window), the same ratio splits a CHARACTER window sized from max_tokens."},
		{Key: "tail_lines", Type: components.FieldInt, Default: 20,
			Hint: "Lines kept from the end of a collapsed output. Also the tail share of the character window used when a line window would not shrink the output."},
		markerModeField(),
		coldCacheField(),
	})
}
