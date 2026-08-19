package offload

import (
	"math"
	"strconv"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("coref", newCoref) }

// Coref is co-reference-aware compaction: at a threshold crossing it cuts the tool
// outputs that later turns never carried anything forward from, in ONE batched pass.
// Design and derivation: docs/proposals/coref-compaction.md.
//
// It differs from every other offloader here in one deliberate way, and the whole shape
// of the component follows from it: it MUTATES THE CACHED PREFIX on purpose. Age-based
// offloaders refuse to (Ctx.TailOnly) because breaking the prefix hash at index i
// cache-WRITES the suffix, at 11.5x a cache-read. So:
//
//   - The cut is BATCHED. One rewrite has to serve every cut in the pass, because a
//     single early cut can never repay its own rewrite on tokens: at 5k cut from 20%
//     depth of a 150k transcript the break-even is T > 276 turns. Batched, 60k of the
//     same transcript needs T > 23, which a long session actually reaches. Hence
//     min_batch_frac, and hence "rare and certain" rather than "per output per turn".
//   - The spend is BUDGETED and reported, not incidental (rewrite_budget). A component
//     that spends cache-writes on purpose has to be answerable for how many.
//   - The decision is LATCHED and replayed, never re-derived. See the freeze note in
//     Offload; this is the constraint that rules out repairLostFreeze.
//
// The default cut set is `unreferenced` only — outputs no later turn exactly reuses
// anything from. That is the honest ceiling of a zero-LLM implementation and needs no
// calibrated threshold. `closed` (the case-A large cut: referenced once or twice, long
// ago) is off by default because its two thresholds are the OUTPUT of the measurement
// pass in deploy/harbor/coref.py, which has not yet run on real traffic; shipping a
// guessed closed_dist would be shipping the one number the proposal says must be
// measured. Turn it on with cut_closed once there are numbers.
type Coref struct {
	trigger         components.Trigger
	minTokens       int
	closedDist      int
	openReps        int
	minLaterTurns   int
	cutUnreferenced bool
	cutClosed       bool
	rewriteBudget   int
	minBatchFrac    float64
	breakEven       bool
	keepHeadChars   int
	mode            markerMode
}

type corefConfig struct {
	Trigger components.Trigger `yaml:"trigger"`
	// MinTokens is the per-output floor; matches coref.py's min_output default so the
	// component and the measurement consider the same population.
	MinTokens int `yaml:"min_tokens"`
	// ClosedDist / OpenReps are the open-vs-closed thresholds. Defaults mirror
	// coref.py's, and are placeholders until it runs on captured traffic.
	ClosedDist int `yaml:"closed_dist"`
	OpenReps   int `yaml:"open_reps"`
	// MinLaterTurns is the opportunity floor: an output with fewer model turns after it
	// than this is never cut, because it has not yet had a chance to be referenced.
	MinLaterTurns *int `yaml:"min_later_turns"`
	// CutUnreferenced / CutClosed select the cut set (see Coref).
	CutUnreferenced *bool `yaml:"cut_unreferenced"`
	CutClosed       *bool `yaml:"cut_closed"`
	// RewriteBudget caps prefix-rewrite passes per session. 0 disables the component's
	// only cache-spending path entirely (replay of already-latched decisions continues).
	RewriteBudget *int `yaml:"rewrite_budget"`
	// MinBatchFrac is the batching constraint: the pass must cut at least this fraction
	// of the request before it is worth a rewrite.
	//
	// The default was 0.15, derived from the illustrative arithmetic in the proposal's §4
	// and never checked against how much mass is actually available. Measured, Tier-1
	// matching finds a mean 4.4% of the request (`unreferenced`) or 9.6% (`+closed`) on real
	// long sessions — so 0.15 admitted ONE of nineteen sessions past the agent's compaction
	// threshold, and zero at the shipped cut set. A gate no traffic can clear is not a
	// conservative default, it is an off switch that looks like a threshold. 0.05 admits
	// 16/19; the honest position is that the right value is an experimental result and this
	// is a starting point, not a claim.
	MinBatchFrac *float64 `yaml:"min_batch_frac"`
	// BreakEven applies the S*T > 11.5*W inequality with an estimated T. Ignored when
	// the context window is unknown, like every other fraction-based threshold here.
	BreakEven *bool `yaml:"break_even"`
	// KeepHeadChars leaves a one-line peek inside the marker so the model knows what was
	// cut without a blind expand round-trip; 0 disables.
	KeepHeadChars *int   `yaml:"keep_head_chars"`
	MarkerMode    string `yaml:"marker_mode"` // full (default) | summary | off
}

