package schema

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Static validation of a message list against the provider's message-SHAPE rules — the
// rules that make a request well-formed regardless of its content.
//
// WHY THIS EXISTS. Four separate shape violations shipped in `summarize` and every one of
// them was found REACTIVELY, by a live provider rejection or a benchmark failure, each
// masked by the one before it:
//
//	2edb9d4  400 messages.1: role 'system' must precede an 'assistant' message or end the array
//	fb5c460  400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	e7d1aa8  400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//	e9bf3a7  panic: index out of range [-1]   (a short transcript; see NOT COVERED below)
//
// None was findable by the project's existing methods. No test asserted the shape of what a
// component emitted, and every offline measurement replayed through the `/compact` endpoint,
// which runs the pipeline and returns the rewritten body WITHOUT forwarding it upstream — so
// no provider ever validated it. Replay can tell you what a component removed; it is
// structurally incapable of telling you whether the result is a sendable request.
//
// The first three are checkable statically, with no provider and no model, because they are
// properties of the message list alone. That is what this is for: a component that mutates a
// message list can be asserted well-formed in a unit test, closing the blind spot without
// paying for live traffic on every change.
//
// WHERE IT RUNS. Unit tests, over the pipeline's NORMALIZED view of a transcript — the shape
// apply.normalize produces and components mutate, where an Anthropic `tool_use` block has
// been lifted into bifrost's ToolCalls and each `tool_result` block is its own synthetic
// role=tool message. It is deliberately NOT on the request hot path: it walks the whole
// transcript and allocates per tool exchange, which is not free enough to spend on every
// request, and a validator that can only fail open adds latency without adding a decision.
//
// NOT COVERED, and worth being explicit about: e9bf3a7 was a PANIC inside boundary
// arithmetic, not a malformed list. No check on the output list can see it — by the time
// there is a list to inspect, the panic has already happened (and pipeline.runOne swallowed
// it into verdict=reverted). Shape validation is not a substitute for exercising a component
// on transcripts shorter than its own thresholds.
//
// DELIBERATELY NOT A REQUEST VALIDATOR. It checks shape invariants that hold across
// providers with an Anthropic-style tool protocol; it does not check token limits, model
// names, sampling parameters, or anything content-dependent.

// ShapeViolation is one broken invariant, phrased to point at the message that broke it.
type ShapeViolation struct {
	Index int    // message index, or -1 when the violation is about the list as a whole
	Rule  string // short, stable identifier for the invariant
	Msg   string // human-readable, mirroring the provider's own wording where possible
}

func (v ShapeViolation) String() string {
	if v.Index < 0 {
		return fmt.Sprintf("[%s] %s", v.Rule, v.Msg)
	}
	return fmt.Sprintf("messages.%d [%s] %s", v.Index, v.Rule, v.Msg)
}

// Shape rule identifiers, so a test can assert on the specific invariant it exists for
// rather than on wording.
const (
	RuleSystemPosition   = "system-position"
	RuleAnsweredToolUse  = "answered-tool-use"
	RulePairedToolResult = "paired-tool-result"
)

