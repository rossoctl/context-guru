package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
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

// summarizeSpan picks the span to summarize so that it NEVER cuts inside a tool exchange,
// and reports how many head messages to preserve.
//
// The naive boundaries — preserve msgs[0], summarize msgs[1 : len-keepLast] — are pure
// arithmetic and know nothing about tool pairing, which produced two separate provider
// rejections on live traffic:
//
//	400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	    — the kept tail began with a tool_result whose tool_use was inside the span
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//	    — msgs[0] was an assistant tool_use whose result was inside the span
//
// Both are the same mistake seen from either side, so both are fixed by one rule: **a tool
// exchange is atomic**. Review put it best — an unanswered call means the agent is still
// waiting on that tool, so summarize after it completes, not through it.
//
// Two adjustments implement that:
//
//   - END is advanced forward past any tool messages the kept tail would begin with, so the
//     exchange is summarized WHOLE rather than split. Advancing (rather than retreating)
//     keeps the call and its result on the same side of the boundary without ever keeping
//     LESS context than asked for.
//   - The HEAD is dropped when msgs[0] is an assistant message carrying tool calls, because
//     its results necessarily lie inside the span. msgs[0] is preserved to retain the
//     conversation's identity — its system prompt or opening user turn — and an assistant
//     tool-call message is neither, so nothing is lost by folding it into the summary.
//
// Returns headCount (0 or 1), start, end. A caller that gets end <= start should skip.
func summarizeSpan(msgs []bschemas.ChatMessage, keepLast int) (headCount, start, end int) {
	headCount = 1
	if len(msgs) > 0 && msgs[0].Role == bschemas.ChatMessageRoleAssistant &&
		msgs[0].ChatAssistantMessage != nil && len(msgs[0].ChatAssistantMessage.ToolCalls) > 0 {
		headCount = 0 // preserving it would leave its calls unanswered
	}
	start = headCount
	end = len(msgs) - keepLast
	if end > len(msgs) {
		end = len(msgs)
	}
	// CLAMP BEFORE INDEXING. A transcript shorter than keep_last makes `end` NEGATIVE, and the
	// loop below indexes msgs[end] — `end < len(msgs)` is trivially true for a negative index, so
	// it reads msgs[-1] and panics. With the default keep_last: 3 that is any request with fewer
	// than three messages, i.e. the first turn or two of EVERY session, not an edge case.
	//
	// The old boundary was pure arithmetic and never indexed anything, so Offload's `end <= start`
	// check caught this case cleanly; adding the tool-boundary walk moved an index read in front of
	// that guard. Clamping to `start` restores the short-circuit: Offload sees end <= start and
	// declines, exactly as before.
	//
	// It was survivable rather than visible because pipeline.runOne recovers per component, so the
	// panic surfaced only as verdict=reverted in the logs while summarize silently did nothing on
	// short turns. Found in review, on live sessions.
	if end < start {
		end = start
	}
	// Advance past a tail that would begin mid-exchange.
	for end < len(msgs) && msgs[end].Role == bschemas.ChatMessageRoleTool {
		end++
	}
	return headCount, start, end
}

// trimSpanForKeptVerbatim lowers end so the span never contains content the agent EXPANDED.
//
// summarize is the one offloader that consults neither skipReduce nor isKeptVerbatim, and it does
// not edit messages in place — it replaces msgs[start:end] with a single summary. So expanded
// content inside the span was summarized away, which is two separate problems:
//
//   - It is the BOUNCE LOOP cg:keep: exists to prevent, which is why the other ten offloaders
//     consult it: the agent asks for content back, gets it, and the next turn it is gone again.
//     Pre-existing, and the reason this fix belongs here rather than at the caller.
//   - It falsifies expand.RestoredInPlace's pointer. The repair says "the content is present in
//     the transcript above" on the strength of the body as it stands BEFORE the pipeline runs; if
//     summarize then removes it, the model is left with a pointer to nothing where it used to get
//     the content.
//
// THE RETREAT MIRRORS THE ADVANCE ABOVE, and for the same reason: a tool exchange is atomic. Simply
// setting end to the kept-verbatim message's index is not enough, because that message is typically
// a tool output — so the kept tail would begin with a tool_result whose tool_use is still inside the
// span, which is one of the two provider rejections summarizeSpan was written to stop:
//
//	400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//
// So end retreats past the whole exchange. Advancing instead (as the tail-alignment rule does) is
// not available here: advancing past a tool message would swallow the very message being protected.
//
// A caller that gets end <= start skips, which is an outcome summarize already supports — the safe
// direction, and better than either summarizing expanded content away or emitting a request the
// provider rejects.
// The scan stops at the FIRST kept-verbatim message, since everything after it is protected by the
// retreat anyway — so the common case (nothing expanded) is one store lookup per span message and no
// allocation, and the acting case is fewer. The caller runs this below its trigger gate so a turn
// that was never going to summarize pays none of it.
func trimSpanForKeptVerbatim(msgs []bschemas.ChatMessage, start, end int, kept func(string) bool) int {
	first := -1
	for i := start; i < end; i++ {
		if kept(schema.MessageText(msgs[i])) {
			first = i
			break
		}
	}
	if first < 0 {
		return end
	}
	end = first
	// Retreat off any boundary that would split an exchange: a tail beginning with a tool message,
	// or a span ending on an assistant turn whose results now sit in the tail.
	for end > start {
		if end < len(msgs) && msgs[end].Role == bschemas.ChatMessageRoleTool {
			end--
			continue
		}
		prev := msgs[end-1]
		if prev.Role == bschemas.ChatMessageRoleAssistant &&
			prev.ChatAssistantMessage != nil && len(prev.ChatAssistantMessage.ToolCalls) > 0 {
			end--
			continue
		}
		break
	}
	return end
}
