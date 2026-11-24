package reqfacts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMetaFromBodyAnthropic(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5",
		"max_tokens":4096,
		"temperature":1.0,
		"top_p":0.95,
		"top_k":50,
		"stop_sequences":["</search>"],
		"metadata":{"user_id":"u-1"},
		"stream":true,
		"thinking":{"type":"adaptive"},
		"output_config":{"effort":"xhigh"},
		"tool_choice":{"type":"auto"},
		"system":[
			{"type":"text","text":"You are careful.","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"More context."}],
		"tools":[{"name":"search"},{"name":"bash"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	m := MetaFromBody(body)
	if m.ReasoningEffort != "xhigh" {
		t.Errorf("effort = %q, want xhigh (Anthropic nests it under output_config)", m.ReasoningEffort)
	}
	if m.ThinkingMode != "adaptive" || m.ThinkingBudget != 0 {
		t.Errorf("thinking = %q/%d, want adaptive/0", m.ThinkingMode, m.ThinkingBudget)
	}
	if m.MaxTokens != 4096 || !m.Stream {
		t.Errorf("max_tokens/stream = %d/%v, want 4096/true", m.MaxTokens, m.Stream)
	}
	if m.Temperature == nil || *m.Temperature != 1.0 {
		t.Errorf("temperature = %v, want 1", m.Temperature)
	}
	if m.TopP == nil || *m.TopP != 0.95 {
		t.Errorf("top_p = %v, want 0.95", m.TopP)
	}
	if m.ToolChoice != "auto" {
		t.Errorf("tool_choice = %q, want auto (from the object form)", m.ToolChoice)
	}
	if m.Tools != 2 || m.SystemBlocks != 2 {
		t.Errorf("tools/system_blocks = %d/%d, want 2/2", m.Tools, m.SystemBlocks)
	}
}

func TestMetaFromBodyAnthropicLegacyThinking(t *testing.T) {
	m := MetaFromBody([]byte(`{
		"thinking":{"type":"enabled","budget_tokens":8000},
		"output_config":{"effort":{"type":"low"}},
		"system":"a bare string system prompt",
		"messages":[]}`))
	if m.ThinkingMode != "enabled" || m.ThinkingBudget != 8000 {
		t.Errorf("thinking = %q/%d, want enabled/8000", m.ThinkingMode, m.ThinkingBudget)
	}
	if m.ReasoningEffort != "low" {
		t.Errorf("effort = %q, want low (from the {\"type\":…} object form)", m.ReasoningEffort)
	}
	if m.SystemBlocks != 1 {
		t.Errorf("system_blocks = %d, want 1 for a bare string", m.SystemBlocks)
	}
}

func TestMetaFromBodyOpenAI(t *testing.T) {
	m := MetaFromBody([]byte(`{
		"model":"gpt-x",
		"max_completion_tokens":2048,
		"temperature":0,
		"reasoning_effort":"high",
		"tool_choice":"required",
		"stream":false,
		"tools":[{"type":"function","function":{"name":"run"}}],
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`))
	if m.ReasoningEffort != "high" {
		t.Errorf("effort = %q, want high", m.ReasoningEffort)
	}
	if m.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048 from max_completion_tokens", m.MaxTokens)
	}
	// The distinction the nullable column exists for: temperature 0 is a REQUEST for
	// determinism, not an absent value.
	if m.Temperature == nil || *m.Temperature != 0 {
		t.Fatalf("temperature = %v, want a set 0 — collapsing it to absent misreports every deterministic request", m.Temperature)
	}
	if m.ToolChoice != "required" {
		t.Errorf("tool_choice = %q, want required (bare string form)", m.ToolChoice)
	}
	if m.Tools != 1 || m.SystemBlocks != 0 || m.Stream {
		t.Errorf("tools/system_blocks/stream = %d/%d/%v, want 1/0/false", m.Tools, m.SystemBlocks, m.Stream)
	}
}

func TestMetaFromBodyAbsentAndObjectToolChoice(t *testing.T) {
	m := MetaFromBody([]byte(`{"model":"m","tool_choice":{"type":"function","function":{"name":"run"}},"messages":[]}`))
	if m.Temperature != nil || m.TopP != nil {
		t.Errorf("absent sampling params became %v/%v, want nil", m.Temperature, m.TopP)
	}
	if m.ToolChoice != "function" {
		t.Errorf("tool_choice = %q, want the mode, not the tool name", m.ToolChoice)
	}
	// A body that is not an object at all must not panic or invent values.
	if got := MetaFromBody([]byte(`[]`)); got != (metaZero) {
		t.Errorf("non-object body produced %+v, want the zero value", got)
	}
}

var metaZero = MetaFromBody([]byte(`{}`))

func BenchmarkMetaFromBody(b *testing.B) {
	body, err := json.Marshal(map[string]any{
		"model": "aws/claude-sonnet-5", "max_tokens": 512, "temperature": 1,
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "high"},
		"tools":         []any{map[string]any{"name": "bash"}},
		"system":        []any{map[string]any{"type": "text", "text": "sys"}},
		"messages":      []any{map[string]any{"role": "user", "content": bigText()}},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = MetaFromBody(body)
	}
}

// bigText is a repetitive ~90 KB string, shaped like a tool result.
func bigText() string {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("  File \"/repo/pkg/mod/thing.py\", line 42, in handler\n    result = compute(x, y)\n")
	}
	return b.String()
}
