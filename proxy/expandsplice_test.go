package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

	ch := sseChunks(resp.Body)
	first, ok := collectUntil(ch, "LEADING", 2*time.Second)
	close(release)
	if !ok {
		t.Fatalf("the blocks before the expand call must reach the client while the upstream "+
			"is still generating them; got %q", first)
	}
	whole := first + drain(ch)
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

// The paths the splice CANNOT close, closed on the request side. The model batched expand
// with a tool only the client owns: the proxy cannot answer half a batch, so the client does
// receive our tool_use and answers it the only way it can — `No such tool available`. That
// error must not reach the model; the content it asked for must.
//
// This is the whole sequence a user lives through, in two requests, because that is the only
// place it is visible: the leak on the first and the repair on the second.
func TestTheClientsNoSuchToolErrorNeverReachesTheModel(t *testing.T) {
	var lastUpstream []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUpstream, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"context_guru_expand\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"id\\\":\\\"HASH\\\"}\"}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"Bash\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"ls\\\"}\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	h, st := buildHandler(t, "pipeline: []\n", upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Turn 1: the batch the proxy declines to answer. The client gets the raw call — that is
	// the documented limit, and it is counted rather than hidden.
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(string(anthropicSSEBody(t, "look at <<cg:HASH>> then list files"))))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(out), "context_guru_expand") {
		t.Skip("the batch is now intercepted; this test's premise is gone and the repair " +
			"needs a different unanswerable path")
	}
	var snap metrics.Snapshot
	stx, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stx.Body).Decode(&snap)
	stx.Body.Close()
	if snap.SSEExpandAfterStream != 1 {
		t.Fatalf("handing a client our own tool_use must be counted: %+v", snap)
	}

	// Turn 2: what the client sends back. Claude Code's error, verbatim.
	follow := `{"model":"claude","stream":true,` +
		`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"look at <<cg:HASH>> then list files"},` +
		`{"role":"assistant","content":[{"type":"text","text":"on it"},` +
		`{"type":"tool_use","id":"toolu_1","name":"context_guru_expand","input":{"id":"HASH"}},` +
		`{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_1","content":"<tool_use_error>Error: No such tool available: context_guru_expand</tool_use_error>","is_error":true},` +
		`{"type":"tool_result","tool_use_id":"toolu_2","content":"file1 file2"}]}]}`
	resp2, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(follow))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if strings.Contains(string(lastUpstream), "No such tool available") {
		t.Fatalf("the model received the client's error for OUR tool:\n%s", lastUpstream)
	}
	if !strings.Contains(string(lastUpstream), "THE ORIGINAL CONTENT") {
		t.Fatalf("the model must receive the content it asked for:\n%s", lastUpstream)
	}
	// And the client's own tool result is untouched — it is not ours to rewrite.
	if !strings.Contains(string(lastUpstream), "file1 file2") {
		t.Fatalf("the client's own tool result was lost:\n%s", lastUpstream)
	}
}

// Once the prefix is on the wire, EVERY way this request can end has to end with a complete
// turn. A continuation round whose upstream call fails used to be a 502 — which was fine
// while the response was buffered and nothing had been written, and is garbage appended to
// the model's turn now that the prefix has already streamed.
func TestAFailedContinuationRoundStillEndsTheClientsTurn(t *testing.T) {
	// atomic, unlike the counters in the tests above: the second round's connection is closed
	// without a response, so there is no happens-before edge between the handler's write and
	// the assertion's read.
	var calls atomic.Int64
	head, tail := leadThenExpand("text")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) > 1 {
			// Kill the connection without a response: doUpstream returns an error.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(head + tail))
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

	if n := calls.Load(); n != 2 {
		t.Fatalf("expected the continuation to be attempted, got %d upstream calls", n)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream was already open with its own status, got %d", resp.StatusCode)
	}
	if strings.Contains(string(out), "upstream request failed") {
		t.Fatalf("a 502 body was appended to an open event stream:\n%s", out)
	}
	// The model's own call comes back, as on every other path the loop cannot finish, and the
	// turn terminates.
	for _, want := range []string{"LEADING", `"type":"message_stop"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the client's turn is incomplete, missing %q:\n%s", want, out)
		}
	}
	var snap metrics.Snapshot
	stx, _ := http.Get(srv.URL + "/stats")
	json.NewDecoder(stx.Body).Decode(&snap)
	stx.Body.Close()
	if snap.SSEExpandAfterStream != 1 {
		t.Fatalf("handing the client our own tool_use must be counted here too: %+v", snap)
	}
}

// A continuation round that answers with JSON instead of an event stream — the request said
// stream: true, so this is an upstream anomaly — cannot be spliced into the open stream. The
// client's turn must still terminate: it gets the events the splice withheld, which is a
// complete message, rather than a stream cut off mid-block.
func TestAJSONContinuationRoundStillEndsTheClientsTurn(t *testing.T) {
	var calls int
	head, tail := leadThenExpand("text")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls++
		if calls > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ANSWERED"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(head + tail))
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

	if calls != 2 {
		t.Fatalf("expected the continuation round, got %d upstream calls", calls)
	}
	if strings.Contains(string(out), `{"type":"message","role":"assistant"`) {
		t.Fatalf("a JSON body was appended to an open event stream:\n%s", out)
	}
	for _, want := range []string{"LEADING", `"type":"message_stop"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the client's turn is incomplete, missing %q:\n%s", want, out)
		}
	}
}
