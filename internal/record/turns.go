package record

// Turn reconstruction: recovering the human's turns from request bodies alone.
//
// WHY THIS EXISTS
//
// claude-record emits an explicit `claude_code.interaction` span per human turn, carrying the
// prompt text, a sequence number and the wall-clock duration of the turn. A proxy never sees
// that span -- it is produced on the developer's machine. What a proxy does see is every model
// call in full, and every model call replays the whole conversation so far. So a human turn
// that the proxy never observed directly still arrives, inside the NEXT call's message list.
//
// This file turns that observation into turns. Everything it produces is DERIVED, and is
// emitted under keys that say so (see AttrTurn* in otel.go). Nothing here is written as
// `claude_code.interaction`: that key means "observed on the machine", and claiming it for a
// reconstruction would make a derived sequence number indistinguishable from a measured one.
//
// WHAT IT CANNOT DO, AND WHY NOT
//
// It cannot recover how long a turn took. `interaction.duration_ms` measures from the human's
// prompt to the end of the agent's answer; the proxy sees only the calls, so the gap between
// two calls mixes the human thinking with the agent working locally, and the two are not
// separable from this side. That number is structurally out of reach, not merely unimplemented.
//
// THE TRAP THIS FILE IS MOSTLY ABOUT
//
// `role: user` in a request body carries at least three different things:
//
//	1. genuine human turns
//	2. framework-injected blocks: <system-reminder>, command echoes, skill preambles,
//	   continuation notices, interruption markers, suggestion-mode probes
//	3. tool results (already split off as role:tool by convertMessage)
//
// Measured on 66 real sessions, roughly half of all role:user text is NOT the human. Counting
// naively yields 1,686 turns where 764 happened -- a 3.2x over-count. Filtering as below
// brings that to 658, and 27 of 66 sessions land on exactly the right number.
//
// A second trap costs more than the first: subagent traffic. A Task/Agent call and a WebFetch
// page dump both arrive as ordinary role:user text on their own model calls, so no content
// rule catches them. They are separated structurally instead, by conversation root -- see
// ConversationRoot.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
)

// ReconTurn is one recovered human turn.
type ReconTurn struct {
	// Sequence is 1-based over the turns recovered for one conversation, in the order the
	// bodies carried them. DERIVED: it is an ordering, and it equals claude-record's
	// interaction.sequence only when no turn was missed at either end.
	Sequence int

	// Text is the human's prompt, wrappers stripped, whitespace-normalised.
	Text string

	// FirstSeen identifies the model call whose body first carried this turn -- the ordering
	// key, and the closest thing to a timestamp a reconstruction has. Set by the caller to
	// the span id or start time it is working from.
	FirstSeen string

	// Root is the conversation this turn belongs to (see ConversationRoot). Turns from
	// different roots are different conversations and must not be merged into one sequence.
	Root string

	// Prior marks a turn that was already HISTORY in the first call observed for its root:
	// it sits before that call's last assistant message, so it was typed before the proxy saw
	// the conversation. Live, that is a resumed session -- Claude Code gives a resume a new
	// session id, and its first request replays the old conversation, so without this mark
	// every resume re-counts the earlier session's turns (a second developer's corpus: one prompt chain
	// replayed across three session files). It is also what a session started before
	// recording began looks like, and, offline, a session whose early calls were lost.
	// The proxy cannot tell those apart, so the turn is MARKED, not dropped.
	Prior bool
}

// injectedPrefixes are whole-message markers: text starting with one of these is framework
// output, never a human turn. Spelled as the corpus spells them -- these were read off 30,224
// role:user text parts, not guessed.
var injectedPrefixes = []string{
	"This session is being continued from a previous conversation",
	"[Request interrupted by user",
	"[Tool use was rejected",
	"[SUGGESTION MODE",
	"[Image #",
	// A tool_result image (e.g. the agent's own Read of a PNG, or a screen capture) gets
	// flattened to a caption and replayed as plain role:user text, same shape as the
	// pasted-image-ref case above but a different wrapper on each capture path:
	// "[Image: source: <filename>]", "[Image: original WxH, displayed at WxH...]". Measured
	// 2026-10-08 on a Kahoot-quiz-images session: inflated 9-10 real turns to 13.
	"[Image:",
	"The user stepped away and is coming back",
	"Describe your most recent action in 3-5 words",
	"Base directory for this skill:",
	"Caveat: The messages below were generated by the user while running",
	"API Error",
	"Web page content:", // WebFetch result replayed into a subagent's context
	"CRITICAL: Respond with TEXT ONLY",
	"Your directive:", // subagent task framing
	"Analyze if this message indicates a new conversation topic",
	"## Page contract", // Artifact tool's own skill-load preamble, injected as role:user

	// Cross-session and teammate traffic. Claude Code delivers a message from another
	// session, a teammate or a coordinator as an ordinary role:user text part, so it reads
	// exactly like a human turn and no wrapper encloses it. Measured on 5 of 101 sessions,
	// 125 spans; on one 206-call session it accounted for 12 of 18 apparent turns. These are
	// another AGENT talking, not our human.
	"The coordinator sent a message while you were working",
	"Another Claude session sent a message while you were working",
	"A teammate sent a message while you were working",

	// Harness notices that arrive as bare role:user text. Neither ever appears as a
	// claude_code.interaction user_prompt in either corpus (73 sessions from one developer, 33 from another),
	// i.e. Claude Code itself does not count them as human turns.
	"[SYSTEM NOTIFICATION - NOT USER INPUT]",
	"Continue from where you left off.",
}

