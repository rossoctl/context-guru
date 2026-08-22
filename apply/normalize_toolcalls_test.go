package apply

import (
	"encoding/json"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// Does normalize populate ChatAssistantMessage.ToolCalls for an ANTHROPIC assistant message whose
// tool calls arrive as  CONTENT BLOCKS? dropOrphanedToolResults builds its "answerable"
// set exclusively from that field, so if it is empty on Anthropic traffic then every tool_result
// looks orphaned and the repair DELETES them all -- which is exactly what the instrumented rebuild
// showed (out contained no tool messages at all).
//
// The offload-level tests passed because they hand-build ChatMessages with ToolCalls populated, i.e.
// the OpenAI-shaped representation. That is why unit tests were green while live Anthropic traffic
// failed 28/75.
func TestNormalizePopulatesToolCallsForAnthropicBlocks(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "calling"},
				{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "t1", "content": "the output"},
			}},
		},
	})
	arr := gjson.GetBytes(body, "messages").Array()
	norm, _ := normalize(bschemas.Anthropic, arr)
	for i, m := range norm {
		n := 0
		if m.ChatAssistantMessage != nil {
			n = len(m.ChatAssistantMessage.ToolCalls)
		}
		id := ""
		if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
			id = *m.ChatToolMessage.ToolCallID
		}
		t.Logf("norm[%d] role=%-9s ToolCalls=%d answers=%q", i, m.Role, n, id)
	}
	var calls int
	for _, m := range norm {
		if m.ChatAssistantMessage != nil {
			calls += len(m.ChatAssistantMessage.ToolCalls)
		}
	}
	if calls == 0 {
		t.Errorf("no assistant ToolCalls recovered from Anthropic tool_use blocks: "+
			"dropOrphanedToolResults will treat every tool_result as an orphan and delete it")
	}
}
