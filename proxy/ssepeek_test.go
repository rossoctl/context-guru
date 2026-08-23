package proxy

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
)

const (
	pkStart  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n"
	pkText   = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	pkThink  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"
	pkDelta  = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
	pkBStop  = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	pkStop   = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	pkPing   = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	pkErrEv  = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	pkOtherT = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{}}}\n\n"
)

func pkExpand() string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1," +
		"\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"" + expand.ToolName + "\",\"input\":{}}}\n\n"
}

// The one event a client must never receive. Everything else — thinking, text, another
// tool, a ping, an error — is the client's to have, and the block INDEX is irrelevant:
// deciding by index is the bug this replaced.
func TestStartsExpandCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   string
		want bool
	}{
		{"the expand call itself", pkExpand(), true},
		{"a text block", pkText, false},
		{"a thinking block", pkThink, false},
		{"another tool the client owns", pkOtherT, false},
		{"a delta", pkDelta, false},
		{"message_start", pkStart, false},
		{"message_stop", pkStop, false},
		{"a ping", pkPing, false},
		{"an error event", pkErrEv, false},
		{"an OpenAI-shaped chunk", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", false},
		{"[DONE]", "data: [DONE]\n\n", false},
		{"no data line at all", "event: content_block_start\n\n", false},
	} {
		if got := startsExpandCall([]byte(tc.ev), expand.ToolName); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// pass must never eat or duplicate input: what it forwarded plus what it withheld has to
// reconstitute the stream byte-for-byte, because the client's response is built from one
// half and the continuation loop's input from both.
func TestPassSplitsTheStreamWithoutLosingAByte(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stream     string
		wantFound  bool
		wantClient string // "" means "everything"
	}{
		{"a plain text answer streams whole", pkStart + pkText + pkDelta + pkBStop + pkStop, false, ""},
		{"an expand call after a text block is withheld from it on",
			pkStart + pkText + pkDelta + pkBStop + pkExpand() + pkDelta + pkStop, true,
			pkStart + pkText + pkDelta + pkBStop},
		{"an expand call after thinking, likewise",
			pkStart + pkThink + pkDelta + pkBStop + pkExpand() + pkStop, true,
			pkStart + pkThink + pkDelta + pkBStop},
		{"an expand call in the FIRST block: the client gets the preamble only",
			pkStart + pkExpand() + pkStop, true, pkStart},
		{"another tool is not ours to withhold", pkStart + pkOtherT + pkStop, false, ""},
		{"pings and errors pass through", pkStart + pkPing + pkErrEv, false, ""},
		{"an empty body", "", false, ""},
		{"a truncated final event", pkStart + "event: content_block_st", false, ""},
		{"CRLF framing", strings.ReplaceAll(pkStart+pkText+pkExpand(), "\n", "\r\n"), true,
			strings.ReplaceAll(pkStart+pkText, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			sp := newSSESplicer(rec)
			sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
			whole, withheld, found := sp.pass(strings.NewReader(tc.stream), expand.ToolName)
			if found != tc.wantFound {
				t.Fatalf("found=%v, want %v", found, tc.wantFound)
			}
			if string(whole) != tc.stream {
				t.Fatalf("whole must be the stream as it arrived:\n want %q\n got  %q", tc.stream, whole)
			}
			wantClient := tc.wantClient
			if wantClient == "" {
				wantClient = tc.stream
			}
			if got := rec.Body.String(); got != wantClient {
				t.Fatalf("client got:\n %q\n want %q", got, wantClient)
			}
			if got := wantClient + string(withheld); got != tc.stream {
				t.Fatalf("forwarded + withheld must be the whole stream:\n want %q\n got  %q", tc.stream, got)
			}
			if found && bytes.Contains([]byte(rec.Body.String()), []byte(expand.ToolName)) {
				t.Fatalf("the client received our own tool_use: %q", rec.Body.String())
			}
		})
	}
}

// The splice: round 2's blocks are renumbered to follow the ones the client already has,
// its message_start is dropped (a client sees ONE message per turn), and round 1's own
// events are passed through untouched — byte-for-byte, which is what keeps a response that
// never calls expand an exact pass-through.
func TestSpliceRenumbersTheContinuationAndKeepsOneMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	sp := newSSESplicer(rec)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}

	sp.round(resp)
	round1 := pkStart + pkText + pkDelta + pkBStop + pkExpand() + pkStop
	_, withheld, found := sp.pass(strings.NewReader(round1), expand.ToolName)
	if !found || sp.blocks != 1 {
		t.Fatalf("round 1 should have forwarded exactly one block, found=%v blocks=%d", found, sp.blocks)
	}
	prefix := rec.Body.String()
	if prefix != pkStart+pkText+pkDelta+pkBStop {
		t.Fatalf("round 1 prefix must be verbatim: %q", prefix)
	}

	sp.round(resp)
	round2 := pkStart + pkText + pkDelta + pkBStop + pkStop
	if _, _, found := sp.pass(strings.NewReader(round2), expand.ToolName); found {
		t.Fatal("round 2 has no expand call")
	}
	out := rec.Body.String()
	spliced := strings.TrimPrefix(out, prefix)
	if spliced == out {
		t.Fatalf("the prefix must still be intact at the front: %q", out)
	}
	if strings.Contains(spliced, "message_start") {
		t.Fatalf("a second message_start must be dropped: %q", spliced)
	}
	for _, want := range []string{
		`"type":"content_block_start","index":1`,
		`"type":"content_block_delta","index":1`,
		`"type":"content_block_stop","index":1`,
	} {
		if !strings.Contains(spliced, want) {
			t.Fatalf("round 2's block must be renumbered to 1, missing %s:\n%q", want, spliced)
		}
	}
	if strings.Count(out, `"type":"message_stop"`) != 1 {
		t.Fatalf("the client's turn must end exactly once: %q", out)
	}
	if sp.blocks != 2 {
		t.Fatalf("the client holds 2 blocks, splicer says %d", sp.blocks)
	}
	if _, withheldAgain, _ := sp.pass(strings.NewReader(""), expand.ToolName); withheldAgain != nil {
		t.Fatal("an empty round withholds nothing")
	}
	_ = withheld
}