// wrapperTags enclose injected content. They are STRIPPED rather than used to drop the whole
// message, because a message may hold genuine human text alongside them -- most commonly a
// <system-reminder> appended after a real prompt.
var wrapperTags = []string{
	"system-reminder",
	"local-command-caveat",
	"local-command-stdout",
	"local-command-stderr",
	"command-name",
	"command-message",
	"command-args",
	"fork-boilerplate",
	// NOT pasted_content: it encloses text the HUMAN pasted, and claude-record keeps it in
	// user_prompt. Stripping it dropped paste-only turns and broke matching on mixed ones
	// (4 of 51 turns with bodies on a third developer's corpus).
	"agent-message",         // a peer session's message, delivered as role:user text
	"cross-session-message", // same, under its other spelling
}

// sessionTag is handled differently from the rest: <session>...</session> WRAPS a genuine
// human prompt (it is how a resumed session's first turn arrives), so the content is kept and
// only the tag removed. Treating it as an injected marker silently drops a real turn.
const sessionTag = "session"

// normalizeSpace collapses all whitespace runs to single spaces and trims. Matching raw
// substrings against body JSON under-reports badly, because prompt text containing quotes is
// backslash-escaped inside the JSON string; comparisons must be made on PARSED text. An
// earlier measurement read 76% recovery for this reason alone, against a true 98%.
func normalizeSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading space is trimmed
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimRight(b.String(), " ")
}

// stripTagBlock removes every <tag ...> ... </tag> region, then any stray opener or closer.
// Case-insensitive; non-greedy, so two blocks in one message do not swallow the text between.
func stripTagBlock(s, tag string) string {
	open, close := "<"+tag, "</"+tag
	from := 0
	for {
		// ASCII-only lowering keeps every byte offset valid in s. strings.ToLower does not:
		// some characters change length when lowered (İ is 2 bytes, its lower form 1).
		low := asciiLower(s)
		k := strings.Index(low[from:], open)
		if k < 0 {
			break
		}
		i := from + k
		// Confirm this is the tag and not a longer name sharing the prefix.
		after := i + len(open)
		if after < len(s) && s[after] != '>' && !unicode.IsSpace(rune(s[after])) {
			// e.g. <system-reminders> when looking for <system-reminder>: skip past it and
			// keep looking, since a real block can still follow.
			from = after
			continue
		}
		end := strings.Index(low[i:], close)
		if end < 0 {
			// Unclosed opener: drop from the opener to end of that line.
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return s[:i]
			}
			s = s[:i] + " " + s[i+nl:]
			continue
		}
		// Advance past the closer's '>'.
		tail := i + end + len(close)
		if g := strings.IndexByte(s[tail:], '>'); g >= 0 {
			tail = tail + g + 1
		}
		s = s[:i] + " " + s[tail:]
	}
	return s
}

// asciiLower lowers A-Z only, so the result has exactly the byte length of s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// unwrapSession keeps the inside of <session>...</session> and drops the tags.
func unwrapSession(s string) string {
	low := strings.ToLower(s)
	i := strings.Index(low, "<"+sessionTag+">")
	if i < 0 {
		return strings.NewReplacer("<"+sessionTag+">", " ", "</"+sessionTag+">", " ").Replace(s)
	}
	start := i + len(sessionTag) + 2
	j := strings.Index(low[start:], "</"+sessionTag+">")
	if j < 0 {
		return s[start:]
	}
	return s[start : start+j]
}

