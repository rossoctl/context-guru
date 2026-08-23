package apply_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// LOCA's context clearing is Anthropic's SERVER-SIDE context_management feature, passed as a request
// parameter (clear_tool_uses_20250919 with trigger input_tokens). If a count-changing component
// dropped that parameter while rewriting the body, the provider's clearing would be silently
// disabled -- and since the proxy also shrinks the request below the trigger, nothing would keep an
// oversized transcript legal. Five runs in the 128k arms died on "prompt is too long" at up to 6M
// tokens, so whether this survives is a product question, not a formality.
func TestContextManagementParamSurvivesCountChange(t *testing.T) {
	big := strings.Repeat("verbose tool output line\n", 200)
	msgs := []map[string]any{{"role": "user", "content": "go"}}
	for i := 0; i < 6; i++ {
		id := "t" + string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "calling"},
				{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": id, "content": big}}})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "final"})
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-x",
		"messages": msgs,
		"context_management": map[string]any{"edits": []map[string]any{{
			"type":          "clear_tool_uses_20250919",
			"trigger":       map[string]any{"type": "input_tokens", "value": 128000},
			"keep":          map[string]any{"type": "tool_uses", "value": 3},
			"clear_at_least": map[string]any{"type": "input_tokens", "value": 20000},
		}}},
	})
	cfg := pipe(t, "pipeline: [format, summarize]\ncomponents:\n  summarize: {keep_last: 2, start_from_message: 0, min_tokens: 1}\n")
	p, _ := cfg.Build(nil)
	out, changed := apply.BodyWithModel(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
	if !changed {
		t.Skip("summarize did not act")
	}
	got := gjson.GetBytes(out, "context_management")
	if !got.Exists() {
		t.Fatal("context_management was DROPPED by the rewrite: the provider's own clearing would be " +
			"silently disabled while the proxy also keeps requests under its trigger")
	}
	want := gjson.GetBytes(body, "context_management")
	if got.Raw != want.Raw {
		t.Errorf("context_management was altered:\n got %s\nwant %s", got.Raw, want.Raw)
	}
	if v := got.Get("edits.0.trigger.value").Int(); v != 128000 {
		t.Errorf("trigger value changed to %d", v)
	}
}
