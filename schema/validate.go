package schema

import (
	"fmt"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// Static validation of a message list against the provider's message-SHAPE rules — the
// rules that make a request well-formed regardless of its content.
//
// WHY THIS EXISTS. Three separate shape violations shipped in `summarize` and were caught
// only by sending live traffic to a real API, one after another, each masked by the one
// before it:
//
//	400 messages.1: role 'system' must precede an 'assistant' message or end the array
//	400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//
// None was findable by the project's existing methods. No test asserted them, and every
// offline measurement replayed through the `/compact` endpoint, which runs the pipeline and
// returns the rewritten body WITHOUT forwarding it upstream — so no provider ever validated
// it. Replay can tell you what a component removed; it is structurally incapable of telling
// you whether the result is a sendable request.
//
// All three are checkable statically, with no provider and no model. That is what this is
// for: a component that mutates a message list can be asserted well-formed in a unit test,
// closing the blind spot without paying for live traffic on every change.
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

// ValidateShape reports every message-shape invariant the list breaks. An empty result
// means the list is well-formed in the ways a provider enforces structurally.
//
// The invariants, and why each one exists:
//
//  1. system-position — a system-role message may appear only at index 0. Providers expect
//     system content in a dedicated top-level field; one spliced mid-array is rejected.
//     This is the defect that made `summarize` unusable on every single call.
//  2. answered-tool-use — every tool call must be answered by a result in the NEXT message.
//     Removing a span can delete the answer while keeping the call.
//  3. paired-tool-result — every result must answer a call that appeared earlier. Removing a
//     span can delete the call while keeping the answer. The mirror of (2), and the reason
//     both are checked: fixing one alone leaves the other live, which is exactly what
//     happened here.
//
// (2) requires the answer in the immediately following message, matching the provider's
// wording. (3) permits a call at any earlier position, because a legitimately inserted
// summary may sit between a call and its result.
func ValidateShape(msgs []schemas.ChatMessage) []ShapeViolation {
	var out []ShapeViolation
	seenCall := map[string]bool{}

	for i := range msgs {
		m := msgs[i]

		if m.Role == schemas.ChatMessageRoleSystem && i != 0 {
			out = append(out, ShapeViolation{Index: i, Rule: "system-position",
				Msg: "role 'system' must precede an 'assistant' message or end the array"})
		}

		// A result must answer a call seen earlier (possibly at a distance).
		if m.Role == schemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil {
			if id := *m.ChatToolMessage.ToolCallID; !seenCall[id] {
				out = append(out, ShapeViolation{Index: i, Rule: "paired-tool-result",
					Msg: fmt.Sprintf("unexpected `tool_use_id` found in `tool_result` blocks: %s", id)})
			}
		}

		if m.ChatAssistantMessage == nil || len(m.ChatAssistantMessage.ToolCalls) == 0 {
			continue
		}
		var ids []string
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			if tc.ID != nil {
				seenCall[*tc.ID] = true
				ids = append(ids, *tc.ID)
			}
		}
		// Every call must be answered by the very next message.
		answered := map[string]bool{}
		if i+1 < len(msgs) {
			n := msgs[i+1]
			if n.Role == schemas.ChatMessageRoleTool && n.ChatToolMessage != nil &&
				n.ChatToolMessage.ToolCallID != nil {
				answered[*n.ChatToolMessage.ToolCallID] = true
			}
		}
		for _, id := range ids {
			if !answered[id] {
				out = append(out, ShapeViolation{Index: i, Rule: "answered-tool-use",
					Msg: fmt.Sprintf("`tool_use` ids were found without `tool_result` blocks "+
						"immediately after: %s", id)})
			}
		}
	}
	return out
}
