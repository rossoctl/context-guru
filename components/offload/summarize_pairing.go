package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// wireGroupsStale reports whether c.WireGroup existed for this request (this provider/traffic
// shape DOES produce wire groups — Anthropic normalized at least one shared message) but no
// longer lines up with msgs: an earlier component changed the message count, so the groups'
// current positions are unknown. summarizeSpan's own defensive check collapses this the same
// way as a true nil WireGroup (no wire groups ever existed — e.g. OpenAI-shaped traffic, or a
// Ctx built directly as most unit tests do) and computes an UNALIGNED boundary, which can split
// a wire message that is really there, just not where WireGroup says.
//
// A caller (summarize.Offload, cache_aware_summarizer.Offload) checks this BEFORE calling
// summarizeSpan and declines rather than guesses — see #457's split-boundary requirement.
func wireGroupsStale(c *components.Ctx, msgs []bschemas.ChatMessage) bool {
	return c.WireGroup != nil && len(c.WireGroup) != len(msgs)
}

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
// keepFirst is how many leading messages stay verbatim before the span. It exists because
// "preserve msgs[0]" means different things in the two dialects, and only one of them keeps
// the task: on Anthropic traffic the system prompt is a top-level field the pipeline never
// sees, so msgs[0] IS the opening user turn — but on OpenAI-shaped traffic msgs[0] is the
// system prompt and the task statement sits at msgs[1], inside the span. A deployment on
// OpenAI-shaped traffic therefore needs keep_first: 2 to retain what it is working on.
//
// groups is #457's wire-message grouping: groups[i] names the ORIGINAL wire message
// normalized message i was extracted from, so several normalized messages (an Anthropic
// tool_result's synthetic tool message and the sibling text block beside it) can share one
// group. Nil (the OpenAI-shaped and legacy-caller case, where every normalized message already
// IS its own wire message) disables the check below entirely — see components.Ctx.WireGroup.
//
// Returns headCount (0..keepFirst), start, end. A caller that gets end <= start should skip.
func summarizeSpan(msgs []bschemas.ChatMessage, keepFirst, keepLast int, groups []int) (headCount, start, end int) {
	if len(groups) != len(msgs) {
		groups = nil // mismatched coordinates (an earlier component changed the count) — disable
	}
	// sameGroup reports whether msgs[a] and msgs[b] came from the SAME wire message, so a and b
	// must never end up on opposite sides of a boundary — see the headCount loop below. False
	// whenever groups is disabled or either index is out of its bounds (defensive; a and b are
	// always adjacent indices here, but a boundary at 0 or len(msgs) must not panic).
	sameGroup := func(a, b int) bool {
		return groups != nil && a >= 0 && b < len(groups) && groups[a] == groups[b]
	}
	headCount = keepFirst
	if headCount < 0 {
		headCount = 0
	}
	if headCount > len(msgs) {
		headCount = len(msgs)
	}
	// THE HEAD MUST BE SELF-CONSISTENT. An assistant message that requested tool calls has its
	// results in the message(s) after it, so a head ENDING on one keeps a call whose results were
	// summarized away — the second of the two provider rejections above. Retreat until it does
	// not. A loop rather than one decrement: consecutive tool-calling assistant messages are
	// malformed input, and malformed input is exactly where a 400 comes from.
	//
	// At keepFirst: 1 this reduces to the original rule (msgs[0] is a tool-calling assistant ⇒
	// headCount 0), and at keepFirst: 2 a call/result pair at msgs[0:2] is kept WHOLE, because
	// msgs[1] is the result and carries no calls of its own.
	//
	// The sameGroup clause (#457) retreats the SAME direction for the same reason: a head ending
	// between a tool_result and its sibling text block would summarize one half of one wire
	// message and keep the other half verbatim, which the count-changing rebuild cannot represent
	// (there is no single body message that is "half rewritten").
	for headCount > 0 && (requestedToolCalls(msgs[headCount-1]) || sameGroup(headCount-1, headCount)) {
		headCount--
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
	// Advance past a tail that would begin mid-exchange, mid wire-message, or on a wire message
	// whose group hides an orphaned tool_result behind a leading sibling — see
	// advanceSpanBoundary, same reasoning as the headCount retreat above, same direction:
	// advancing only ever keeps MORE context verbatim, never less.
	end = advanceSpanBoundary(msgs, groups, end)
	return headCount, start, end
}

// retreatPastLastToolExchange moves end back so the LAST tool exchange in [start, len(msgs)) stays
// verbatim after the span: end becomes the index of the assistant message that called the tool
// whose results are the last run of role=tool messages. Returns end unchanged when there is no tool
// result at or after start, or when the exchange starts at or before start (nothing left to
// summarize). cache_aware_summarizer only (#723): a splice that keeps a tool exchange gives the next
// turn a cache entry it can read, once the host marks that exchange's tool_result.
//
// The exchange is found from its results — the run of role=tool messages, and the assistant
// message just before them — rather than from the assistant's ToolCalls, so it does not depend on
// how a caller populated those. groups keeps the cut on a wire-message edge, as summarizeSpan's own
// boundary does.
func retreatPastLastToolExchange(msgs []bschemas.ChatMessage, groups []int, start, end int) int {
	if len(groups) != len(msgs) {
		groups = nil
	}
	last := -1
	for i := len(msgs) - 1; i >= start; i-- {
		if msgs[i].Role == bschemas.ChatMessageRoleTool {
			last = i
			break
		}
	}
	if last < 0 {
		return end
	}
	first := last
	for first-1 >= start && (msgs[first-1].Role == bschemas.ChatMessageRoleTool ||
		(groups != nil && groups[first-1] == groups[first])) {
		first--
	}
	call := first - 1 // the assistant turn that asked for the tool
	if call <= start || msgs[call].Role != bschemas.ChatMessageRoleAssistant {
		return end
	}
	for call > start && groups != nil && groups[call-1] == groups[call] {
		call--
	}
	if call <= start || call >= end {
		return end
	}
	return call
}

// lastKeptToolResultID is the tool_call_id of the last role=tool message in msgs[from:], or "".
func lastKeptToolResultID(msgs []bschemas.ChatMessage, from int) string {
	for i := len(msgs) - 1; i >= from && i >= 0; i-- {
		m := msgs[i]
		if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
			return *m.ChatToolMessage.ToolCallID
		}
	}
	return ""
}

