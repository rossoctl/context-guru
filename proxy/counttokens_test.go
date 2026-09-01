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
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
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

// TestCachePresetAdvertisesNoExtraTool is the test that should have existed when the `cache`
// preset shipped, and did not.
//
// The preset's promise — repeated in `config/config.go`, `docs/reference/presets.md`,
// `docs/how-to/choose-a-preset.md`, `docs/how-to/install-plugin.md` and the install skill — is
// that nothing is added to the user's requests. It was false: `expand.Inject` under `auto` gated
// only on "the request declares tools" and "the store persists", so a cachesplit-only pipeline
// forwarded `context_guru_expand` to the provider. Measured before the fix:
//
//	tools SENT by client    : [Read Bash]
//	tools FORWARDED upstream: [Read Bash context_guru_expand]
//
// That is not cosmetic. A pipeline with no Offload mints no markers, so EVERY expand call against
// it must fail — on a real session with marker-shaped text in a file, the model called it
// unprompted and got "[expand: original for id ... is no longer available]", costing a round trip
// and a step of the user's turn.
//
// TestCachePresetIsCachesplitAlone could not catch this: it asserts the presets MAP, while the
// injection happens in proxy.go after apply returns. This one reads the bytes that leave the
// process, which is the only place the claim is actually true or false.
