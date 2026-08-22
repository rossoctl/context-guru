package proxy

import (
	"strings"
	"testing"
)

func TestParseUsageAnthropic(t *testing.T) {
	// Anthropic's input_tokens EXCLUDES the cached tiers, so it is fresh input as-is.
	body := `{"id":"msg_1","usage":{"input_tokens":14,"output_tokens":11,
	  "cache_read_input_tokens":8612,"cache_creation_input_tokens":2780}}`
	u, ok := parseUsage([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.FreshInput != 14 || u.Output != 11 || u.CacheRead != 8612 || u.CacheWrite != 2780 {
		t.Errorf("got %+v", u)
	}
}

func TestParseUsageOpenAISubtractsCachedFromPrompt(t *testing.T) {
	// OpenAI's prompt_tokens INCLUDES cached_tokens. Getting this backwards
	// double-counts the whole transcript on every turn — the kind of error a
	// "savings" figure conceals.
	body := `{"usage":{"prompt_tokens":10000,"completion_tokens":120,
	  "prompt_tokens_details":{"cached_tokens":9500}}}`
	u, ok := parseUsage([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.FreshInput != 500 {
		t.Errorf("fresh = %d; want 500 (10000 prompt − 9500 cached)", u.FreshInput)
	}
	if u.CacheRead != 9500 || u.Output != 120 {
		t.Errorf("got %+v", u)
	}
	// A cached count larger than the prompt (never seen, but arithmetic must not go
	// negative and turn into a credit).
	u, _ = parseUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":1,
	  "prompt_tokens_details":{"cached_tokens":50}}}`))
	if u.FreshInput < 0 {
		t.Errorf("fresh went negative: %+v", u)
	}
}

func TestParseUsageAbsentOrEmpty(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"id":"msg_1"}`,
		`{"usage":{}}`,
		`{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
		`not json at all`,
	} {
		if _, ok := parseUsage([]byte(body)); ok {
			t.Errorf("reported usage for %q; a response that tells us nothing must report ok=false, "+
				"so the row is flagged partial rather than priced as free", body)
		}
	}
}

func TestParseSSEUsageMergesAcrossEvents(t *testing.T) {
	// Anthropic reports the input tiers in message_start and the output count in
	// message_delta, so the tiers must be merged, not taken from one event.
	stream := `event: message_start
data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":7,"output_tokens":1,"cache_read_input_tokens":12345,"cache_creation_input_tokens":89}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"hello"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":64}}

event: message_stop
data: {"type":"message_stop"}
`
	u, ok := parseSSEUsage([]byte(stream))
	if !ok {
		t.Fatal("SSE usage not parsed")
	}
	if u.FreshInput != 7 || u.CacheRead != 12345 || u.CacheWrite != 89 {
		t.Errorf("input tiers lost across events: %+v", u)
	}
	if u.Output != 64 {
		t.Errorf("output = %d; want the final 64, not the initial 1", u.Output)
	}
}

func TestParseSSEUsageOpenAIFinalChunk(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":850}}}

data: [DONE]
`
	u, ok := parseSSEUsage([]byte(stream))
	if !ok {
		t.Fatal("SSE usage not parsed")
	}
	if u.FreshInput != 50 || u.CacheRead != 850 || u.Output != 40 {
		t.Errorf("got %+v", u)
	}
}

func TestParseSSEUsageNoneReported(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n"
	if _, ok := parseSSEUsage([]byte(stream)); ok {
		t.Error("reported usage for a stream that carried none")
	}
}

func TestSnifferKeepsHeadAndTailBounded(t *testing.T) {
	s := newSniffer(true)
	// A response far larger than the window, with the usage block at the very end.
	filler := strings.Repeat("x", sniffMax*3)
	s.write([]byte("HEAD-MARKER"))
	s.write([]byte(filler))
	s.write([]byte("TAIL-MARKER"))

	got := string(s.bytes())
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Error("head window lost")
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Error("tail window lost")
	}
	// Bounded: at most the two windows plus the separator.
	if len(got) > 2*sniffMax+1 {
		t.Errorf("sniffer retained %d bytes; the window must be bounded at %d", len(got), 2*sniffMax+1)
	}
}

func TestSnifferDisabledCostsNothing(t *testing.T) {
	s := newSniffer(false)
	s.write([]byte(strings.Repeat("x", 1<<20)))
	if got := s.bytes(); got != nil {
		t.Errorf("a disabled sniffer retained %d bytes", len(got))
	}
}

func TestSnifferSmallResponseReturnsItOnce(t *testing.T) {
	s := newSniffer(true)
	body := `{"usage":{"input_tokens":5,"output_tokens":2}}`
	s.write([]byte(body))
	got := string(s.bytes())
	if got != body {
		t.Errorf("small response mangled: %q", got)
	}
	// And it must still parse (no duplicated head+tail confusing the decoder).
	if u, ok := parseUsage(s.bytes()); !ok || u.FreshInput != 5 {
		t.Errorf("parse from sniffer = %+v ok=%v", u, ok)
	}
}

