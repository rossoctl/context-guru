package proxy_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
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
// The preset under test is `cache` (cachesplit alone) throughout, because that is what an
// evaluator actually runs. The shared Claude-Code-shaped fixtures are in ccbody_test.go, and the
// two items that were DEFECTS rather than confirmations — the expand-tool gate and the missing
// count_tokens route — are tested beside their fixes in expandgate_test.go and
// counttokens_test.go.
// TestCachePresetForwardsAnUpstreamErrorByteForByte covers conformance item 4.
//
// Claude Code's capability-rejection recovery matches on the upstream's error WORDING. A gateway
// that wraps, re-encodes or summarises an error body breaks that recovery path — the client can
// no longer tell "your cache_control was refused" from any other 400, so instead of retrying
// without the capability it surfaces a failure. The status, the body and the content type all
// have to arrive exactly as the upstream wrote them.
func TestCachePresetForwardsAnUpstreamErrorByteForByte(t *testing.T) {
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

// TestCachePresetDoesNotBufferSSE covers conformance item 1.
//
// Claude Code aborts a stream that has been silent for 300s, and a gateway that buffers a whole
// response before relaying it stalls the client. context-guru does buffer SOME responses — the
// ones where the model opens by calling the expand tool — but under the `cache` preset nothing
// injects that tool, so the buffering path must be unreachable. This asserts that rather than
// assuming it: the client's first event has to arrive while the upstream is still writing later
// ones.
func TestCachePresetDoesNotBufferSSE(t *testing.T) {
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
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("no event reached the client while the upstream was still streaming: the " +
			"response is being buffered. Claude Code aborts a stream silent for 300s, and a " +
			"stalled first byte reads as context-guru making the model slow.")
	}
}

// TestCachePresetNeverAddsACacheControlBreakpoint covers conformance item 2, which is the
// strongest argument for shipping `cache` rather than a placement preset.
//
// The provider caps `cache_control` markers at 4. Exceed it and the request is REJECTED — and
// Claude Code's reaction to a rejected capability is to retry without it and leave prompt
// caching OFF for the rest of the conversation. So a breakpoint-budget mistake is not an error
// the user sees; it silently switches off the thing this whole funnel is selling, and the demo
// reads as "context-guru made my session more expensive".
//
// cachesplit MOVES a breakpoint onto the stable half of a block it splits; it must never add
// one. The body below arrives at the cap, so any addition at all is a 400.
func TestCachePresetNeverAddsACacheControlBreakpoint(t *testing.T) {
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","usage":{"input_tokens":1}}`)
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, cachePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Four inbound breakpoints — the provider's cap — spread the way a real client spreads
	// them: two system blocks, one tool, one message. Assembled as text, for the key-order
	// reason documented on claudeCodeBody.
	bp := `,"cache_control":{"type":"ephemeral"}`
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":64,"system":[` +
		`{"type":"text","text":` + jsonStr(attributionText) + bp + `},` +
		`{"type":"text","text":` + jsonStr(volatileSystemText()) + bp + `}` +
		`],"tools":[{"name":"read_file","input_schema":{"type":"object"}` + bp + `}` +
		`],"messages":[{"role":"user","content":[{"type":"text","text":"hello"` + bp + `}]}]}`)
	if !json.Valid(body) {
		t.Fatalf("test fixture is not valid JSON: %s", body)
	}

	inbound := countBreakpoints(body)
	if inbound != 4 {
		t.Fatalf("test setup is wrong: the request carries %d breakpoints, not the cap of 4", inbound)
	}
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	forwarded := up.body(1)
	if len(forwarded) == 0 {
		t.Fatal("upstream received nothing")
	}
	// The precondition that stops this being a vacuous pass: the component under test has to
	// have ACTED. If cachesplit did not split, "breakpoints unchanged" is trivially true and
	// asserts nothing about the rewrite.
	if n := len(gjson.GetBytes(forwarded, "system").Array()); n != 3 {
		t.Fatalf("cachesplit did not split the volatile tail (system has %d blocks, want 3): "+
			"the breakpoint assertion below would be vacuous", n)
	}
	if out := countBreakpoints(forwarded); out != inbound {
		t.Errorf("breakpoints on the wire = %d, inbound = %d (cap 4). Exceeding the cap is a "+
			"400, and Claude Code answers a rejected cache_control by disabling prompt caching "+
			"for the rest of the conversation — silently switching off what this preset exists "+
			"to demonstrate.\nforwarded: %s", out, inbound, forwarded)
	}
}

