package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/metrics"
)

// The events of a round-1 response that calls expand from its SECOND block — the shape
// production actually produces, because a reasoning turn opens with thinking or text and
// only then calls a tool. leadType is "text" or "thinking": the bug is identical for both,
// which is why the pair is a table and not two hand-written streams.
func leadThenExpand(leadType string) (head, tail string) {
	lead := `{"type":"` + leadType + `","` + leadType + `":""}`
	deltaType := "text_delta"
	deltaField := "text"
	if leadType == "thinking" {
		deltaType, deltaField = "thinking_delta", "thinking"
	}
	head = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":" + lead + "}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"" + deltaType + "\",\"" + deltaField + "\":\"LEADING\"}}\n\n"
	if leadType == "thinking" {
		head += "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"SIG\"}}\n\n"
	}
	head += "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	tail = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"context_guru_expand\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HASH\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	return head, tail
}

const round2Answer = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ANSWERED\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// THE BUG, as a user reported it: the client received
//
//	context_guru_expand
//	IN  { "id": "..." }
//	OUT <tool_use_error>Error: No such tool available: context_guru_expand</tool_use_error>
//
// A tool_use naming OUR tool reached the client, which implements no such tool. It got
// there because the peek decided on the FIRST content_block_start and streamed everything
// that did not open with the expand call — and a tool_use is almost never the first block.
//
// The client must never see that block: it is ours to answer.
func TestExpandCalledAfterALeadingBlockIsNeverGivenToTheClient(t *testing.T) {
	for _, leadType := range []string{"text", "thinking"} {
		t.Run(leadType, func(t *testing.T) {
			var calls int
			var secondBody []byte
			head, tail := leadThenExpand(leadType)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				if calls == 1 {
					w.Write([]byte(head + tail))
					return
				}
				secondBody = b
				w.Write([]byte(round2Answer))
			}))
			defer upstream.Close()

			h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
			st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
				strings.NewReader(string(anthropicSSEBody(t, "look at <<cg:HASH>> and finish"))))
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if strings.Contains(string(out), "context_guru_expand") {
				t.Fatalf("the client received a raw tool_use for OUR tool — this is the "+
					"reported bug (`No such tool available: context_guru_expand`):\n%s", out)
			}
			if calls != 2 {
				t.Fatalf("the expand call must drive a continuation round, got %d upstream calls", calls)
			}
			if !strings.Contains(string(secondBody), "THE ORIGINAL CONTENT") {
				t.Fatalf("continuation must carry the resolved original: %s", secondBody)
			}
			// The client's turn has to be COMPLETE and well-formed: the model's leading
			// block, then the answer it gave once it had the content, in one message.
			for _, want := range []string{"LEADING", "ANSWERED", "message_stop"} {
				if !strings.Contains(string(out), want) {
					t.Fatalf("client stream lost %q:\n%s", want, out)
				}
			}
			if n := strings.Count(string(out), `"type":"message_start"`); n != 1 {
				t.Fatalf("a client sees ONE message per turn, got %d message_start events:\n%s", n, out)
			}
			if n := strings.Count(string(out), `"type":"message_stop"`); n != 1 {
				t.Fatalf("got %d message_stop events, want 1:\n%s", n, out)
			}
			// Round 2's block arrives as index 1: it follows the leading block the client
			// already has. An unremapped index 0 would collide with it.
			if !strings.Contains(string(out), `"index":1,"content_block":{"type":"text"`) {
				t.Fatalf("round 2's block must be renumbered after the streamed prefix:\n%s", out)
			}
			var snap metrics.Snapshot
			stx, _ := http.Get(srv.URL + "/stats")
			json.NewDecoder(stx.Body).Decode(&snap)
			stx.Body.Close()
			if snap.SSEExpandAfterStream != 0 {
				t.Fatalf("an intercepted expand call is not a leak: %+v", snap)
			}
		})
	}
}

// And the interception must not cost the stream: the blocks BEFORE the expand call are the
// model's reasoning and prose, most of the turn's tokens, and they are already on the wire
// when the tool call arrives. The upstream here holds the expand block back until the test
// confirms the client already has the leading text — a proxy that buffers to intercept
// cannot pass this, which is the whole difference between splicing and re-buffering.
func TestTheStreamedPrefixReachesTheClientBeforeTheExpandCall(t *testing.T) {
	release := make(chan struct{})
	var calls int
	head, tail := leadThenExpand("text")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls > 1 {
			w.Write([]byte(round2Answer))
			return
		}
		w.Write([]byte(head))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.Write([]byte(tail))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(string(anthropicSSEBody(t, "look at <<cg:HASH>> and finish"))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, rerr := resp.Body.Read(buf)
	first := string(buf[:n])
	close(release)
	if n == 0 {
		t.Fatalf("first read returned no bytes: %v", rerr)
	}
	if !strings.Contains(first, "LEADING") {
		t.Fatalf("the blocks before the expand call must stream while the upstream is still "+
			"generating; got %q", first)
	}
	rest, _ := io.ReadAll(resp.Body)
	whole := first + string(rest)
	if strings.Contains(whole, "context_guru_expand") {
		t.Fatalf("client received our own tool_use:\n%s", whole)
	}
	if !strings.Contains(whole, "ANSWERED") {
		t.Fatalf("the spliced continuation must finish the turn:\n%s", whole)
	}

	// A response whose prefix streamed is a STREAMED response: the client's first byte
	// arrived while the model was still generating. Filing it as buffered would report a
	// time-to-first-byte the client never experienced.
	var snap metrics.Snapshot
	stx, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stx.Body).Decode(&snap)
	stx.Body.Close()
	if snap.SSEStreamed != 1 || snap.SSEBuffered != 0 {
		t.Fatalf("want one streamed, zero buffered: %+v", snap)
	}
}