// handBack is the fail-open path: whatever the splice withheld goes to the client exactly
// as it arrived when the loop cannot answer it. On round 1 that is byte-for-byte the old
// pass-through, which is the behaviour every "replays verbatim" test depends on.
func TestHandBackReplaysTheWithheldEventsVerbatim(t *testing.T) {
	rec := httptest.NewRecorder()
	sp := newSSESplicer(rec)
	sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
	stream := pkStart + pkText + pkDelta + pkBStop + pkExpand() + pkOtherT + pkStop
	_, withheld, _ := sp.pass(strings.NewReader(stream), expand.ToolName)
	sp.handBack(withheld)
	if got := rec.Body.String(); got != stream {
		t.Fatalf("prefix + handBack must reconstitute the stream:\n want %q\n got  %q", stream, got)
	}
}

// readSSEEvent frames on the blank line and hands back the bytes it read, unmodified,
// including a final event the upstream never terminated.
func TestReadSSEEventFramesWholeEventsAndKeepsAPartialTail(t *testing.T) {
	stream := pkStart + pkPing + "event: content_block_st"
	br := bufio.NewReader(strings.NewReader(stream))
	var got []string
	var buf bytes.Buffer
	for {
		ev, err := readSSEEvent(br, &buf)
		if len(ev) > 0 {
			got = append(got, string(ev)) // copied: ev is only valid until the next read
		}
		if err != nil {
			break
		}
	}
	want := []string{pkStart, pkPing, "event: content_block_st"}
	if strings.Join(got, "") != stream {
		t.Fatalf("events must reconstitute the stream: %q", got)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// bigStream builds a round of at least n bytes, with the expand call placed before or after
// the retain bound so both orderings can be driven.
func bigStream(n int, expandFirst bool) string {
	var sb strings.Builder
	sb.WriteString(pkStart)
	sb.WriteString(pkText)
	if expandFirst {
		sb.WriteString(pkExpand())
	}
	for sb.Len() < n {
		sb.WriteString(pkDelta)
	}
	if !expandFirst {
		sb.WriteString(pkExpand())
	}
	sb.WriteString(pkStop)
	return sb.String()
}

// Past sseRetainBoundMaxBytes the turn cannot be rebuilt, so nothing is intercepted and
// NOTHING IS DROPPED: the client gets the stream exactly as it arrived, expand call included,
// and pass says so with whole=nil and found=true so the caller can count the leak.
//
// The bound is a per-ROUND ceiling and AggregateSSE's 8 MB is a per-LINE one, so a round past
// this bound is not necessarily a round the aggregator would have refused — a 20 MiB round of
// short events parses fine today. The bound wins anyway, deliberately: it exists to stop one
// pathological stream being retained in full, which is what the old peek's 64 KB bail did.
func TestARoundPastTheRetainBoundIsForwardedWholeAndNothingIsWithheld(t *testing.T) {
	for _, tc := range []struct {
		name        string
		expandFirst bool
	}{
		{"expand call past the bound", false},
		{"expand call BEFORE the bound, so withheld events must be handed back", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := bigStream(20<<20, tc.expandFirst)
			if len(stream) <= sseRetainMaxBytes {
				t.Fatalf("fixture is %d B, must exceed the %d B bound", len(stream), sseRetainMaxBytes)
			}
			rec := httptest.NewRecorder()
			sp := newSSESplicer(rec)
			sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
			whole, withheld, found := sp.pass(strings.NewReader(stream), expand.ToolName)
			if whole != nil {
				t.Fatalf("past the bound the round must not be retained, got %d B", len(whole))
			}
			if withheld != nil {
				t.Fatalf("past the bound nothing can be withheld, got %d B", len(withheld))
			}
			if !found {
				t.Fatal("the response called expand and the caller has to know, to count the leak")
			}
			if got := rec.Body.String(); got != stream {
				t.Fatalf("the client must receive the stream byte-for-byte: got %d B, want %d B",
					len(got), len(stream))
			}
		})
	}
}

// And a round UNDER the bound is unaffected — the bound must not change the behaviour of any
// response a model can actually produce. This gateway allows max_tokens up to 128,000, which
// at the measured ~35 B/token is ~4.5 MB.
func TestARoundUnderTheRetainBoundIsStillIntercepted(t *testing.T) {
	stream := bigStream(4500000, false) // 128,000 output tokens at the measured ~35 B/token
	if len(stream) >= sseRetainMaxBytes {
		t.Fatalf("fixture %d B must stay under the %d B bound", len(stream), sseRetainMaxBytes)
	}
	rec := httptest.NewRecorder()
	sp := newSSESplicer(rec)
	sp.round(&http.Response{StatusCode: 200, Header: http.Header{}})
	whole, withheld, found := sp.pass(strings.NewReader(stream), expand.ToolName)
	if !found || withheld == nil || string(whole) != stream {
		t.Fatalf("a full-length 4.5 MB round must still be retained and withheld: found=%v withheld=%d whole=%d",
			found, len(withheld), len(whole))
	}
	if strings.Contains(rec.Body.String(), expand.ToolName) {
		t.Fatal("the client received our own tool_use")
	}
}
