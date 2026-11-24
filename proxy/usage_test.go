package proxy

import (
	"strings"
	"testing"
)

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
	if u, ok := responseUsage("application/json", s.bytes()); !ok || u.FreshInput != 5 {
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

	u, ok := responseUsage("text/event-stream", s.bytes())
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
