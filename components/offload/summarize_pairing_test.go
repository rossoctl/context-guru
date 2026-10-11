package offload

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

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