// HumanText returns the human-authored text of one role:user text part, or "" if the part is
// framework-injected. This is the filter the "with work" column is about.
func HumanText(content string) string {
	s := content
	if strings.Contains(strings.ToLower(s), "<"+sessionTag+">") {
		s = unwrapSession(s)
	}
	for _, tag := range wrapperTags {
		s = stripTagBlock(s, tag)
	}
	n := normalizeSpace(s)
	if len(n) < 2 {
		// Nothing but wrappers, or a bare punctuation remnant.
		return ""
	}
	for _, p := range injectedPrefixes {
		if len(n) >= len(p) && strings.EqualFold(n[:len(p)], p) {
			return ""
		}
	}
	if isSkillReloadNotice(n) {
		return ""
	}
	return n
}

// isSkillReloadNotice matches the role:user line Claude Code appends after a Skill tool call
// for a skill already in context: "Skill /fetch is already loaded above; instructions
// unchanged." Matched as the WHOLE message, not by prefix, because "Skill /" alone could
// begin a real prompt. Seen 2026-10-07, where it made one /fetch count as two turns; it never
// appears as a claude_code.interaction user_prompt.
func isSkillReloadNotice(n string) bool {
	const head, tail = "Skill /", " is already loaded above; instructions unchanged"
	rest, ok := strings.CutPrefix(n, head)
	if !ok {
		return false
	}
	name, ok := strings.CutSuffix(strings.TrimSuffix(rest, "."), tail)
	return ok && name != "" && !strings.ContainsAny(name, " \t")
}

// ConversationRoot identifies which conversation a model call belongs to, from the set of tools
// the call advertises.
//
// # WHY TOOL DEFINITIONS AND NOT THE FIRST MESSAGE
//
// The obvious anchor is the conversation's first human turn: the main thread replays it on every
// call, each subagent has its own. That was tried and it FAILS on long sessions, measured. Once a
// conversation passes compaction the earliest turn drops out of the replayed window, so the same
// conversation silently acquires a new identity mid-session and its turns fragment across several
// "conversations". On the worst measured session it produced 32 roots for one session, five of
// them above any sensible threshold, and reconstructed 29 turns where 31 happened.
//
// The advertised tool list does not scroll out of the window, so it survives compaction. It also
// separates the threads that matter: the main agent advertises the full tool set, a subagent gets
// a restricted one (Explore has no Write or Edit), and a WebFetch-fed thread advertises none.
// Measured over 66 sessions: 2 to 5 distinct sets per session, the dominant set holding 83% of
// calls on average and never less than 50%.
//
// Returns "" when a call advertises no tools at all. That is the signature of a helper call --
// a WebFetch-fed thread, or Claude Code's own session-title generator -- and such calls are
// dropped. Measured: adopting them into the dominant conversation instead adds 13 turns of which
// only 2 are real, so it buys 1.7 points of apparent coverage by readmitting 11 non-turns.
func ConversationRoot(msgs []Message) string {
	return ""
}

// conversationAnchor hashes a call's advertised tool set. Callers pass the raw
// gen_ai.tool.definitions string; identical sets hash identically, which is the whole
// requirement.
func conversationAnchor(toolDefs string) string {
	t := strings.TrimSpace(toolDefs)
	if t == "" || t == "[]" || t == "null" {
		return ""
	}
	sum := sha1.Sum([]byte(t))
	return hex.EncodeToString(sum[:])[:12]
}

// Call is one observed model call, as the reconstruction needs it: the translated input
// messages plus a stable ordering key.
type Call struct {
	Order    string // sort key: span start time, or span id -- caller's choice, must be consistent
	Messages []Message

	// ToolDefs is the call's advertised tool schema, verbatim (gen_ai.tool.definitions). It is
	// the conversation anchor -- see ConversationRoot for why the first message is not.
	ToolDefs string
}

// RootShare is the fraction of a session's calls a conversation root must hold to count as a
// real conversation rather than a subagent. Measured: 64 of 66 sessions have exactly one root
// above this line; 2 have two, both genuine (a /clear mid-session starts a second
// conversation). Subagent roots sit far below it -- typically one to four calls each.
// Measured sweep on 100 sessions: raising it to 0.10/0.20/0.30 shaves over-count but costs
// real recall (92.7% -> 92.3% -> 91.4% -> 91.3%), trading good turns for bad at about 1:3.
// 0.05 is the measured optimum, not a guess.
const RootShare = 0.05

// MinRootCalls floors RootShare for short sessions, where 5% of a handful of calls would admit
// every subagent.
const MinRootCalls = 3

