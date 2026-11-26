package keepalive

import "testing"

func TestPingUsageReadsAnthropic(t *testing.T) {
	u := PingUsage([]byte(`{"stop_reason":"max_tokens","usage":{"input_tokens":3,` +
		`"cache_read_input_tokens":48000,"cache_creation_input_tokens":7,` +
		`"cache_creation":{"ephemeral_1h_input_tokens":5},"output_tokens":1}}`))
	want := Usage{FreshInput: 3, CacheRead: 48000, CacheWrite: 7, CacheWrite1h: 5, Output: 1, StopReason: "max_tokens"}
	if u != want {
		t.Fatalf("PingUsage = %+v, want %+v", u, want)
	}
}

// Responses' input_tokens INCLUDES the cached part; fresh is the difference.
func TestPingUsageReadsResponses(t *testing.T) {
	u := PingUsage([]byte(`{"usage":{"input_tokens":48003,` +
		`"input_tokens_details":{"cached_tokens":48000},"output_tokens":16}}`))
	if u.FreshInput != 3 || u.CacheRead != 48000 || u.Output != 16 || u.CacheWrite != 0 {
		t.Fatalf("PingUsage = %+v", u)
	}
}

// A Responses dialect ping that WRITES the cache reports it under input_tokens_details's own
// cache_write_tokens field, not cache_creation_input_tokens (that's Anthropic's shape). PingUsage
// must read it, or the write-vs-read guard (CacheWrite > CacheRead) never fires for this dialect.
func TestPingUsageReadsResponsesCacheWrite(t *testing.T) {
	u := PingUsage([]byte(`{"usage":{"input_tokens":48003,` +
		`"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":48000},"output_tokens":5}}`))
	if u.CacheWrite != 48000 {
		t.Fatalf("CacheWrite = %d, want 48000", u.CacheWrite)
	}
	if u.FreshInput != 3 {
		t.Fatalf("FreshInput = %d, want 3 (input_tokens minus both cache tiers)", u.FreshInput)
	}
}

func TestPingUsageOfGarbageIsZero(t *testing.T) {
	if u := PingUsage([]byte("not json")); u != (Usage{}) {
		t.Fatalf("PingUsage(garbage) = %+v, want zero", u)
	}
}

// A ping on an OpenAI wire gives its stop reason too (#1481). It was empty: only Anthropic's
// stop_reason was read.
func TestPingUsageReadsTheStopReasonOfEveryWire(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"anthropic", `{"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "end_turn"},
		{"chat", `{"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`, "tool_calls"},
		{"responses", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":1}}`, "max_output_tokens"},
		{"responses, completed", `{"status":"completed","usage":{"input_tokens":5,"output_tokens":1}}`, ""},
	} {
		if got := PingUsage([]byte(c.body)).StopReason; got != c.want {
			t.Errorf("%s: stop reason %q, want %q", c.name, got, c.want)
		}
	}
}

// keepalive.ToolStopReasons is the one list, and every word in it says "called a tool".
func TestToolStopReasonsIsTheSchemaList(t *testing.T) {
	for _, r := range []string{"tool_use", "tool_calls", "function_call"} {
		if !toolStop(r) {
			t.Errorf("%q is not a tool stop reason", r)
		}
	}
	for _, r := range []string{"end_turn", "stop", "length", ""} {
		if toolStop(r) {
			t.Errorf("%q is a tool stop reason", r)
		}
	}
}