// countBreakpoints counts cache_control/cachePoint markers anywhere in the body. Both spellings,
// because Bedrock/Vertex write `cachePoint` where Anthropic writes `cache_control`, and the
// provider's cap counts whatever arrives.
func countBreakpoints(body []byte) int {
	n := 0
	var walk func(gjson.Result)
	walk = func(v gjson.Result) {
		v.ForEach(func(k, val gjson.Result) bool {
			if k.String() == "cache_control" || k.String() == "cachePoint" {
				n++
			}
			if val.IsObject() || val.IsArray() {
				walk(val)
			}
			return true
		})
	}
	walk(gjson.ParseBytes(body))
	return n
}

// TestCachePresetLeavesTheAttributionBlockUntouched covers conformance item 3.
//
// Claude Code prepends an attribution block as the FIRST system block, and the API strips it
// only if that array arrives unchanged. cachesplit reshapes the system array, so the question is
// whether the first block survives byte-identically.
//
// Three separate properties keep it safe, and the second is the one a plausible change would
// break, so both are exercised below:
//
//  1. the attribution block carries no volatile marker, so it is not a split candidate;
//  2. blocks the split does not act on are re-emitted from their ORIGINAL raw bytes rather than
//     re-encoded — re-marshalling would reorder keys and change the bytes even with identical
//     content, which is enough to defeat a positional strip;
//  3. the split's minSplitTokens floor (1024) excludes a small block even when it does contain a
//     marker — the second case below, where a user's own prompt happens to mention one.
//
// Proving this rather than reasoning about it is what makes the alternative — shipping
// CLAUDE_CODE_ATTRIBUTION_HEADER=0 in the installer — unnecessary, and keeps it unnecessary.
func TestCachePresetLeavesTheAttributionBlockUntouched(t *testing.T) {
	for _, c := range []struct{ name, first string }{
		{"the ordinary attribution block", attributionText},
		// Small, but it mentions something the split looks for. Only the token floor keeps this
		// out of the rewrite; without it the FIRST eligible block is the one that gets split,
		// and that is this one.
		{"a small first block that happens to name a volatile marker",
			attributionText + "\nCurrent branch: whatever the user was talking about\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var up upstreamCapture
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				up.record(r)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"type":"message","usage":{"input_tokens":1}}`)
			}))
			defer upstream.Close()

			h, _ := buildHandler(t, cachePipeline, upstream.URL)
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			body := claudeCodeBodyWithFirst(t, false, c.first)
			want := gjson.GetBytes(body, "system.0").Raw

			resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json",
				strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			forwarded := up.body(1)
			if len(forwarded) == 0 {
				t.Fatal("upstream received nothing")
			}
			// Precondition: cachesplit must actually have rewritten the array, or "the first
			// block is unchanged" is true because nothing happened. It must also have split the
			// SECOND block, not the first — 3 blocks with the attribution intact is the only
			// shape that means that.
			blocks := gjson.GetBytes(forwarded, "system").Array()
			if len(blocks) != 3 {
				t.Fatalf("cachesplit did not act (system has %d blocks, want 3); the assertion "+
					"below would be vacuous", len(blocks))
			}
			if got := blocks[0].Raw; got != want {
				t.Errorf("the attribution block changed, so the API will no longer strip it "+
					"positionally.\n got: %s\nwant: %s", got, want)
			}
			if !strings.HasPrefix(blocks[0].Get("text").String(), attributionText) {
				t.Errorf("the attribution block is no longer the first system block: %s", blocks[0].Raw)
			}
		})
	}
}
