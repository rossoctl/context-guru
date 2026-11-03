package expand

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func sseEvent(data string) string { return "data: " + data + "\n\n" }

// AggregateSSE must reconstruct an extended-thinking response faithfully: the thinking
// block's text AND signature (from thinking_delta / signature_delta) plus the tool_use
// call. Anthropic rejects a continued assistant turn that omits the thinking block or its
// signature, so a lossy reconstruction turns the expand loop into a hard upstream error.
func TestAggregateSSEPreservesThinkingAndToolUse(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseEvent(`{"type":"message_start","message":{"role":"assistant"}}`))
	b.WriteString(sseEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	b.WriteString(sseEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me reason "}}`))
	b.WriteString(sseEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"about this"}}`))
	b.WriteString(sseEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIGV123"}}`))
	b.WriteString(sseEvent(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"context_guru_expand"}}`))
	b.WriteString(sseEvent(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":\"abc\"}"}}`))
	b.WriteString(sseEvent(`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`))

	msg, ok := AggregateSSE("anthropic", []byte(b.String()))
	if !ok {
		t.Fatalf("AggregateSSE should reconstruct the anthropic stream")
	}
	res := gjson.ParseBytes(msg)
	c0 := res.Get("content.0")
	if c0.Get("type").String() != "thinking" {
		t.Fatalf("block 0 should be a thinking block: %s", msg)
	}
	if got := c0.Get("thinking").String(); got != "let me reason about this" {
		t.Fatalf("thinking text lost/garbled: %q", got)
	}
	if got := c0.Get("signature").String(); got != "SIGV123" {
		t.Fatalf("thinking signature dropped: %q\n%s", got, msg)
	}
	c1 := res.Get("content.1")
	if c1.Get("type").String() != "tool_use" || c1.Get("name").String() != "context_guru_expand" {
		t.Fatalf("tool_use block lost: %s", msg)
	}
	if got := c1.Get("input.id").String(); got != "abc" {
		t.Fatalf("tool_use input lost: %q", got)
	}
}

// Non-anthropic dialects are not reconstructed (the caller streams through unchanged).
func TestAggregateSSENonAnthropicBails(t *testing.T) {
	if _, ok := AggregateSSE("openai", []byte("data: {}\n\n")); ok {
		t.Fatal("non-anthropic dialect must return ok=false")
	}
}

// The Chat Completions stream: text, tool calls joined by index, finish reason, usage, and the
// reasoning deltas GLM and Kimi stream (#825).
func TestAggregateSSEChat(t *testing.T) {
	raw := "data: {\"id\":\"x\",\"model\":\"gpt-5.5\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"context_guru_tools\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"names\\\":[\\\"Mon\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"itor\\\"]}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	msg, ok := AggregateSSE("openai", []byte(raw))
	if !ok {
		t.Fatal("not rebuilt")
	}
	tc := gjson.GetBytes(msg, "choices.0.message.tool_calls.0")
	if tc.Get("id").String() != "c1" || tc.Get("function.name").String() != "context_guru_tools" ||
		gjson.Get(tc.Get("function.arguments").String(), "names.0").String() != "Monitor" {
		t.Fatalf("tool call = %s", tc.Raw)
	}
	if gjson.GetBytes(msg, "choices.0.message.reasoning_content").String() != "think" {
		t.Fatalf("reasoning lost: %s", msg)
	}
	if gjson.GetBytes(msg, "usage.prompt_tokens").Int() != 10 || gjson.GetBytes(msg, "choices.0.finish_reason").String() != "tool_calls" {
		t.Fatalf("msg = %s", msg)
	}
}
