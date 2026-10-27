package proxy

import "testing"

// STREAMED USAGE SHAPES (#491). A litellm gateway streaming an Anthropic-shaped response puts the TOTAL
// prompt in message_start's input_tokens (no cache fields) and the true split in message_delta. The merge
// took the maximum of each field, so the read was counted twice: billed 14,029 for a prompt of 7,373, and
// PrevBilledInput about 2x too large. Synthetic numbers shaped like one captured GLM stream, no content.
func TestStreamedUsageReadsTheTrueSplit(t *testing.T) {
	start := func(usage string) string {
		return "data: {\"type\":\"message_start\",\"message\":{\"usage\":" + usage + "}}\n"
	}
	delta := func(usage string) string {
		return "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":" + usage + "}\n"
	}
	type split struct{ fresh, read, write, out int64 }
	for _, tc := range []struct {
		name string
		sse  string
		want split
	}{
		{"1 litellm cache hit: total in message_start, split in the delta",
			start(`{"input_tokens":7373,"output_tokens":0}`) +
				delta(`{"input_tokens":717,"cache_read_input_tokens":6656,"cache_creation_input_tokens":0,"output_tokens":3}`),
			split{717, 6656, 0, 3}},
		{"2 litellm cache miss: total in both events, no cache fields",
			start(`{"input_tokens":7373,"output_tokens":0}`) +
				delta(`{"input_tokens":7373,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":3}`),
			split{7373, 0, 0, 3}},
		{"3 real Anthropic: the tiers in message_start, an output-only delta",
			start(`{"input_tokens":717,"cache_read_input_tokens":6656,"cache_creation_input_tokens":0,"output_tokens":1}`) +
				delta(`{"output_tokens":3}`),
			split{717, 6656, 0, 3}},
		{"4 real Anthropic whose delta repeats the same input fields",
			start(`{"input_tokens":717,"cache_read_input_tokens":6656,"cache_creation_input_tokens":0,"output_tokens":1}`) +
				delta(`{"input_tokens":717,"cache_read_input_tokens":6656,"cache_creation_input_tokens":0,"output_tokens":3}`),
			split{717, 6656, 0, 3}},
		{"5 real Anthropic cold turn: a write, no read",
			start(`{"input_tokens":12,"cache_read_input_tokens":0,"cache_creation_input_tokens":7361,"output_tokens":1}`) +
				delta(`{"output_tokens":3}`),
			split{12, 0, 7361, 3}},
		{"7 output keeps the larger count when a later event reports less",
			start(`{"input_tokens":717,"cache_read_input_tokens":6656,"output_tokens":9}`) +
				delta(`{"input_tokens":717,"cache_read_input_tokens":6656,"output_tokens":3}`),
			split{717, 6656, 0, 9}},
		{"6 a stream with only a start (no delta)",
			start(`{"input_tokens":717,"cache_read_input_tokens":6656,"output_tokens":1}`),
			split{717, 6656, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, why, _ := parseSSEUsageWhy([]byte(tc.sse))
			if why != usageMissNone {
				t.Fatalf("not parsed: %v", why)
			}
			got := split{u.FreshInput, u.CacheRead, u.CacheWrite, u.Output}
			if got != tc.want {
				t.Errorf("read %+v, want %+v (billed %d, want %d)", got, tc.want,
					got.fresh+got.read+got.write, tc.want.fresh+tc.want.read+tc.want.write)
			}
		})
	}
}

// The non-streamed control for the same call reports the true split directly and must be unchanged.
func TestTheNonStreamedControlStillReadsTheTrueSplit(t *testing.T) {
	u, ok := parseUsage([]byte(`{"usage":{"input_tokens":717,"cache_read_input_tokens":6656,"cache_creation_input_tokens":0,"output_tokens":3}}`))
	if !ok || u.FreshInput != 717 || u.CacheRead != 6656 || u.CacheWrite != 0 {
		t.Fatalf("usage = %+v ok=%v, want fresh 717, read 6656", u, ok)
	}
}
