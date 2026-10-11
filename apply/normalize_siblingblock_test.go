package apply

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// TestNormalizeDropsASiblingBlockBesideAToolResult pins a REAL defect on main, found while
// building issue #424's wait-cause recorder (PR #453): an Anthropic user message that mixes a
// tool_result block with any OTHER content block (plain text, most commonly) in the SAME
// message loses that other block entirely.
//
// normalize's own Anthropic branch only reads blocks of type "tool_result" out of a user
// message; every tool_result found sets `handled = true`, and `if handled { continue }` then
// skips the message's whole-message fallback path too — so a sibling text block is never
// read by either path. It is lost from the NORMALIZED view only; the raw body is untouched,
// so nothing downstream of normalize that reads bytes directly is affected, but every reader
// of normalize's own output (NormalizeMessages, and so this package's own compaction
// components) never sees that text at all.
//
// This is NOT hypothetical. The Messages API requires alternating user/assistant turns, so a
// client that wants to say something ALONGSIDE a tool's result — text typed while the tool
// ran, a system-reminder, a task notification — has nowhere to put it but a sibling block in
// that SAME tool_result message. #453 found this while making issue #424's wait-cause
// recorder read apply.NormalizeMessages instead of its own parser, and decided NOT to fix it
// there: a defect on main that affects every component gets its own issue and its own PR, not
// a fix riding a feature branch. This test exists so that issue has a fix to turn green rather
// than a bug to rediscover. It is EXPECTED TO FAIL once that issue is fixed — when it does,
// update this test to assert the sibling text IS present, and remove this comment.
func TestNormalizeDropsASiblingBlockBesideAToolResult(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call1","content":"file1\nfile2"},
			{"type":"text","text":"also, please double check the second file"}
		]}
	]}`)
	msgs := gjson.GetBytes(body, "messages").Array()
	norm, _ := normalize(bschemas.Anthropic, msgs)

	if len(norm) != 1 {
		t.Fatalf("got %d normalized messages, want 1 (the extracted tool_result only) — "+
			"if this is now 2, the sibling text is no longer being dropped and this test "+
			"should be updated to assert its content is kept, not that it is missing", len(norm))
	}
	if norm[0].Role != bschemas.ChatMessageRoleTool {
		t.Fatalf("got role %q, want the extracted tool_result message", norm[0].Role)
	}
	// The sibling text does not appear ANYWHERE in the normalized output — not merged into
	// the tool message, not as its own entry.
	for _, m := range norm {
		if containsText(m, "double check the second file") {
			t.Fatalf("the sibling text block is no longer dropped — update this test's " +
				"assertions to match the fix instead of pinning the defect")
		}
	}
}

func containsText(m bschemas.ChatMessage, substr string) bool {
	if m.Content == nil {
		return false
	}
	if m.Content.ContentStr != nil {
		return strings.Contains(*m.Content.ContentStr, substr)
	}
	for _, b := range m.Content.ContentBlocks {
		if b.Text != nil && strings.Contains(*b.Text, substr) {
			return true
		}
	}
	return false
}
