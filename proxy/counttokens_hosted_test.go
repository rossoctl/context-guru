package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// The hosted branch of the count_tokens route.
//
// This is here because the branch was shipped untested. `Mux()` has one caller, shared between
// the single-tenant binary and the hosted service, so the multi-tenant deployment serves this
// route — and the `h.opts.Tenants != nil` block is the only thing standing between it and an
// unmetered open forwarder that would send OUR credential upstream in place of the caller's.
// The conformance tests build a handler with `Tenants == nil`, so they exercise none of it.

// TestCountTokensHostedRequiresAuth: no token, no forwarding. The failure this prevents is not
// subtle — an unauthenticated POST that reaches the upstream is an open relay on somebody else's
// credential, and it would also be unmetered.
func TestCountTokensHostedRequiresAuth(t *testing.T) {
	f := newHostedFixture(t, "up1", "anthropic")
	const path = "/anthropic/v1/messages/count_tokens"

	if w := f.post(path, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("%s without a token = %d, want 401", path, w.Code)
	}
	if w := f.post(path, "cg_live_"+strings.Repeat("A", 26), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("%s with an unissued token = %d, want 401", path, w.Code)
	}
	// And nothing reached the upstream on either attempt.
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d unauthenticated request(s) reached the upstream", n)
	}
}

// TestCountTokensHostedForwardsWithTheTenantsCredential: an authenticated call resolves the
// TENANT's upstream and injects the configured key, exactly as the chat route does — and the
// caller's own context-guru token must not travel with it.
func TestCountTokensHostedForwardsWithTheTenantsCredential(t *testing.T) {
	f := newHostedFixture(t, "up1", "anthropic")
	_, tok := f.register(t, "user@ibm.com")

	w := f.post("/anthropic/v1/messages/count_tokens", tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated count_tokens = %d, want 200: %s", w.Code, w.Body.String())
	}
	up := f.lastUpstream(t)
	if got := up.URL.Path; got != countTokensPath {
		t.Errorf("upstream path = %q, want %q", got, countTokensPath)
	}
	// The server key is injected in the slot the Anthropic dialect uses...
	if got := up.Header.Get("x-api-key"); got != "real-upstream-secret" {
		t.Errorf("upstream x-api-key = %q, want the injected server key", got)
	}
	// ...and OUR token does not leak upstream. This is the same rule the catch-all documents:
	// the caller's header holds a context-guru credential, which must not leave the box.
	if got := up.Header.Get(TokenHeader); got != "" {
		t.Errorf("the context-guru token leaked upstream in %s: %q", TokenHeader, got)
	}
	if got := up.Header.Get("Authorization"); strings.Contains(got, tok) {
		t.Errorf("the context-guru token leaked upstream in Authorization: %q", got)
	}
}
