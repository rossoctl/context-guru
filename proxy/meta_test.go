package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

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