// advanceSpanBoundary is THE rule for "is it safe for a kept tail (or a replayed checkpoint's
// tail, see emitCheckpoint) to begin at `at`", so there is exactly one implementation rather
// than two that can drift. Advances `at` while it would:
//
//   - begin on a role=tool message directly (orphans its tool_use, which is now one message
//     earlier, outside whatever is being kept from `at` onward);
//   - split a wire group in half (`at-1` and `at` share one wire message — #457);
//   - begin on a group whose FIRST member is a sibling text block, with its own tool_result
//     one or more slots later in the SAME group (#457 round 2: `msgs[at].Role == tool` alone
//     misses this, because `at` itself is role=user).
//
// groups is components.Ctx.WireGroup, or nil to disable the group-aware clauses entirely (a
// length mismatch — an earlier count-changing component ran — is treated the same as nil).
func advanceSpanBoundary(msgs []bschemas.ChatMessage, groups []int, at int) int {
	if len(groups) != len(msgs) {
		groups = nil
	}
	sameGroup := func(a, b int) bool {
		return groups != nil && a >= 0 && b < len(groups) && groups[a] == groups[b]
	}
	// groupHasToolRole reports whether the wire group STARTING at i contains a role=tool
	// member anywhere in it, not just at i itself. apply.normalize emits a group's members in
	// BODY block order, so a sibling text block that precedes its tool_result puts a role=user
	// message at the group's own start — this is what lets the boundary land there undetected
	// by a plain `msgs[i].Role == Tool` check.
	groupHasToolRole := func(i int) bool {
		if groups == nil || i < 0 || i >= len(msgs) {
			return false
		}
		g := groups[i]
		for k := i; k < len(msgs) && groups[k] == g; k++ {
			if msgs[k].Role == bschemas.ChatMessageRoleTool {
				return true
			}
		}
		return false
	}
	for at < len(msgs) && (msgs[at].Role == bschemas.ChatMessageRoleTool ||
		sameGroup(at-1, at) || groupHasToolRole(at)) {
		at++
	}
	return at
}

// requestedToolCalls reports whether m is an assistant message that asked for tool calls, as
// bifrost models them.
//
// bifrost's own unmarshal does not map Anthropic `tool_use` content blocks onto ToolCalls, but
// apply's normalize lifts them there (attachToolUse), so on proxy traffic this is true for an
// Anthropic tool-calling turn too. On a Ctx built some other way it can be false for one; the
// head-retreat above is still correct then, because apply's normalize turns each Anthropic
// tool_result block into a synthetic role=tool message and the ORIGINAL msgs[0] on that path is the
// opening user turn, never a tool-calling assistant message.
func requestedToolCalls(m bschemas.ChatMessage) bool {
	return m.Role == bschemas.ChatMessageRoleAssistant &&
		m.ChatAssistantMessage != nil && len(m.ChatAssistantMessage.ToolCalls) > 0
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
