package offload

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// resolveTimeoutEnv reads a per-call model-timeout budget from the environment,
// falling back to def when unset or unparseable. A bare number is seconds, so "180"
// works as well as "180s".
//
// Shared by the two NeedsModel components (extract_llm, summarize) because their
// budgets are the same KIND of knob — a client-side assumption about how long a
// loaded server takes to answer — and the parse rules must not drift between them:
// one accepting "180" while the other silently fell back to its default would be
// invisible in a run and would look like the component not firing.
func resolveTimeoutEnv(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Second
		}
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return def
}

// resolveBudget converts an absolute token knob + an optional fraction-of-window
// into an effective token count: the fraction (ceil(frac*window)) wins when set and
// the window is known, else the absolute. Lets size knobs (collapse.max_tokens)
// scale with the model context window while staying backward compatible when no
// fraction is configured or the window is unknown.
func resolveBudget(absolute int, frac float64, window int) int {
	if frac > 0 && window > 0 {
		return int(math.Ceil(frac * float64(window)))
	}
	return absolute
}

// headPeek returns a single-line, whitespace-collapsed, clipped snippet of the
// start of content — the cue an age/budget-based offloader (mask) can
// leave inside its marker so the model knows WHAT was hidden without a blind
// expand round-trip. Replay/diff analysis showed a bare "[older tool output
// masked]" marker gives the model zero signal (e.g. it masked a still-relevant
// source-file read on SWE), forcing needless expand calls; a ~1-line peek fixes
// that at negligible token cost (the marker-inclusive never-worse guard in
// tryMark still drops the rewrite if the peek would make it not shrink).
// maxChars<=0 disables the peek (returns "").
func headPeek(content string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	// Peek across the START of the content (not just the first line): a bounded
	// prefix, newlines→spaces, whitespace collapsed. This keeps the cue useful for
	// line-numbered file reads (first line is a bare number) as well as command
	// output (first line "Exit code 1 / Traceback ..."). Slice first so we never
	// scan a multi-KB output.
	head := clipRunes(content, maxChars*4)
	head = strings.Join(strings.Fields(head), " ") // newlines+runs → single spaces
	if head == "" {
		return ""
	}
	// never let a peek carry a marker sentinel back into the request
	head = strings.ReplaceAll(head, "<<cg:", "<< cg:")
	if r := []rune(head); len(r) > maxChars {
		head = string(r[:maxChars]) + "…"
	}
	return head
}

// clipRunes returns at most maxBytes bytes of s truncated on a UTF-8 rune boundary,
// so a multibyte rune is never split into invalid UTF-8 (which would corrupt the
// marker text spliced back into the request body).
func clipRunes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		if maxBytes <= 0 {
			return ""
		}
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b]
}

// progressNoiseRe matches a line that is ENTIRELY a progress indicator (tqdm/pip
// bars, percentage-only lines, spinner/byte-counter redraws) — safe to drop because
// it carries no information the agent acts on. Deliberately narrow to avoid eating
// real content: the whole line must be bar/percent glyphs + digits + separators.
var progressNoiseRe = regexp.MustCompile(`^[\s\d./%|=><:\-\[\]()x,]*[%█━▏▎▍▌▋▊▉►][\s\d./%|=><:\-\[\]()x,█━▏▎▍▌▋▊▉►]*$`)

// ansiRe matches ANSI/VT100 escape sequences (colors, cursor moves, etc.). Stripping
// them is universally safe — they are pure terminal display control, never content —
// and general across any tool/agent/benchmark. Reversible (the original is stashed).
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// stripTerminalNoise removes ANSI escapes and collapses carriage-return progress
// redraws (a line rewritten in place by \r keeps only its final rendered segment).
// Returns (cleaned, changed). Deterministic, content-preserving except for pure
// terminal control — the safe, zero-cost first pass before structural collapsing.
func stripTerminalNoise(s string) (string, bool) {
	changed := false
	if ansiRe.MatchString(s) {
		s = ansiRe.ReplaceAllString(s, "")
		changed = true
	}
	if strings.Contains(s, "\r") {
		lines := strings.Split(s, "\n")
		for i, ln := range lines {
			// A trailing "\r" is just the CRLF (\r\n) line separator surfacing after the
			// split on "\n" — NOT a redraw. Treating it as one would keep only ln[len:] = ""
			// and blank every CRLF line (silent content loss). Only a CR *inside* the line
			// is an in-place progress redraw; there we keep the final rendered segment and
			// re-attach the original trailing CR so line endings stay byte-identical.
			core := strings.TrimSuffix(ln, "\r")
			j := strings.LastIndexByte(core, '\r')
			if j < 0 {
				continue // no interior CR: pure content (possibly CRLF) — leave untouched
			}
			seg := core[j+1:]
			if strings.HasSuffix(ln, "\r") {
				seg += "\r"
			}
			lines[i] = seg
			changed = true
		}
		s = strings.Join(lines, "\n")
	}
	return s, changed
}

