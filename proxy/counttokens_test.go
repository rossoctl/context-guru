// The count_tokens route: served at all, forwarded verbatim, errors passed through.
package proxy_test

import (
	"fmt"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCountTokensIsServed covers conformance item 5.
//
// Without this route the client gets a 404 and falls back to counting context by issuing
// INFERENCE requests — billed calls added by a proxy sold on removing them. The body must arrive
// unmodified (the client is asking about the context IT holds, and uses the answer to budget its
// own transcript), and the answer must come back verbatim.
func TestCountTokensIsServed(t *testing.T) {
	var rec recordedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens":4321}`)
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, cachePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := claudeCodeBody(t, false)
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages/count_tokens", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a 404 here sends the client back to counting with "+
			"inference requests): %s", resp.StatusCode, out)
	}
	gotPath, gotBody := rec.requestPath(), rec.forwarded()
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q, want /v1/messages/count_tokens", gotPath)
	}
	if string(gotBody) != string(body) {
		t.Errorf("the body was modified before counting. The client is asking about the context "+
			"IT holds and budgets its own transcript from the answer, so a count of a compacted "+
			"body would make it believe it has room it does not.\n got: %s\nwant: %s", gotBody, body)
	}
	if gjson.GetBytes(out, "input_tokens").Int() != 4321 {
		t.Errorf("response not relayed verbatim: %s", out)
	}
}

// TestCountTokensForwardsUpstreamErrors: same wording-preservation rule as the chat route. A
// client that cannot read the real error here has no way to tell a malformed request from an
// auth failure.
func TestCountTokensForwardsUpstreamErrors(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, errBody)
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, cachePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/anthropic/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || string(got) != errBody {
		t.Errorf("status/body not forwarded verbatim: %d %s", resp.StatusCode, got)
	}
}