func newCoref(raw []byte) (components.Component, error) {
	cfg := corefConfig{MinTokens: 300, ClosedDist: 12, OpenReps: 3}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	cf := &Coref{
		trigger:         cfg.Trigger,
		minTokens:       cfg.MinTokens,
		closedDist:      cfg.ClosedDist,
		openReps:        cfg.OpenReps,
		minLaterTurns:   8,
		cutUnreferenced: true,
		cutClosed:       false,
		rewriteBudget:   3,
		minBatchFrac:    0.05,
		breakEven:       true,
		keepHeadChars:   96,
		mode:            parseMarkerMode(cfg.MarkerMode),
	}
	if cfg.MinLaterTurns != nil {
		cf.minLaterTurns = *cfg.MinLaterTurns
	}
	if cfg.CutUnreferenced != nil {
		cf.cutUnreferenced = *cfg.CutUnreferenced
	}
	if cfg.CutClosed != nil {
		cf.cutClosed = *cfg.CutClosed
	}
	if cfg.RewriteBudget != nil {
		cf.rewriteBudget = *cfg.RewriteBudget
	}
	if cfg.MinBatchFrac != nil {
		cf.minBatchFrac = *cfg.MinBatchFrac
	}
	if cfg.BreakEven != nil {
		cf.breakEven = *cfg.BreakEven
	}
	if cfg.KeepHeadChars != nil {
		cf.keepHeadChars = *cfg.KeepHeadChars
	}
	return cf, nil
}

func (Coref) Name() string                 { return "coref" }
func (Coref) Enabled(*components.Ctx) bool { return true }

// cacheWriteX is one cache-write in cache-read-equivalents: ($2.50 - $0.20) / $0.20 on
// Anthropic's published per-MTok prices. Shared with deploy/harbor/coref.py.
const cacheWriteX = 11.5

// plannedCut is one accepted candidate, held until the whole batch clears its gates —
// nothing is stashed or rewritten before then, so a batch that fails a gate leaves the
// request byte-identical.
type plannedCut struct {
	idx      int
	original string
	newText  string
	key      string
	eff      markerMode
	saved    int
}