// collapseObviousNoise is the CONSERVATIVE deterministic reducer: it deletes ONLY
// provably-redundant noise and keeps every unique informative line verbatim, so it
// can never hide content the agent needs and force a redo (the failure mode of blind
// head/tail truncation). It removes: runs of blank lines (→ one), consecutively
// repeated single lines or multi-line blocks (keeping one copy — e.g. a traceback
// dumped 50× by a retry loop), and pure progress-bar/spinner lines. Everything else
// is kept. Relevance-aware trimming is the LLM strategy's job, not this one. Returns
// changed=false when there was no obvious noise to drop (leave the output untouched).
func collapseObviousNoise(content string) (string, bool) {
	content, termChanged := stripTerminalNoise(content) // universally-safe first pass
	lines := strings.Split(content, "\n")
	n := len(lines)
	out := make([]string, 0, n)
	changed := false
	blocksEqual := func(a, b, k int) bool {
		for j := 0; j < k; j++ {
			if lines[a+j] != lines[b+j] {
				return false
			}
		}
		return true
	}
	const maxBlock = 12
	for i := 0; i < n; {
		ln := lines[i]
		if strings.TrimSpace(ln) != "" && progressNoiseRe.MatchString(ln) && strings.ContainsAny(ln, "%█━▏▎▍▌▋▊▉►") {
			changed = true
			i++
			continue
		}
		if strings.TrimSpace(ln) == "" { // collapse blank runs to one
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				changed = true
				i++
				continue
			}
			out = append(out, ln)
			i++
			continue
		}
		// consecutively repeated block of size k (k=1 covers duplicate lines):
		// keep one copy, drop the immediate repeats.
		collapsed := false
		for k := 1; k <= maxBlock && i+2*k <= n; k++ {
			if !blocksEqual(i, i+k, k) {
				continue
			}
			reps := 2
			for i+(reps+1)*k <= n && blocksEqual(i, i+reps*k, k) {
				reps++
			}
			out = append(out, lines[i:i+k]...)
			i += reps * k
			changed = true
			collapsed = true
			break
		}
		if collapsed {
			continue
		}
		out = append(out, ln)
		i++
	}
	if !changed {
		return content, termChanged // terminal-noise stripping may have changed it alone
	}
	return strings.Join(out, "\n"), true
}

// errWords mark a tool output (or an item) as carrying a failure — such items
// are preserved by smartcrush and prioritized elsewhere, since dropping the one
// error in a haystack is exactly the accuracy loss to avoid.
var errWords = []string{"error", "fail", "exception", "panic", "fatal", "traceback"}

func hasError(s string) bool {
	ls := strings.ToLower(s)
	for _, w := range errWords {
		if strings.Contains(ls, w) {
			return true
		}
	}
	return false
}

// goalCap bounds the conversational context handed to an LLM component so a huge
// task statement can't blow up every prompt. Generous — the point is to pass the
// real task, not one trailing sentence.
const goalCap = 8000

// --- How much conversation the extraction prompt carries -------------------------
//
// The model is asked to reduce ONE tool output "toward what the agent needs next", so what
// it is told about the conversation is the whole basis of that judgement. More context
// means better decisions and a bigger prompt on every call, which is a cost/quality dial
// the operator should own rather than a constant.

// contextMode selects how much conversation goes into an extraction prompt.
type contextMode string

// The context modes.
//
//	goal   — the task statement plus the current intent. The cheapest useful signal, and
//	         what this component sent before the mode existed.
//	recent — every user turn plus the last N non-tool messages (the DEFAULT): the task, all
//	         the corrections since, and what the agent has just been doing.
//	full   — the entire transcript, tool outputs included. Expensive per call; the right
//	         answer when the whole transcript is being swept at once and the request is
//	         being re-billed from scratch anyway.
const (
	ctxGoal   contextMode = "goal"
	ctxRecent contextMode = "recent"
	ctxFull   contextMode = "full"
)

// defaultContextMessages is the N for `recent`. Seven is enough to hold a tool call, its
// result and the reasoning either side of it without carrying a whole session.
const defaultContextMessages = 7

// Per-mode byte budgets. `recent` gets more room than a bare goal because it carries
// several turns; `full` gets a large one because its whole purpose is completeness, and
// the real bound on it is the extraction model's context window — checked by
// fitsModelContext, which declines the call rather than truncating the conversation.
const (
	recentContextCap = 24_000
	fullContextCap   = 400_000
)

func parseContextMode(s string) (contextMode, error) {
	switch m := contextMode(s); m {
	case "", ctxRecent:
		return ctxRecent, nil
	case ctxGoal, ctxFull:
		return m, nil
	default:
		return ctxRecent, fmt.Errorf("context must be goal|recent|full, got %q", s)
	}
}

