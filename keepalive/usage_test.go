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

func TestPingUsageOfGarbageIsZero(t *testing.T) {
	if u := PingUsage([]byte("not json")); u != (Usage{}) {
		t.Fatalf("PingUsage(garbage) = %+v, want zero", u)
	}
}
