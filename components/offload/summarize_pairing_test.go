package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// TestSummarizeSpanNeverCutsAWireGroup is #457's split-boundary requirement: on Anthropic
// traffic, apply.normalize can turn ONE wire message into several normalized messages (a
// tool_result's synthetic tool message plus a sibling text block beside it). Those share one
// entry in components.Ctx.WireGroup, and summarizeSpan must never choose a boundary that falls
// BETWEEN them — the count-changing rebuild has no way to keep half of one wire message
// verbatim while summarizing the other half.
func TestSummarizeSpanNeverCutsAWireGroup(t *testing.T) {
	// msgs[2] (sibling text) and msgs[3] (tool_result) came from the SAME wire message —
	// group 2 — exactly like msgs[4]/msgs[5] came from group 3. Every other message is its own
	// group (an ordinary whole-message slot).
	msgs := []bschemas.ChatMessage{
		sysMsg("system"),      // group 0
		userMsg("go"),         // group 1
		userMsg("sibling 1"),  // group 2 (sibling text)
		userMsg("tool out 1"), // group 2 (tool_result)
		userMsg("sibling 2"),  // group 3 (sibling text)
		userMsg("tool out 2"), // group 3 (tool_result)
		userMsg("final"),      // group 4
	}
	groups := []int{0, 1, 2, 2, 3, 3, 4}

	// keepLast=1 and keepLast=3 land on a boundary that is already clean. keepLast=2 and
	// keepLast=4 land naively INSIDE group 3 and group 2 respectively — the boundary must
	// advance to that group's end.
	for _, tc := range []struct {
		keepLast int
		wantEnd  int
	}{
		{keepLast: 1, wantEnd: 6}, // naive end=6: already clean (group 4 starts there)
		{keepLast: 2, wantEnd: 6}, // naive end=5 is mid-group-3 -> advances to 6
		{keepLast: 3, wantEnd: 4}, // naive end=4: already clean (group 3 starts there)
		{keepLast: 4, wantEnd: 4}, // naive end=3 is mid-group-2 -> advances to 4
	} {
		_, start, end := summarizeSpan(msgs, 1, tc.keepLast, groups)
		if end > 0 && end < len(msgs) && groups[end-1] == groups[end] {
			t.Errorf("keepLast=%d: end=%d still splits group %d", tc.keepLast, end, groups[end])
		}
		if end != tc.wantEnd {
			t.Errorf("keepLast=%d: end=%d, want %d", tc.keepLast, end, tc.wantEnd)
		}
		if start != 1 {
			t.Errorf("keepLast=%d: start=%d, want 1 (headCount unaffected by a tail-side split)", tc.keepLast, start)
		}
	}

	// Mirror case: a head-side split. keepFirst=3 would naively keep msgs[0:3] verbatim, which
	// cuts group 2 between msgs[2] and msgs[3] — headCount must retreat to the group's start (2).
	headCount, start, _ := summarizeSpan(msgs, 3, 1, groups)
	if headCount > 0 && headCount < len(msgs) && groups[headCount-1] == groups[headCount] {
		t.Errorf("headCount=%d still splits group %d", headCount, groups[headCount])
	}
	if headCount != 2 {
		t.Errorf("headCount=%d, want 2 (retreated off the group-2 split)", headCount)
	}
	if start != headCount {
		t.Errorf("start=%d must equal headCount=%d", start, headCount)
	}
}

