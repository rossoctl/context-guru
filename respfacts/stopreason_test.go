package respfacts

import "testing"

func TestStopReasonAllWires(t *testing.T) {
	ev := func(s string) string { return "data: " + s + "\n\n" }
	for _, c := range []struct {
		name string
		sse  bool
		body string
		want string
	}{
		{"anthropic body", false, `{"stop_reason":"tool_use"}`, "tool_use"},
		{"openai body", false, `{"choices":[{"finish_reason":"tool_calls"}]}`, "tool_calls"},
		{"responses body, incomplete", false, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, "max_output_tokens"},
		{"responses body, completed has none", false, `{"status":"completed","output":[]}`, ""},
		{"anthropic stream", true, ev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`), "end_turn"},
		{"openai stream, last chunk wins", true,
			ev(`{"choices":[{"finish_reason":null}]}`) + ev(`{"choices":[{"finish_reason":"stop"}]}`) + ev(`{"choices":[]}`) + "data: [DONE]\n\n", "stop"},
		{"responses stream, incomplete", true,
			ev(`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`), "content_filter"},
		{"responses stream, completed has none", true, ev(`{"type":"response.completed","response":{"status":"completed"}}`), ""},
		{"crlf framing", true, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\r\n\r\n", "stop"},
	} {
		if got := StopReason(c.sse, []byte(c.body)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
