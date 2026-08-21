package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// The shape live traffic actually carries, and which no existing fixture covered: assistant messages
// with TWO tool_use blocks (a parallel call), each answered by its own role=tool message. Live LOCA
// runs failed 28/75 with the provider reporting an unanswered call, and the capture hop showed
// summarize emitting a trailing assistant(tool_use, tool_use) with nothing after it.
//
// The rig-side shim's repair_tool_pairing synthesises results for unanswered calls and reported
// repairs=0 on that run, so the INPUT was well formed. That leaves summarize's restructuring.
func TestSummarizePreservesParallelToolExchanges(t *testing.T) {
	big := strings.Repeat("tool output mentioning src/auth.py and TOKEN_GRACE_41ab\n", 80)
	var msgs []bschemas.ChatMessage
	msgs = append(msgs, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: sp("do the thing")}})
	nm := "Read"
	for i := 0; i < 10; i++ {
		a := "pa_" + string(rune('a'+i))
		b := "pb_" + string(rune('a'+i))
		msgs = append(msgs,
			bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
				Content: &bschemas.ChatMessageContent{ContentStr: sp("calling two tools")},
				ChatAssistantMessage: &bschemas.ChatAssistantMessage{
					ToolCalls: []bschemas.ChatAssistantMessageToolCall{
						{ID: &a, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &nm, Arguments: `{}`}},
						{ID: &b, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &nm, Arguments: `{}`}},
					}}},
			bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
				Content:         &bschemas.ChatMessageContent{ContentStr: sp(big)},
				ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &a}},
			bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
				Content:         &bschemas.ChatMessageContent{ContentStr: sp(big)},
				ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &b}},
		)
	}
	for _, keepLast := range []int{1, 2, 3, 4, 5} {
		t.Run("keep_last="+string(rune('0'+keepLast)), func(t *testing.T) {
			in := append([]bschemas.ChatMessage(nil), msgs...)
			if v := schema.ValidateShape(in); len(v) != 0 {
				t.Fatalf("fixture invalid, proves nothing: %v", v)
			}
			cfg := "{\"min_tokens\":1,\"start_from_message\":0,\"keep_last\":" + string(rune(0x30+keepLast)) + "}"
			s, err := newSummarize([]byte(cfg))
			if err != nil {
				t.Fatalf("newSummarize: %v", err)
			}
			req := &bschemas.BifrostChatRequest{Input: in}
			c := &components.Ctx{Ctx: context.Background(), Session: "par" + cfg,
				Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
				Model: components.ModelSpec{Incoming: stubMdl{}, Static: stubMdl{}}}
			off, ok := s.(components.Offload)
			if !ok {
				t.Fatal("summarize is not an Offload")
			}
			if _, err := off.Offload(req, &components.Report{}, c); err != nil {
				t.Fatalf("Offload: %v", err)
			}
			if v := schema.ValidateShape(req.Input); len(v) != 0 {
				t.Errorf("keep_last=%d: summarize broke a valid PARALLEL transcript:", keepLast)
				for _, x := range v {
					t.Errorf("    %s", x)
				}
				for i, m := range req.Input {
					n := 0
					if m.ChatAssistantMessage != nil {
						n = len(m.ChatAssistantMessage.ToolCalls)
					}
					id := ""
					if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
						id = *m.ChatToolMessage.ToolCallID
					}
					t.Errorf("      [%d] %-9s calls=%d answers=%s", i, m.Role, n, id)
				}
			}
		})
	}
}
