package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// The two dialects spell the same knobs differently, and the dashboard aggregates them in
// ONE column each — so the normalization is the thing under test, not the JSON reading.
//
// The Anthropic body is shaped after a real captured Claude request (model, max_tokens,
// temperature, top_p, top_k, stop_sequences, metadata, stream, a `system` ARRAY of blocks
// carrying cache_control, tools, messages), plus the two reasoning controls current models
// use: `thinking.type` and `output_config.effort`.
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
	m := metaFromBody(body)
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

// The pre-4.6 thinking form, and Anthropic's other accepted effort shape.
func TestMetaFromBodyAnthropicLegacyThinking(t *testing.T) {
	m := metaFromBody([]byte(`{
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

// OpenAI puts effort at the top level as `reasoning_effort`, has no top-level `system`
// (it is a role=system message), caps output with `max_completion_tokens`, and sends
// tool_choice as a bare string.
func TestMetaFromBodyOpenAI(t *testing.T) {
	m := metaFromBody([]byte(`{
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

// An unset sampling parameter must stay unset, and OpenAI's object tool_choice names one
// function — recorded as the MODE, never the name.
func TestMetaFromBodyAbsentAndObjectToolChoice(t *testing.T) {
	m := metaFromBody([]byte(`{"model":"m","tool_choice":{"type":"function","function":{"name":"run"}},"messages":[]}`))
	if m.Temperature != nil || m.TopP != nil {
		t.Errorf("absent sampling params became %v/%v, want nil", m.Temperature, m.TopP)
	}
	if m.ToolChoice != "function" {
		t.Errorf("tool_choice = %q, want the mode, not the tool name", m.ToolChoice)
	}
	// A body that is not an object at all must not panic or invent values.
	if got := metaFromBody([]byte(`[]`)); got != (metaZero) {
		t.Errorf("non-object body produced %+v, want the zero value", got)
	}
}

// metaZero is the zero value, named so the comparison above reads as intent.
var metaZero = metaFromBody([]byte(`{}`))

// The stop reason is the provider's own vocabulary, read from whichever shape and
// transport the response arrived in. The streamed case is the one with a trap in it: an
// earlier chunk carries `finish_reason: null`, and only the LAST event that names one is
// the answer.
func TestResponseStopReason(t *testing.T) {
	cases := []struct {
		name, contentType, body, want string
	}{
		{"anthropic unary", "application/json",
			`{"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`, "tool_use"},
		{"anthropic refusal", "application/json",
			`{"stop_reason":"refusal","usage":{"input_tokens":5,"output_tokens":1}}`, "refusal"},
		{"openai unary", "application/json",
			`{"choices":[{"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`, "length"},
		{"anthropic sse", "text/event-stream",
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":9}}\n",
			"max_tokens"},
		{"openai sse ignores the null in an earlier chunk", "text/event-stream",
			"data: {\"choices\":[{\"finish_reason\":null}]}\n" +
				"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n" +
				"data: [DONE]\n",
			"tool_calls"},
		{"none reported", "application/json", `{"usage":{"input_tokens":1,"output_tokens":1}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, _ := responseUsage(c.contentType, []byte(c.body))
			if u.StopReason != c.want {
				t.Fatalf("stop reason = %q, want %q", u.StopReason, c.want)
			}
		})
	}
}

// A response with no usage block at all still reports why it stopped: the two facts are
// independent, and dropping the reason would blank it for exactly the rows already marked
// partial. OpenAI without `stream_options` is the real case.
func TestStopReasonSurvivesMissingUsage(t *testing.T) {
	u, ok := responseUsage("application/json", []byte(`{"choices":[{"finish_reason":"stop"}]}`))
	if ok {
		t.Fatal("a body with no usage block must not report usage as available")
	}
	if u.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want stop even with no usage block", u.StopReason)
	}
}

// End to end, through the real handler: the row a proxied request produces has to carry
// the request's metadata, the breakpoint placement, and the provider's stop reason. The
// per-field parsing is unit-tested above; what this covers is the WIRING — a forgotten
// noteMeta call, or a trace field never copied onto the event, is invisible to a parser
// test and would silently blank every one of these columns.
func TestDashboardCapturesRequestMetadataEndToEnd(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// A metadata-rich body: two breakpoints in `system`, one on a message content block,
	// plus the reasoning and sampling knobs.
	body := map[string]any{
		"model":      "aws/claude-sonnet-5",
		"max_tokens": 512,
		"stream":     false,
		// 0 rather than absent, because the two must not be confused end to end either.
		"temperature":   0,
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "xhigh"},
		"tool_choice":   map[string]any{"type": "auto"},
		"tools":         []any{map[string]any{"name": "bash", "input_schema": map[string]any{"type": "object"}}},
		"system": []any{
			map[string]any{"type": "text", "text": "You are careful.", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "Repo context.", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "fix the test", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tu_1", "name": "bash", "input": map[string]any{"command": "pytest"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": bigToolOutput()},
			}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-meta")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("captured %d rows, want 1", len(page.Requests))
	}
	e := page.Requests[0]
	if e.ReasoningEffort != "xhigh" || e.ThinkingMode != "adaptive" {
		t.Errorf("effort/thinking = %q/%q, want xhigh/adaptive", e.ReasoningEffort, e.ThinkingMode)
	}
	if e.MaxTokens != 512 || e.ToolChoice != "auto" || e.Tools != 1 || e.SystemBlocks != 2 {
		t.Errorf("max_tokens/tool_choice/tools/system_blocks = %d/%q/%d/%d, want 512/auto/1/2",
			e.MaxTokens, e.ToolChoice, e.Tools, e.SystemBlocks)
	}
	if e.Temperature == nil || *e.Temperature != 0 {
		t.Errorf("temperature = %v, want a stored 0", e.Temperature)
	}
	// Placement, from the count the pipeline made anyway: two in system, one on a
	// message content block. The total is what the provider's cap of four applies to.
	if e.CacheBPSystem != 2 || e.CacheBPBlocks != 1 || e.CacheBreakpoints() != 3 {
		t.Errorf("breakpoints = system %d / tools %d / messages %d / blocks %d (total %d), want 2/0/0/1 (3)",
			e.CacheBPSystem, e.CacheBPTools, e.CacheBPMessages, e.CacheBPBlocks, e.CacheBreakpoints())
	}
	if e.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn from the upstream response", e.StopReason)
	}
}

// The claim this measures: reading the metadata is one structural pass over the top-level
// object, so it does not scale with the transcript. The body here carries a ~90 KB tool
// result, which is what a real agent turn looks like; a per-field gjson.GetBytes loop would
// re-scan past that array once per field.
func BenchmarkMetaFromBody(b *testing.B) {
	body, err := json.Marshal(map[string]any{
		"model": "aws/claude-sonnet-5", "max_tokens": 512, "temperature": 1,
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "high"},
		"tools":         []any{map[string]any{"name": "bash"}},
		"system":        []any{map[string]any{"type": "text", "text": "sys"}},
		"messages":      []any{map[string]any{"role": "user", "content": bigToolOutput()}},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = metaFromBody(body)
	}
}