// TestSnifferSeparatorPreventsGluedSSELines guards a subtle failure: joining a
// truncated head to a tail without a newline could fuse two SSE `data:` lines into
// one unparseable line, silently losing usage for every large stream.
func TestSnifferSeparatorPreventsGluedSSELines(t *testing.T) {
	s := newSniffer(true)
	head := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":1,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":2}}}\n\n"
	s.write([]byte(head))
	s.write([]byte("data: " + strings.Repeat("{\"filler\":1}", sniffMax/6) + "\n\n"))
	s.write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":77}}\n\n"))

	u, ok := parseSSEUsage(s.bytes())
	if !ok {
		t.Fatal("usage lost across a windowed SSE stream")
	}
	if u.FreshInput != 9 || u.CacheRead != 100 {
		t.Errorf("head tiers lost: %+v", u)
	}
	if u.Output != 77 {
		t.Errorf("tail output lost: %+v", u)
	}
}

func TestResponseUsagePicksTheParserByContentType(t *testing.T) {
	json := []byte(`{"usage":{"input_tokens":3,"output_tokens":4}}`)
	if u, ok := responseUsage("application/json", json); !ok || u.FreshInput != 3 {
		t.Errorf("json path: %+v ok=%v", u, ok)
	}
	sse := []byte("data: {\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}\n\n")
	if u, ok := responseUsage("text/event-stream; charset=utf-8", sse); !ok || u.FreshInput != 3 {
		t.Errorf("sse path: %+v ok=%v", u, ok)
	}
	if _, ok := responseUsage("application/json", nil); ok {
		t.Error("empty body reported usage")
	}
}

// The per-TTL write breakdown. Without it a 1-hour write prices as a 5-minute one, which
// understates the row by 0.75x of its whole written prefix — and it is also the only signal
// that a requested ttl:"1h" was honoured rather than silently downgraded.
//
// Both fixtures are real responses from this gateway, one per verdict.
func TestUsageReadsTheOneHourWriteTier(t *testing.T) {
	granted := `{"usage":{"input_tokens":2,"output_tokens":32,"cache_read_input_tokens":0,
		"cache_creation_input_tokens":36574,
		"cache_creation":{"ephemeral_5m_input_tokens":323,"ephemeral_1h_input_tokens":36251}}}`
	refused := `{"usage":{"input_tokens":2,"output_tokens":32,"cache_read_input_tokens":0,
		"cache_creation_input_tokens":48212,
		"cache_creation":{"ephemeral_5m_input_tokens":48212,"ephemeral_1h_input_tokens":0}}}`

	u, ok := parseUsage([]byte(granted))
	if !ok {
		t.Fatal("granted fixture did not parse")
	}
	if u.CacheWrite != 36574 || u.CacheWrite1h != 36251 {
		t.Errorf("granted: write=%d write_1h=%d, want 36574/36251", u.CacheWrite, u.CacheWrite1h)
	}
	if u.CacheWrite1h > u.CacheWrite {
		t.Error("the 1h figure is a SUBSET of cache_write, never an addition to it")
	}
	u2, ok := parseUsage([]byte(refused))
	if !ok {
		t.Fatal("refused fixture did not parse")
	}
	if u2.CacheWrite != 48212 || u2.CacheWrite1h != 0 {
		t.Errorf("refused: write=%d write_1h=%d, want 48212/0", u2.CacheWrite, u2.CacheWrite1h)
	}
	// A response with no breakdown at all — every pre-existing fixture — must read as zero
	// rather than as missing usage.
	u3, ok := parseUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":1,
		"cache_creation_input_tokens":100}}`))
	if !ok || u3.CacheWrite != 100 || u3.CacheWrite1h != 0 {
		t.Errorf("no-breakdown response: ok=%v write=%d write_1h=%d", ok, u3.CacheWrite, u3.CacheWrite1h)
	}
	// And through the SSE path, where the tiers arrive in message_start. One LINE per event, so
	// the payload has to be compact — a `data:` field with an embedded newline is two events.
	sse := `event: message_start` + "\n" + `data: {"message":{"usage":{"input_tokens":2,` +
		`"cache_creation_input_tokens":36574,"cache_creation":` +
		`{"ephemeral_5m_input_tokens":323,"ephemeral_1h_input_tokens":36251}}}}` + "\n\n"
	u4, ok := parseSSEUsage([]byte(sse))
	if !ok || u4.CacheWrite1h != 36251 {
		t.Errorf("sse: ok=%v write_1h=%d, want 36251", ok, u4.CacheWrite1h)
	}
}