// TestSummarizeSpanDoesNotOrphanAToolResultBehindASibling is #457 round 2: a group whose
// members are [sibling text, tool_result] (sibling first, matching Claude Code's own block
// order) must not let the tail start exactly on the sibling while the tool_result's tool_use
// stays behind in the summarized span — the plain "does the message AT end have role=tool"
// check only sees the sibling, not the orphan one slot later.
func TestSummarizeSpanDoesNotOrphanAToolResultBehindASibling(t *testing.T) {
	id := "call_a"
	asst := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{
			ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id}}}}
	tool := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
	schema.SetMessageText(&tool, "tool output")

	msgs := []bschemas.ChatMessage{
		userMsg("task"),         // group 0
		userMsg("small talk 1"), // group 1
		userMsg("small talk 2"), // group 2
		userMsg("small talk 3"), // group 3
		asst,                    // group 4 (the tool_use)
		userMsg("sibling text"), // group 5 (sibling, FIRST in body order)
		tool,                    // group 5 (the paired tool_result, same group)
		userMsg("final"),        // group 6
	}
	groups := []int{0, 1, 2, 3, 4, 5, 5, 6}

	// Naive end = len(msgs)-keepLast = 8-3 = 5, exactly the sibling's index: role=user there,
	// so the plain check sees nothing wrong, yet msgs[4] (the tool_use) would be summarized
	// away while msgs[6] (its result) survives in the tail two slots later.
	_, _, end := summarizeSpan(msgs, 1, 3, groups)
	if end <= 4 {
		t.Fatalf("end=%d leaves the tool_use (index 4) in the summarized span while its "+
			"result (index 6) survives in the tail — orphaned tool_result", end)
	}
	if end > 0 && end < len(msgs) && groups[end-1] == groups[end] {
		t.Errorf("end=%d still splits group %d", end, groups[end])
	}
}

// TestSummarizeSpanGroupsIgnoredWhenMismatched: a nil or length-mismatched groups slice (an
// earlier count-changing component already ran, or a Ctx built directly as most unit tests do)
// disables the check entirely rather than indexing out of range or guessing.
func TestSummarizeSpanGroupsIgnoredWhenMismatched(t *testing.T) {
	msgs := []bschemas.ChatMessage{sysMsg("s"), userMsg("a"), userMsg("b"), userMsg("c")}
	if _, _, end := summarizeSpan(msgs, 1, 1, nil); end != 3 {
		t.Errorf("nil groups: end=%d, want 3 (unaffected)", end)
	}
	mismatched := []int{0, 1} // shorter than msgs
	if _, _, end := summarizeSpan(msgs, 1, 1, mismatched); end != 3 {
		t.Errorf("mismatched groups: end=%d, want 3 (disabled, not out-of-range)", end)
	}
}

// TestEmitCheckpointDoesNotOrphanAToolResultBehindASibling is #457 round 2's second site:
// emitCheckpoint (the REPLAY path — tryReuse and replayStale both call it) had its own copy of
// just the role==tool half of the boundary-safety rule, so it had the exact gap
// summarizeSpan's fresh path had before this PR: a replayed checkpoint's boundary can land
// exactly on a sibling text block whose tool_result, one slot later in the SAME wire group,
// pairs with a tool_use that the checkpoint's span is about to summarize away.
//
// This is a DIFFERENT call path from TestSummarizeRebuildWithASplitGroup's "kept tail" case in
// apply/normalizer_siblings_test.go: there, the boundary reaching emitCheckpoint was already
// clean because it came straight from a FRESH summarizeSpan call. Here, boundary is given
// directly (as tryReuse/replayStale actually do: `start + cp.CoveredCount`, independent of any
// fresh span computation), so it is the only test that can catch emitCheckpoint failing to
// apply the rule itself.
func TestEmitCheckpointDoesNotOrphanAToolResultBehindASibling(t *testing.T) {
	id := "call_a"
	asst := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{
			ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id}}}}
	tool := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
	schema.SetMessageText(&tool, "tool output")

	msgs := []bschemas.ChatMessage{
		userMsg("task"),         // group 0 — head
		asst,                    // group 1 — the tool_use
		userMsg("sibling text"), // group 2 — sibling, FIRST in body order
		tool,                    // group 2 — the paired tool_result, same group
		userMsg("final"),        // group 3 — tail
	}
	groups := []int{0, 1, 2, 2, 3}

	s := &Summarize{}
	cp := sumCheckpoint{SummaryMsg: "<summary>covers the task</summary>", CoveredCount: 1}
	// boundary = headCount(1) + CoveredCount(1) = 2, exactly the sibling's index: role=user
	// there, so the plain `msgs[boundary].Role == Tool` check this replaced saw nothing wrong,
	// while msgs[1] (the tool_use) is about to be dropped and msgs[3] (its result) would have
	// survived in the tail two slots later.
	out, _ := s.emitCheckpoint(cp, msgs, 1, 2, groups)

	for _, m := range out {
		if m.Role == bschemas.ChatMessageRoleTool {
			t.Fatalf("a role=tool message leaked into the replayed output (orphaned tool_result "+
				"not caught): %+v", out)
		}
	}
	// Correct behaviour: the whole group (sibling + tool_result) and its tool_use are pulled
	// into the summarized half along with the head, and the tail is just the final message —
	// never a partial group. [head, summary, final].
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3 (head, summary, final): %+v", len(out), out)
	}
	if got := schema.MessageText(out[0]); got != "task" {
		t.Errorf("out[0] = %q, want %q (the head)", got, "task")
	}
	if got := schema.MessageText(out[2]); got != "final" {
		t.Errorf("out[2] = %q, want %q (the tail)", got, "final")
	}
}