func (cf *Coref) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// The trigger is pure request shape, so ask before paying for the index. Replay
	// (below) is NOT gated on it: a latched cut must be re-applied on every turn whether
	// or not this turn would have decided to cut anything, or the output flips
	// cut→full→cut and churns the very cache this component is budgeting.
	fires := cf.trigger.Fires(req, c.CtxWindow)

	// Index the PRISTINE request, before any replay rewrites a message. Two reasons, and
	// the second is the load-bearing one: the agent re-sends originals every turn, so the
	// pristine transcript is the stable input the offline measurement also reads — and if
	// the index were built after replay, an earlier cut would remove identifiers from the
	// exclusion sets and silently reclassify unrelated outputs. That is history dependence
	// on our OWN past output, which is how a "keep" turns into a "cut" turns into a
	// different set of bytes at the same index.
	classes := map[int]coref.Class{}
	if fires {
		for _, r := range coref.Index(flattenForCoref(req), cf.trigger.OutputFloor(c.CtxWindow, cf.minTokens), schema.TextTokens) {
			classes[r.Idx] = coref.Classify(r, cf.closedDist, cf.openReps, cf.minLaterTurns)
		}
	}

	pristineTokens := schema.MessagesTokens(req)
	var keys []string
	changed := 0

	// Replay latched decisions, on every tool output, every turn, at any depth.
	replayed := map[int]bool{}
	for _, i := range toolIndices(req) {
		m := &req.Input[i]
		if !schema.Rewritable(*m) || schema.MessageText(*m) == "" {
			continue
		}
		if fk, _, ok := reapplyFrozen(c, cf.Name(), m); ok {
			replayed[i] = true
			changed++
			keys = append(keys, fk...)
		}
	}

	if !fires {
		rep.Gate("trigger")
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	// A new pass spends a cache-write. Ask before planning one.
	spent := corefRewrites(c)
	if cf.rewriteBudget <= 0 || spent >= cf.rewriteBudget {
		rep.Gate("rewrite_budget")
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	plan, err := cf.planCuts(req, rep, c, classes, replayed)
	if err != nil {
		return keys, err
	}
	if len(plan) == 0 {
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}

	saved := 0
	for _, p := range plan {
		saved += p.saved
	}
	// Batching: one rewrite must serve the whole pass, so the pass has to be big enough
	// to be worth one. A batch below the floor is not a small win, it is a loss.
	if cf.minBatchFrac > 0 && pristineTokens > 0 &&
		float64(saved) < cf.minBatchFrac*float64(pristineTokens) {
		rep.Gate("batch_too_small")
		if changed == 0 {
			rep.Skipped = true
		}
		return keys, nil
	}
	if cf.breakEven {
		if _, _, ok := cf.breakEvenTurns(req, plan, c); !ok {
			rep.Gate("break_even")
			if changed == 0 {
				rep.Skipped = true
			}
			return keys, nil
		}
	}

	// Commit the whole batch, then charge the session exactly one rewrite for it.
	for _, p := range plan {
		commitMark(c, rep, p.eff, p.key, p.original)
		schema.SetMessageText(&req.Input[p.idx], p.newText)
		// Latch. From here the bytes for this content are fixed for the session: the next
		// turn replays them rather than re-deciding, because the decision is a function of
		// the transcript that existed when it was taken, not of the content alone.
		//
		// This is also why coref must NOT consult repairLostFreeze, which mask and
		// failed_run legitimately do. That repair re-derives a lost decision at depth on the
		// grounds that the replacement is a pure function of (content, config) and so
		// reproduces the bytes the provider already cached. A co-reference decision is
		// history-dependent by construction — re-deriving it against a longer transcript can
		// yield a DIFFERENT class and different bytes, which is precisely the prefix flip the
		// repair exists to avoid. A lost coref freeze therefore just declines.
		freeze(c, cf.Name(), p.original, p.newText)
		changed++
		if p.key != "" {
			keys = append(keys, p.key)
		}
	}
	setCorefRewrites(c, spent+1)

	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// planCuts builds the batch without side effects: every candidate is size-checked with
// its marker included (tryMark stashes nothing), so a batch that later fails a gate
// leaves the request untouched.
func (cf *Coref) planCuts(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	classes map[int]coref.Class, replayed map[int]bool) ([]plannedCut, error) {

	floor := cf.trigger.OutputFloor(c.CtxWindow, cf.minTokens)
	var plan []plannedCut
	for _, i := range toolIndices(req) {
		if replayed[i] {
			continue // already latched; counted above, never re-decided
		}
		m := req.Input[i]
		if !schema.Rewritable(m) {
			rep.Gate("non_text_blocks")
			continue
		}
		content := schema.MessageText(m)
		if content == "" || schema.TextTokens(content) < floor {
			rep.Gate("below_min_tokens")
			continue
		}
		if skipReduce(c, content) {
			rep.Gate("marker_or_kept_verbatim")
			continue
		}
		class, ok := classes[i]
		if !ok {
			rep.Gate("not_indexed") // below the index floor, or not a recorded output
			continue
		}
		if !(class == coref.Unreferenced && cf.cutUnreferenced) &&
			!(class == coref.Closed && cf.cutClosed) {
			rep.Gate("class_" + string(class))
			continue
		}
		// The marker states WHAT was removed and never why it was safe to remove. The
		// earlier wording ("no later turn referred back to it") asserted the one claim that
		// is false whenever the reference was transformed or semantic — tiers 2 and 3, which
		// this index cannot see — so it read as reassurance and discouraged the expand call
		// that would have repaired the mistake. Since only the model can initiate recovery, a
		// marker that talks it out of recovering is worse than an opaque one.
		note := "[tool output compacted"
		if stub := corefStub(content); stub != "" {
			// Structured content: describe the shape, which is addressable. "200 records,
			// fields: address, id, name" tells a model hunting for an address that this is
			// where addresses live; a head peek of one arbitrary row does not.
			note += "; " + stub
		} else if peek := headPeek(content, cf.keepHeadChars); peek != "" {
			note += "; starts: " + peek
		}
		note += "] "
		newText, key, eff, ok := tryMark(c, cf.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return note + tok })
		if !ok {
			rep.Gate("marker_no_win")
			continue
		}
		plan = append(plan, plannedCut{
			idx: i, original: content, newText: newText, key: key, eff: eff,
			saved: schema.TextTokens(content) - schema.TextTokens(newText),
		})
	}
	return plan, nil
}

// breakEvenTurns applies the inequality from the proposal's §4:
//
//	cost    = W x (2.50 - 0.20) = 11.5 x W   (in cache-read-equivalents)
//	benefit = S x T x 0.20      =  S x T
//	worth it when  S x T > 11.5 x W
//
// S is what the batch cuts; W is the suffix this cut forces the provider to re-write —
// counted from the shallowest cut index to the CACHED boundary, because content past it
// was never cached and would be written on this turn regardless; T is how many more
// turns the session has to collect the saving on, which is the quantity nobody has, so
// it is estimated from how fast the transcript has been growing.
//
// The consequence is counter-intuitive and worth stating: firing at 90% of the window
// means T is nearly zero, i.e. paying a rewrite for a saving that will be collected
// once. The profitable moment to compact is EARLIER than the moment of maximum pressure.
//
// Returns (needed T, estimated T, whether it clears). Always clears when the context
// window is unknown — same convention as every fraction-based threshold here: an
// unresolvable threshold imposes no constraint rather than silently disabling the pass.
func (cf *Coref) breakEvenTurns(req *bschemas.BifrostChatRequest, plan []plannedCut, c *components.Ctx) (need, have int, ok bool) {
	if c.CtxWindow <= 0 {
		return 0, 0, true
	}
	saved := 0
	shallowest := len(req.Input)
	for _, p := range plan {
		saved += p.saved
		if p.idx < shallowest {
			shallowest = p.idx
		}
	}
	if saved <= 0 {
		return 0, 0, false
	}
	// The rewritten span: from the shallowest mutated index up to the last message the
	// provider already holds. Unknown boundary => assume the whole transcript is cached,
	// which is the conservative direction (it over-states the cost, never under-states it).
	end := len(req.Input) - 1
	if c.CacheAware && c.MaxCachedIdx >= 0 && c.MaxCachedIdx < end {
		end = c.MaxCachedIdx
	}
	rewritten := 0
	for j := shallowest; j <= end && j < len(req.Input); j++ {
		rewritten += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	rewritten -= saved // the cut mass is not part of what gets written back

	need = int(math.Ceil(cacheWriteX * float64(rewritten) / float64(saved)))
	have = estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), c.CtxWindow)
	return need, have, need <= have
}