// conversationContext renders the conversation for an extraction prompt in the requested
// mode. Always returns something usable: an empty result would strip the only relevance
// signal the model has, so every mode falls back to the goal.
func conversationContext(req *bschemas.BifrostChatRequest, mode contextMode, lastN int) string {
	switch mode {
	case ctxFull:
		if s := renderTranscript(req, len(req.Input), true, fullContextCap); s != "" {
			return s
		}
	case ctxRecent:
		if lastN <= 0 {
			lastN = defaultContextMessages
		}
		if s := renderTranscript(req, lastN, false, recentContextCap); s != "" {
			return s
		}
	}
	return conversationGoal(req)
}

// renderTranscript writes "role: text" lines for every user turn plus the last n messages,
// oldest first. withTools includes tool results (the bulk this component exists to reduce),
// so only `full` asks for them.
//
// The last-n window is counted over the messages that will actually be RENDERED, not over
// raw indices: in agent traffic most messages are tool results, so counting raw indices
// would make "the last 7" resolve to nothing but output the model is about to be asked to
// cut anyway.
func renderTranscript(req *bschemas.BifrostChatRequest, n int, withTools bool, cap int) string {
	type line struct {
		role, text string
	}
	var all []line
	for i := range req.Input {
		role := string(req.Input[i].Role)
		isTool := req.Input[i].Role == bschemas.ChatMessageRoleTool
		if isTool && !withTools {
			continue
		}
		txt := strings.TrimSpace(schema.MessageText(req.Input[i]))
		if txt == "" {
			continue
		}
		all = append(all, line{role, txt})
	}
	keep := make([]bool, len(all))
	for i, l := range all {
		if l.role == string(bschemas.ChatMessageRoleUser) || i >= len(all)-n {
			keep[i] = true
		}
	}
	var b strings.Builder
	for i, l := range all {
		if !keep[i] {
			continue
		}
		b.WriteString(l.role)
		b.WriteString(": ")
		b.WriteString(l.text)
		b.WriteString("\n\n")
	}
	// Clip from the FRONT: the most recent turns are the ones that say what the agent needs
	// next, so an over-long transcript must lose its oldest part, not its newest.
	out := strings.TrimSpace(b.String())
	if len(out) > cap {
		out = "…[earlier conversation elided]…\n\n" + clipRunesTail(out, cap)
	}
	return out
}

// clipRunesTail returns the LAST maxBytes bytes of s, starting on a rune boundary.
//
// The boundary test is utf8.RuneStart, not utf8.ValidString(s[:1]): a single byte is only
// "valid UTF-8" when it is ASCII, so testing it that way advanced past every leading
// multi-byte rune and returned "" for any non-ASCII tail — which left the extraction model
// with the elision marker and no conversation at all.
func clipRunesTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[len(s)-maxBytes:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// conversationGoal is the relevance/context signal for the LLM components: the
// TASK the agent is working (first user turn — in agent traffic this holds the
// problem statement), plus the most recent assistant and user turns (its current
// intent). Passing this instead of just the last message is what lets extract /
// summarize keep what actually matters, on any agent or benchmark. Tool outputs
// are excluded — they are the bulk being reduced, not the goal.
func conversationGoal(req *bschemas.BifrostChatRequest) string {
	var firstUser, lastUser, lastAsst string
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleUser {
			firstUser = schema.MessageText(req.Input[i])
			break
		}
	}
	for i := len(req.Input) - 1; i >= 0; i-- {
		switch req.Input[i].Role {
		case bschemas.ChatMessageRoleUser:
			if lastUser == "" {
				lastUser = schema.MessageText(req.Input[i])
			}
		case bschemas.ChatMessageRoleAssistant:
			if lastAsst == "" {
				lastAsst = schema.MessageText(req.Input[i])
			}
		}
		if lastUser != "" && lastAsst != "" {
			break
		}
	}
	var parts []string
	seen := map[string]struct{}{}
	for _, p := range []string{firstUser, lastAsst, lastUser} {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		parts = append(parts, p)
	}
	// clipRunes, not g[:goalCap]: a byte slice can split a multi-byte rune and hand the
	// model invalid UTF-8 mid-prompt. This never reaches the wire (the goal is prompt-only)
	// so the blast radius is small, but a mangled goal is a silently worse extraction and
	// the correct helper is twenty lines up.
	return clipRunes(strings.Join(parts, "\n\n"), goalCap)
}

// keywords extracts lowercased content words (>3 chars) as a set — a cheap
// relevance signal without embeddings.
func keywords(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(f) > 3 {
			out[f] = struct{}{}
		}
	}
	return out
}

// toolIndices returns the indices of tool-role messages, in order.
func toolIndices(req *bschemas.BifrostChatRequest) []int {
	var out []int
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleTool {
			out = append(out, i)
		}
	}
	return out
}