// subagentTools are the tools whose `prompt` argument becomes a subagent's first role:user
// message. Task is the older name of Agent.
var subagentTools = map[string]bool{"Agent": true, "Task": true}

// dispatchedPrompts collects, normalised as HumanText would leave them, every prompt the
// session handed to a subagent.
//
// A subagent's opening message is text the AGENT wrote, not the human, yet it arrives as an
// ordinary role:user part. The tool-set anchor separates most subagents, but not all of them:
// a code-review fan-out made more calls than the main thread on two of a second developer's sessions (68 vs
// 17 on one), so its root cleared RootShare, and a general-purpose subagent shares the main
// thread's tool set and so its anchor. Matching the dispatch itself catches both, because the
// main thread replays the assistant's Agent tool_call, prompt included, on every later call.
func dispatchedPrompts(calls []Call) map[string]bool {
	out := make(map[string]bool)
	for _, c := range calls {
		for _, m := range c.Messages {
			if m.Role != "assistant" {
				continue
			}
			for _, p := range m.Parts {
				if p.Type != PartToolCall || !subagentTools[p.Name] {
					continue
				}
				var args struct {
					Prompt string `json:"prompt"`
				}
				if json.Unmarshal([]byte(p.Arguments), &args) != nil {
					continue
				}
				if h := HumanText(args.Prompt); h != "" {
					out[h] = true
				}
			}
		}
	}
	return out
}

// ReconstructTurns recovers the human turns from a session's model calls.
//
// Calls need not be sorted; they are ordered by Call.Order here. Turns are de-duplicated by
// text within a conversation root, because every subsequent call replays them.
//
// Roots holding fewer than max(MinRootCalls, RootShare*len(calls)) calls are dropped as
// subagent traffic. Each surviving root is numbered independently from 1.
func ReconstructTurns(calls []Call) []ReconTurn {
	if len(calls) == 0 {
		return nil
	}
	ordered := make([]Call, len(calls))
	copy(ordered, calls)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })

	// Pass 1: count calls per root.
	roots := make(map[string]int)
	rootOf := make([]string, len(ordered))
	for i, c := range ordered {
		r := conversationAnchor(c.ToolDefs)
		rootOf[i] = r
		if r != "" {
			roots[r]++
		}
	}
	threshold := int(RootShare * float64(len(ordered)))
	if threshold < MinRootCalls {
		threshold = MinRootCalls
	}
	keep := make(map[string]bool, len(roots))
	for r, n := range roots {
		if n >= threshold {
			keep[r] = true
		}
	}
	// A session whose every root is below the line (very short sessions) keeps the largest,
	// so a 2-call session still yields its turn.
	if len(keep) == 0 {
		best, bestN := "", 0
		for r, n := range roots {
			if n > bestN {
				best, bestN = r, n
			}
		}
		if best != "" {
			keep[best] = true
		}
	}

	dispatched := dispatchedPrompts(ordered)

	// Pass 2: collect turns in call order, de-duplicated per root.
	firstCall := make(map[string]bool)
	emitted := make(map[string]bool) // turn text, across all roots
	seen := make(map[string]bool)
	perRoot := make(map[string]int)
	var out []ReconTurn
	for i, c := range ordered {
		r := rootOf[i]
		if r == "" || !keep[r] {
			continue
		}
		// In the root's first observed call, everything before the last assistant message is
		// history. A fresh conversation's first call has no assistant message, so nothing is.
		historyEnd := -1
		if !firstCall[r] {
			firstCall[r] = true
			for j, m := range c.Messages {
				if m.Role == "assistant" {
					historyEnd = j
				}
			}
		}
		for j, m := range c.Messages {
			if m.Role != "user" {
				continue
			}
			for _, p := range m.Parts {
				if p.Type != PartText {
					continue
				}
				h := HumanText(p.Content)
				if h == "" || dispatched[h] {
					continue
				}
				key := r + "\x00" + h
				if seen[key] {
					continue
				}
				seen[key] = true
				// History that another root already produced is the SAME conversation seen
				// again, not a new turn: a mid-session tool-set change starts a new anchor, a
				// forked subagent carries its parent's history, and either one's first call
				// replays everything so far. Measured on 3 of 73 sessions, where those turns
				// were counted twice.
				prior := j < historyEnd
				if prior && emitted[h] {
					continue
				}
				emitted[h] = true
				perRoot[r]++
				out = append(out, ReconTurn{
					Sequence:  perRoot[r],
					Text:      h,
					FirstSeen: c.Order,
					Root:      r,
					Prior:     prior,
				})
			}
		}
	}
	return out
}