// estimateTurnsRemaining projects how many more turns fit before the request reaches the
// model's window, assuming the transcript keeps growing at the average rate it has so
// far. Crude on purpose: T only has to be right to an order of magnitude to separate
// "this rewrite pays for itself" from "this rewrite is charity", and every cheaper proxy
// (elapsed turns, observed step rate) is the same shape of guess.
func estimateTurnsRemaining(reqTokens, turns, window int) int {
	if window <= 0 || turns <= 0 || reqTokens <= 0 || reqTokens >= window {
		return 0
	}
	perTurn := reqTokens / turns
	if perTurn <= 0 {
		return 0
	}
	return (window - reqTokens) / perTurn
}

// modelTurns counts assistant messages — the closest thing in a request to "steps taken",
// which is the unit the growth rate is per.
func modelTurns(req *bschemas.BifrostChatRequest) int {
	n := 0
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleAssistant {
			n++
		}
	}
	return n
}

// flattenForCoref projects a request onto the neutral message list internal/coref
// indexes, 1:1 with req.Input indices so a Record points back at its message.
//
// The split is what defines a reference: a tool message is MASS (Results), everything
// else is a reference-bearing SURFACE (Texts) — prose plus the tool-call name and
// arguments, which is where a model names the path/symbol/id it took from an earlier
// output. A later tool result echoing a token is the environment repeating itself, not
// the model using the value, so it never counts as a reference.
func flattenForCoref(req *bschemas.BifrostChatRequest) []coref.Message {
	out := make([]coref.Message, len(req.Input))
	for i := range req.Input {
		m := req.Input[i]
		if m.Role == bschemas.ChatMessageRoleTool {
			id := ""
			if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
				id = *m.ChatToolMessage.ToolCallID
			}
			out[i] = coref.Message{Results: []coref.Result{{ID: id, Text: schema.MessageText(m)}}}
			continue
		}
		texts := []string{}
		if t := schema.MessageText(m); t != "" {
			texts = append(texts, t)
		}
		if m.ChatAssistantMessage != nil {
			for _, tc := range m.ChatAssistantMessage.ToolCalls {
				name := ""
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				texts = append(texts, name+" "+tc.Function.Arguments)
			}
		}
		out[i] = coref.Message{Texts: texts}
	}
	return out
}

// --- per-session rewrite budget ---------------------------------------------
//
// The one number this component is answerable for. Every other offloader's cache
// discipline is "never touch the prefix"; coref's is "touch it at most N times", so N
// has to be counted somewhere durable rather than inferred from the savings.

func corefRewritesKey(session string) string { return "cg:coref:rw:" + session }

func corefRewrites(c *components.Ctx) int {
	b, ok := c.Store.Get(corefRewritesKey(c.Session))
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		// Unreadable counter reads as EXHAUSTED, not as zero. Fail-open here means fail
		// open on the request (which is unaffected — no cut is taken), not fail open on an
		// unbounded cache spend.
		return math.MaxInt32
	}
	return n
}

func setCorefRewrites(c *components.Ctx, n int) {
	c.Store.Put(corefRewritesKey(c.Session), []byte(strconv.Itoa(n)))
}
