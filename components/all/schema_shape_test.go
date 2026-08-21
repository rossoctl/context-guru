package all_test

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// EVERY SHIPPED PRESET MUST EMIT A SENDABLE REQUEST.
//
// This is the guard that was missing. Three message-shape violations shipped in `summarize`
// and were found only by sending live traffic to a real API, one at a time, each masked by
// the previous one:
//
//	400 messages.1: role 'system' must precede an 'assistant' message or end the array
//	400 messages.N.content.M: unexpected `tool_use_id` found in `tool_result` blocks
//	400 messages.N: `tool_use` ids were found without `tool_result` blocks immediately after
//
// They were invisible to this repo's two verification methods. No test asserted shape, and
// every offline measurement replayed through `/compact`, which runs the pipeline and returns
// the rewritten body WITHOUT forwarding upstream — so no provider ever validated it. Replay
// measures what a component removed; it cannot tell you the result is sendable.
//
// All three are statically checkable (schema.ValidateShape), so this test closes the gap for
// free: a table over every registered preset, run on a tool-heavy transcript, asserting the
// output breaks no shape invariant.
func TestEveryPresetEmitsAShapeValidRequest(t *testing.T) {
	// A transcript with the features that break naive restructuring: a system head, several
	// complete tool exchanges, and enough mass that size-gated components actually fire.
	fixture := func() *bschemas.BifrostChatRequest {
		big := ""
		for i := 0; i < 60; i++ {
			big += "line of tool output referencing src/auth.py and TOKEN_GRACE_SECONDS_41ab\n"
		}
		msgs := []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleSystem,
				Content: &bschemas.ChatMessageContent{ContentStr: strp("you are a coding agent")}},
			{Role: bschemas.ChatMessageRoleUser,
				Content: &bschemas.ChatMessageContent{ContentStr: strp("Fix test_auth_expiry in src/auth.py")}},
		}
		for i := 0; i < 6; i++ {
			id := "call_" + string(rune('a'+i))
			name := "Read"
			args := `{"path":"src/auth.py"}`
			msgs = append(msgs,
				bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
					Content: &bschemas.ChatMessageContent{ContentStr: strp("Reading the module.")},
					ChatAssistantMessage: &bschemas.ChatAssistantMessage{
						ToolCalls: []bschemas.ChatAssistantMessageToolCall{
							{ID: &id, Function: bschemas.ChatAssistantMessageToolCallFunction{
								Name: &name, Arguments: args}}}}},
				bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
					Content:         &bschemas.ChatMessageContent{ContentStr: strp(big)},
					ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}},
			)
		}
		msgs = append(msgs, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
			Content: &bschemas.ChatMessageContent{ContentStr: strp("keep going")}})
		return &bschemas.BifrostChatRequest{Input: msgs}
	}

	// Sanity: the fixture itself must be valid, or a failure below proves nothing.
	if v := schema.ValidateShape(fixture().Input); len(v) != 0 {
		t.Fatalf("fixture is not shape-valid, so this test cannot attribute violations: %v", v)
	}

	for _, name := range []string{
		"off", "safe", "balanced", "aggressive", "coding", "mcp",
		"agent", "general", "summarize", "codesmart", "codesafe",
	} {
		t.Run(name, func(t *testing.T) {
			names, ok := config.PresetPipeline(name)
			if !ok {
				t.Fatalf("preset %q is not registered", name)
			}
			cfg := &config.Config{Pipeline: names}
			pipe, err := cfg.Build(nil)
			if err != nil {
				t.Skipf("preset %q needs config this test does not supply: %v", name, err)
			}
			req := fixture()
			c := &components.Ctx{
				Ctx: context.Background(), Session: "shape-" + name,
				Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
				// A stub model so the LLM-driven components actually restructure rather
				// than degrading to a no-op — a skipped component proves nothing here.
				Model: components.ModelSpec{Incoming: stubModel{resp: "SUMMARY: read the auth module."},
					Static: stubModel{resp: "SUMMARY: read the auth module."}},
			}
			pipe.Run(req, c)
			if v := schema.ValidateShape(req.Input); len(v) != 0 {
				t.Errorf("preset %q emitted a request a provider would reject:", name)
				for _, x := range v {
					t.Errorf("    %s", x)
				}
			}
		})
	}
}
