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

// Live LOCA traffic produced 28 provider rejections in 75 runs with
//
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//
// and a capture hop between the proxy and the gateway showed summarize emitting exactly three
// messages: [user, summary(user), assistant(thinking,tool_use,tool_use)] -- a request ENDING on an
// unanswered call. See docs/experiments/loca/iter011/results.md.
//
// The open question this test settles: does summarize turn a VALID transcript into that shape, or was
// the input already invalid? Those need opposite responses -- a component fix versus a rig/cascade
// investigation -- so guessing is not acceptable.
func TestSummarizeNeverEmitsATrailingUnansweredCall(t *testing.T) {
	big := strings.Repeat("tool output line mentioning src/auth.py and TOKEN_GRACE_41ab\n", 80)
	mk := func(tailKind string) []bschemas.ChatMessage {
		msgs := []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleUser,
				Content: &bschemas.ChatMessageContent{ContentStr: sp("Fix test_auth_expiry")}},
		}
		for i := 0; i < 10; i++ {
			id := "call_" + string(rune('a'+i))
			nm := "Read"
			msgs = append(msgs,
				bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
					Content: &bschemas.ChatMessageContent{ContentStr: sp("reading")},
					ChatAssistantMessage: &bschemas.ChatAssistantMessage{
						ToolCalls: []bschemas.ChatAssistantMessageToolCall{
							{ID: &id, Function: bschemas.ChatAssistantMessageToolCallFunction{
								Name: &nm, Arguments: `{"p":"a.py"}`}}}}},
				bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
					Content:         &bschemas.ChatMessageContent{ContentStr: sp(big)},
					ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}})
		}
		if tailKind == "user" {
			msgs = append(msgs, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
				Content: &bschemas.ChatMessageContent{ContentStr: sp("keep going")}})
		}
		return msgs
	}
	for _, kind := range []string{"toolresult", "user"} {
		t.Run("tail="+kind, func(t *testing.T) {
			in := mk(kind)
			if v := schema.ValidateShape(in); len(v) != 0 {
				t.Fatalf("fixture invalid, test proves nothing: %v", v)
			}
			s, err := newSummarize([]byte("{\"min_tokens\":100,\"keep_last\":3}"))
			if err != nil {
				t.Fatalf("newSummarize: %v", err)
			}
			req := &bschemas.BifrostChatRequest{Input: in}
			c := &components.Ctx{Ctx: context.Background(), Session: "trail-" + kind,
				Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
				Model: components.ModelSpec{Incoming: stubMdl{}, Static: stubMdl{}}}
			if off, ok := s.(components.Offload); ok {
				if _, err := off.Offload(req, &components.Report{}, c); err != nil {
					t.Fatalf("Offload: %v", err)
				}
			}
			for i, m := range req.Input {
				t.Logf("  [%d] role=%s calls=%d", i, m.Role,
					func() int { if m.ChatAssistantMessage != nil { return len(m.ChatAssistantMessage.ToolCalls) }; return 0 }())
			}
			if v := schema.ValidateShape(req.Input); len(v) != 0 {
				t.Errorf("summarize turned a VALID transcript into one a provider rejects:")
				for _, x := range v {
					t.Errorf("    %s", x)
				}
			}
		})
	}
}

func sp(s string) *string { return &s }

type stubMdl struct{}

func (stubMdl) Complete(ctx context.Context, prompt string) (string, error) {
	return "SUMMARY: read the auth module.", nil
}
func (stubMdl) CompleteSystem(ctx context.Context, sys, user string) (string, error) {
	return "SUMMARY: read the auth module.", nil
}
func (stubMdl) Name() string { return "stub" }
