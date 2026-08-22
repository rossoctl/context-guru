package proxy

import (
	"bufio"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
)

func peek(t *testing.T, stream string) ([]byte, sseVerdict) {
	t.Helper()
	head, v := peekSSE(bufio.NewReader(strings.NewReader(stream)), expand.ToolName)
	return head, v
}

const (
	pkStart  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n"
	pkText   = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	pkThink  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"
	pkDelta  = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
	pkStop   = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	pkPing   = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	pkErrEv  = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	pkOtherT = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{}}}\n\n"
)

func pkExpand() string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0," +
		"\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"" + expand.ToolName + "\",\"input\":{}}}\n\n"
}

// The verdict table. Only a response that OPENS with a call to the expand tool has to be
// withheld from the client; everything else can stream.
func TestPeekVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   sseVerdict
	}{
		{"opens with text — the common case, must stream", pkStart + pkText + pkDelta + pkStop, sseStreamable},
		{"opens with thinking — 100% of captured traffic has thinking on", pkStart + pkThink + pkDelta + pkStop, sseStreamable},
		{"opens with ANOTHER tool: the loop bails on otherTools anyway", pkStart + pkOtherT + pkStop, sseStreamable},
		{"opens with the expand call: this is what buffering exists for", pkStart + pkExpand() + pkStop, sseMustBuffer},
		{"pings before the first block do not decide anything", pkStart + pkPing + pkPing + pkText, sseStreamable},
		{"an empty message: no block at all, nothing to intercept", pkStart + pkStop, sseStreamable},
		{"an error event must reach the client as it arrived", pkStart + pkErrEv, sseStreamable},
		{"a stream that ends mid-preamble", pkStart, sseStreamable},
		{"an empty body", "", sseStreamable},
		{"a truncated final line", pkStart + "event: content_block_st", sseStreamable},
		{"not the Anthropic dialect at all", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", sseStreamable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := peek(t, tc.stream); got != tc.want {
				t.Fatalf("verdict %v, want %v", got, tc.want)
			}
		})
	}
}

// The peek must never eat input. Whatever it consumed is returned, and head + the
// remainder has to reconstitute the stream byte-for-byte — the client's response on the
// streaming path and the loop's input on the buffered one are both built from that pair.
func TestPeekReturnsEveryByteItConsumed(t *testing.T) {
	for _, stream := range []string{
		pkStart + pkText + pkDelta + pkStop,
		pkStart + pkExpand() + pkDelta + pkStop,
		pkStart + pkPing + pkThink + pkDelta + pkStop,
		pkStart,
		"",
		strings.Repeat(pkPing, 50) + pkText + pkStop,
	} {
		br := bufio.NewReader(strings.NewReader(stream))
		head, _ := peekSSE(br, expand.ToolName)
		rest := new(strings.Builder)
		if _, err := rest.Write(nil); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 512)
		for {
			n, err := br.Read(buf)
			rest.Write(buf[:n])
			if err != nil {
				break
			}
		}
		if got := string(head) + rest.String(); got != stream {
			t.Fatalf("peek lost or duplicated bytes:\n want %q\n got  %q", stream, got)
		}
	}
}

// The peek stops at the deciding event, not at the end of the stream — that is the whole
// point, and a peek that reads to EOF would be the old buffering wearing a new name.
func TestPeekStopsAtTheDecidingEvent(t *testing.T) {
	tail := strings.Repeat(pkDelta, 500)
	head, v := peek(t, pkStart+pkText+tail+pkStop)
	if v != sseStreamable {
		t.Fatalf("verdict %v", v)
	}
	if len(head) >= len(pkStart+pkText+tail) {
		t.Fatalf("peek read %d bytes; it must stop at the first content_block_start "+
			"(~%d bytes), not drain the stream", len(head), len(pkStart+pkText))
	}
	if !strings.Contains(string(head), "content_block_start") {
		t.Fatalf("the deciding event must be part of the flushed head, got %q", head)
	}
}

// An unreadable or absurdly long preamble falls back to buffering — the old behaviour.
// Fail open: a dialect we cannot read must not be streamed past an inspection that the
// reversibility loop is depending on.
func TestPeekFallsBackToBufferingOnAnOversizedPreamble(t *testing.T) {
	// Well-formed events that never decide, past the bound.
	junk := strings.Repeat(pkPing, ssePeekMaxBytes/len(pkPing)+10)
	if _, v := peek(t, junk+pkText); v != sseMustBuffer {
		t.Fatalf("verdict %v, want sseMustBuffer for a preamble past the bound", v)
	}
}

// The counter that keeps the trade-off honest rather than argued.
func TestSSENamesExpandTool(t *testing.T) {
	if !sseNamesExpandTool([]byte(pkExpand()), expand.ToolName) {
		t.Fatal("a chunk carrying the expand tool_use must be detected")
	}
	if sseNamesExpandTool([]byte(pkText+pkDelta), expand.ToolName) {
		t.Fatal("a plain text chunk must not be counted")
	}
}
