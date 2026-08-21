package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// Tool-pairing repair for a component that REMOVES messages.
//
// Anthropic (and every provider with the same shape) requires each `tool_result` to answer a
// `tool_use` that appeared earlier. summarize replaces a span of the transcript with one
// summary message — [msgs[0], summary, msgs[end:]...] — and the kept tail can begin part-way
// through a tool exchange, so its leading `tool_result` blocks refer to `tool_use` blocks that
// were just deleted. The provider then rejects the entire request:
//
//	400 messages.0.content.2: unexpected `tool_use_id` found in `tool_result` blocks
//
// Measured on live LOCA-bench traffic: 5 of 12 tasks failed this way once the earlier
// system-role defect was fixed and summarize could finally act.
//
// This is an invariant for any component that deletes messages, and the reason coref does not
// need it: coref rewrites a tool message's text IN PLACE and never removes a message, so
// pairing is preserved by construction. summarize removes, so summarize must repair.
//
// The repair is deliberately one-directional — DROP orphaned results, never synthesise
// placeholder ones. A synthetic "[tool result unavailable]" would be a second lie on top of
// the summary: the summary already claims to carry that content forward, so re-asserting a
// missing result invites the model to reason about an absence that the summary is supposed to
// have described. The rig-side shim used for LOCA's own trimmer synthesises because it must
// preserve a foreign agent's history; a component summarising its own span does not.
func dropOrphanedToolResults(msgs []bschemas.ChatMessage) ([]bschemas.ChatMessage, int) {
	// Every tool_use id available to answer, accumulated as we walk forward. A result may
	// answer any earlier call, not only the immediately preceding message, because a summary
	// may sit between the call and its result.
	seen := map[string]struct{}{}
	out := make([]bschemas.ChatMessage, 0, len(msgs))
	dropped := 0
	for _, m := range msgs {
		if m.ChatAssistantMessage != nil {
			for _, tc := range m.ChatAssistantMessage.ToolCalls {
				if tc.ID != nil {
					seen[*tc.ID] = struct{}{}
				}
			}
		}
		if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil {
			if _, ok := seen[*m.ChatToolMessage.ToolCallID]; !ok {
				dropped++
				continue // orphaned: its call is gone
			}
		}
		out = append(out, m)
	}
	return out, dropped
}