// TestSummarizeDeclinesOnStaleWireGroups is #457 requirement 3: a non-nil Ctx.WireGroup whose
// length no longer matches req.Input means an EARLIER component changed the message count, so
// the groups' current positions are unknown. summarizeSpan's own defensive check collapses that
// to "no groups" and computes an UNALIGNED boundary; summarize must decline rather than act on
// it, because an unaligned boundary can split a wire message that really exists.
func TestSummarizeDeclinesOnStaleWireGroups(t *testing.T) {
	c, err := newSummarize([]byte("keep_last: 1\nmin_tokens: 1\ntrigger: {min_request_frac: 0}\n"))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	s := c.(*Summarize)

	msgs := []bschemas.ChatMessage{userMsg("task"), userMsg("middle"), userMsg("final")}
	req := &bschemas.BifrostChatRequest{Input: msgs}
	rep := &components.Report{}
	// Deliberately mismatched length (4, not 3): simulates WireGroup computed by apply.go
	// against an EARLIER pipeline position, before some other component changed the count.
	ctx := &components.Ctx{WireGroup: []int{0, 1, 2, 3}}

	_, err = s.Offload(req, rep, ctx)
	if err != nil {
		t.Fatalf("Offload returned an error, want a plain decline: %v", err)
	}
	if !rep.Skipped {
		t.Error("rep.Skipped = false, want true — a stale WireGroup must decline, not act unaligned")
	}
	if rep.Gates["wire_groups_stale"] != 1 {
		t.Errorf("Gates = %v, want wire_groups_stale: 1", rep.Gates)
	}
	if len(req.Input) != 3 {
		t.Errorf("req.Input was mutated (len=%d), want left untouched", len(req.Input))
	}
}

// TestCacheAwareSummarizerDeclinesOnStaleWireGroups is the same requirement for
// cache_aware_summarizer, which shares summarizeSpan but checks staleness in its own Offload.
func TestCacheAwareSummarizerDeclinesOnStaleWireGroups(t *testing.T) {
	c, err := newCacheAwareSummarizer([]byte("keep_last_turns: 1\nmin_tokens: 1\n" +
		"trigger:\n  min_messages: 1\n  min_request_tokens: 1\n  min_request_frac: 0\n"))
	if err != nil {
		t.Fatalf("newCacheAwareSummarizer: %v", err)
	}
	s := c.(*CacheAwareSummarizer)

	msgs := []bschemas.ChatMessage{userMsg("task"), userMsg("middle"), userMsg("final")}
	req := &bschemas.BifrostChatRequest{Input: msgs}
	rep := &components.Report{}
	ctx := &components.Ctx{WireGroup: []int{0, 1, 2, 3}}

	_, err = s.Offload(req, rep, ctx)
	if err != nil {
		t.Fatalf("Offload returned an error, want a plain decline: %v", err)
	}
	if !rep.Skipped {
		t.Error("rep.Skipped = false, want true — a stale WireGroup must decline, not act unaligned")
	}
	if rep.Gates["wire_groups_stale"] != 1 {
		t.Errorf("Gates = %v, want wire_groups_stale: 1", rep.Gates)
	}
	if len(req.Input) != 3 {
		t.Errorf("req.Input was mutated (len=%d), want left untouched", len(req.Input))
	}
}