// ValidateShape reports every message-shape invariant the list breaks. An empty result
// means the list is well-formed in the ways a provider enforces structurally.
//
// The invariants, and why each one exists:
//
//  1. system-position — a system-role message away from index 0 must be immediately
//     followed by an assistant message, or end the array. This is the provider's own
//     wording, and it is the defect that made `summarize` unusable on every call: it
//     emitted [msgs[0], summary(system), tail...], so with the usual system prompt at
//     index 0 a second system role landed at index 1 in front of the kept tail.
//  2. answered-tool-use — every `tool_use` must be answered by a `tool_result` in the
//     messages immediately following it. Removing a span can delete the answer while
//     keeping the call: preserving msgs[0] when it is an assistant tool-call message does
//     exactly that.
//  3. paired-tool-result — every `tool_result` must answer a `tool_use` that appeared
//     earlier. Removing a span can delete the call while keeping the answer. The mirror of
//     (2), and the reason both are checked: e7d1aa8 showed they are one mistake seen from
//     either side, and fixing one alone leaves the other live — which is what happened.
func ValidateShape(msgs []schemas.ChatMessage) []ShapeViolation {
	var out []ShapeViolation
	seenCall := map[string]bool{}

	for i := range msgs {
		m := msgs[i]

		if m.Role == schemas.ChatMessageRoleSystem && i != 0 && !systemPositionOK(msgs, i) {
			out = append(out, ShapeViolation{Index: i, Rule: RuleSystemPosition,
				Msg: "role 'system' must precede an 'assistant' message or end the array"})
		}

		// A result must answer a call seen earlier.
		if m.Role == schemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil && *m.ChatToolMessage.ToolCallID != "" {
			if id := *m.ChatToolMessage.ToolCallID; !seenCall[id] {
				out = append(out, ShapeViolation{Index: i, Rule: RulePairedToolResult,
					Msg: fmt.Sprintf("unexpected `tool_use_id` found in `tool_result` blocks: %s", id)})
			}
		}

		if m.ChatAssistantMessage == nil || len(m.ChatAssistantMessage.ToolCalls) == 0 {
			continue
		}
		var ids []string
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			if tc.ID != nil && *tc.ID != "" {
				seenCall[*tc.ID] = true
				ids = append(ids, *tc.ID)
			}
		}
		answered := answeredInRun(msgs, i+1)
		for _, id := range ids {
			if !answered[id] {
				out = append(out, ShapeViolation{Index: i, Rule: RuleAnsweredToolUse,
					Msg: fmt.Sprintf("`tool_use` ids were found without `tool_result` blocks "+
						"immediately after: %s", id)})
			}
		}
	}
	return out
}

// systemPositionOK applies the provider's rule for a system role inside `messages`, which is
// narrower than "index 0 only" in one direction and wider in another.
//
// "Index 0 only" is what the first version of this check said, and it is WRONG for this
// codebase: the Claude Agent SDK appends a fresh system-role message inside `messages` on
// every turn (its `<system-reminder><total_tokens>N tokens left</total_tokens>` budget
// reminder), and that traffic is ACCEPTED by the provider. apply's captured Agent-SDK
// fixture carries system roles at indices 1, 4 and 7 of a five- and eight-message
// transcript — see schema.SessionHead, which exists because of those same messages. A
// validator that rejected them would fire on ordinary live traffic and be worthless
// precisely where it is needed, the same trap as checking only msgs[i+1] for a parallel
// call's results.
//
// So the rule is the provider's literal one: such a message must be followed by an assistant
// turn (the SDK's reminder always is, or it ends the array). The summarize defect fails it
// because the summary was spliced in FRONT of the kept tail, which begins with whatever the
// conversation was doing — a user turn or a tool exchange, not an assistant reply.
func systemPositionOK(msgs []schemas.ChatMessage, i int) bool {
	if i == len(msgs)-1 {
		return true // ends the array
	}
	return msgs[i+1].Role == schemas.ChatMessageRoleAssistant
}

// answeredInRun collects the tool_result ids in the contiguous run of tool messages starting
// at `from`. Any other role ends the run.
//
// PARALLEL CALLS are why this scans a run rather than one message. One assistant message may
// carry several tool_use blocks, and Anthropic requires every result in the SINGLE user
// message that follows. bifrost's schema represents each result as its OWN role=tool message,
// so the wire's one user message maps to a RUN of consecutive tool messages here (see
// apply.normalize). Inspecting only msgs[i+1] therefore reported a violation on every
// ordinary parallel exchange — the second call always looked unanswered:
//
//	messages.1 [answered-tool-use] ... without `tool_result` blocks immediately after: call_b
//
// which is indistinguishable from the real defect being hunted. Scanning the whole run is the
// representation-correct reading of the same invariant: the run must still be CONTIGUOUS, so
// anything else in between (a summary, a user turn) still fails.
func answeredInRun(msgs []schemas.ChatMessage, from int) map[string]bool {
	answered := map[string]bool{}
	for j := from; j < len(msgs); j++ {
		if msgs[j].Role != schemas.ChatMessageRoleTool {
			break
		}
		if t := msgs[j].ChatToolMessage; t != nil && t.ToolCallID != nil {
			answered[*t.ToolCallID] = true
		}
	}
	return answered
}

// FormatShapeViolations renders violations one per line for a test failure message, with the
// list's roles appended — the roles are what makes a shape failure diagnosable.
func FormatShapeViolations(vs []ShapeViolation, msgs []schemas.ChatMessage) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.String())
		b.WriteString("\n")
	}
	b.WriteString("roles: ")
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d:%s", i, m.Role)
	}
	return b.String()
}
