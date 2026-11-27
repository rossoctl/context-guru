package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Gateway conformance under the funnel's default preset.
//
// The local-distribution funnel puts context-guru on the wire in front of a stranger's Claude
// Code, so "it works" is not enough — it has to not break the client, and the ways it could are
// specific and documented in the gateway protocol reference. Each test below is one of them.
//
// Two of these are worse than an outage, because they make the demo read as NEGATIVE rather
// than broken:
//
//   - a buffered SSE response looks like the proxy made the model slow;
//   - a rejected `cache_control` marker makes Claude Code disable prompt caching for the rest
//     of the conversation, which switches off the exact thing being sold, silently.
//
// The preset under test is `conservative` throughout, the default. The shared Claude-Code-shaped fixtures are in ccbody_test.go, and the
// two items that were DEFECTS rather than confirmations — the expand-tool gate and the missing
// count_tokens route — are tested beside their fixes in expandgate_test.go and
// counttokens_test.go.
// TestConservativeForwardsAnUpstreamErrorByteForByte covers conformance item 4.
//
// Claude Code's capability-rejection recovery matches on the upstream's error WORDING. A gateway
// that wraps, re-encodes or summarises an error body breaks that recovery path — the client can
// no longer tell "your cache_control was refused" from any other 400, so instead of retrying
// without the capability it surfaces a failure. The status, the body and the content type all
// have to arrive exactly as the upstream wrote them.
func TestConservativeForwardsAnUpstreamErrorByteForByte(t *testing.T) {
	// A real Anthropic error shape, whitespace and key order included: this is what the
	// client's matching runs against, so the test compares bytes rather than parsed JSON.
	errBody := `{"type":"error","error":{"type":"invalid_request_error","message":"A maximum of 4 blocks with cache_control may be provided, but found 5."}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_upstream_123")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, errBody)
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, cachePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
		strings.NewReader(string(claudeCodeBody(t, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: a rewritten status breaks the client's retry logic",
			resp.StatusCode)
	}
	if string(got) != errBody {
		t.Errorf("the error body was modified.\n got: %s\nwant: %s\n"+
			"Claude Code matches on the upstream's own wording to decide whether to retry "+
			"without a capability; wrapping it disables that recovery.", got, errBody)
	}
	if resp.Header.Get("request-id") != "req_upstream_123" {
		t.Errorf("request-id header lost (%q): it is what support uses to find the call",
			resp.Header.Get("request-id"))
	}
}

// TestConservativeDoesNotBufferSSE covers conformance item 1.
//
// Claude Code aborts a stream that has been silent for 300s, and a gateway that buffers a whole
// response before relaying it stalls the client. context-guru does buffer SOME responses — the
// ones where the model opens by calling the expand tool — but a request with no tools declared and no marker
// does not get that tool injected, so the buffering path must be unreachable. This asserts that rather than
// assuming it: the client's first event has to arrive while the upstream is still writing later
// ones.
func TestConservativeDoesNotBufferSSE(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Hold the rest of the stream until the test has SEEN the first event. If the proxy
		// buffered, the read below would block here and the test fails on the deadline rather
		// than on a wrong byte — which is exactly the client-visible symptom.
		<-release
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, cachePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(claudeCodeBody(t, true))))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type read struct {
		line string
		err  error
	}
	ch := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		ch <- read{line, err}
	}()
	select {
	case r := <-ch:
		close(release)
		if r.err != nil {
			t.Fatalf("reading the first event: %v", r.err)
		}
		if !strings.Contains(r.line, "message_start") {
			t.Fatalf("first line was %q, want the upstream's first event", r.line)
		}
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("no event reached the client while the upstream was still streaming: the " +
			"response is being buffered. Claude Code aborts a stream silent for 300s, and a " +
			"stalled first byte reads as context-guru making the model slow.")
	}
}
