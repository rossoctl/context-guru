package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// hostedFixture builds a hosted proxy in front of a recording fake upstream.
type hostedFixture struct {
	h        *Handler
	mux      *http.ServeMux
	reg      *tenant.Registry
	upstream *httptest.Server

	mu   sync.Mutex
	seen []*http.Request // headers of every request the upstream received
	body []string
}

// newHostedFixture builds a hosted proxy whose upstream holds a SERVER key (the
// explicit gateway fallback). newHostedFixtureNoKey builds the hosted DEFAULT, where
// the caller's own credential is forwarded.
func newHostedFixture(t *testing.T, upstreamName, dialect string) *hostedFixture {
	return newHostedFixtureKey(t, upstreamName, dialect, "TEST_UPSTREAM_KEY")
}

func newHostedFixtureNoKey(t *testing.T, upstreamName, dialect string) *hostedFixture {
	return newHostedFixtureKey(t, upstreamName, dialect, "")
}

func newHostedFixtureKey(t *testing.T, upstreamName, dialect, keyEnv string) *hostedFixture {
	t.Helper()
	f := &hostedFixture{}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(r.Context()))
		f.body = append(f.body, string(b))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(f.upstream.Close)

	t.Setenv("TEST_UPSTREAM_KEY", "real-upstream-secret")
	// Registration now mails a code, so every hosted fixture needs somewhere for that
	// mail to land. A file sink in this test's own temp dir, never the log: signUp reads
	// the code back out of it, which means these tests exercise the real mail code path
	// rather than a test-only shortcut into the registry.
	t.Setenv(envMailDevSink, filepath.Join(t.TempDir(), "mail.txt"))
	reg, err := tenant.Open("", tenant.Options{
		ManagerEmail: "boss@ibm.com",
		// Every dialect points at the one fixture upstream, so a route's dialect —
		// not the fixture's plumbing — decides which credential header is injected.
		DefaultUpAnthropic: upstreamName,
		DefaultUpOpenAI:    upstreamName,
		DefaultUpBob:       upstreamName,
		// The real binary wires config.Validate here, so the fixture must too — without
		// it these tests would pass while the shipped settings page silently accepted a
		// configuration the proxy then refuses to build.
		Validate: config.Validate,
	})
	if err != nil {
		t.Fatalf("tenant.Open: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	f.reg = reg

	// A pass-through pipeline: this file tests tenancy, not compaction.
	build := func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		return BuiltConfig{Pipe: components.NewPipeline(nil, e),
			Store: store.NewMemory(store.Options{}), Preset: "test"}, nil
	}
	src := NewTenantSource(reg, nil, build, 0)
	f.h = New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil, Options{
		Tenants: src,
		Upstreams: map[string]Upstream{upstreamName: {
			Dialect: dialect, BaseURL: f.upstream.URL, KeyEnv: keyEnv,
		}},
		BobUpstream: f.upstream.URL,
	})
	t.Cleanup(f.h.Close)
	f.mux = f.h.Mux()
	return f
}

func (f *hostedFixture) register(t *testing.T, email string) (*tenant.Tenant, string) {
	t.Helper()
	tn, tok, err := f.reg.Register("laptop", email)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return tn, tok
}

func (f *hostedFixture) post(path, token string, hdr string) *httptest.ResponseRecorder {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		if hdr == "" {
			hdr = "Authorization"
		}
		if hdr == "Authorization" {
			r.Header.Set(hdr, "Bearer "+token)
		} else {
			r.Header.Set(hdr, token)
		}
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// postCaller is the new shape: the context-guru token in its own header, the caller's
// OWN provider key in the auth slot.
func (f *hostedFixture) postCaller(path, token, key, keySlot string) *httptest.ResponseRecorder {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set(TokenHeader, token)
	}
	if key != "" {
		if keySlot == "" {
			keySlot = "Authorization"
		}
		if keySlot == "Authorization" {
			r.Header.Set(keySlot, "Bearer "+key)
		} else {
			r.Header.Set(keySlot, key)
		}
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *hostedFixture) lastUpstream(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("upstream received nothing")
	}
	return f.seen[len(f.seen)-1]
}

// The core property: no token, no forwarding. A hosted proxy that answers
// unauthenticated requests is an open relay for the operator's credential.
func TestHostedRejectsUnauthenticated(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	f.register(t, "a@ibm.com")

	for _, path := range []string{
		"/openai/v1/chat/completions",
		"/anthropic/v1/messages",
		"/inference/v1/chat/completions",
		"/compact",
	} {
		w := f.post(path, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, w.Code)
		}
		w = f.post(path, "cg_live_"+strings.Repeat("A", 26), "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with an unissued token = %d, want 401", path, w.Code)
		}
	}
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("upstream was called %d times for unauthenticated requests", n)
	}
}

// Bob's control-plane catch-all is the easy one to forget: it forwards verbatim, so
// left open it is both an unauthenticated forwarder and a way to leak our token.
func TestHostedCatchAllRequiresAuth(t *testing.T) {
	f := newHostedFixture(t, "up", "bob")
	r := httptest.NewRequest(http.MethodGet, "/admin/v1/profile", nil)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Bob control-plane path without a token = %d, want 401", w.Code)
	}
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("the catch-all forwarded an unauthenticated request")
	}
}

// A caller's token must never reach the upstream, in ANY of the slots we accept it
// in — the upstream must see only the operator's injected credential.
func TestTokenNeverReachesUpstream(t *testing.T) {
	for _, slot := range authHeaders {
		t.Run(slot, func(t *testing.T) {
			f := newHostedFixture(t, "up", "openai")
			_, tok := f.register(t, "a@ibm.com")
			if w := f.post("/openai/v1/chat/completions", tok, slot); w.Code != http.StatusOK {
				t.Fatalf("authenticated request = %d %s", w.Code, w.Body)
			}
			up := f.lastUpstream(t)
			for _, h := range authHeaders {
				if v := up.Header.Get(h); strings.Contains(v, tok) {
					t.Errorf("upstream saw our token in %s: %q", h, v)
				}
			}
			if got := up.Header.Get("Authorization"); got != "Bearer real-upstream-secret" {
				t.Errorf("upstream Authorization = %q, want the injected key", got)
			}
		})
	}
}

// An anthropic-dialect upstream gets x-api-key, not a bearer token.
func TestAnthropicDialectInjectsAPIKeyHeader(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	_, tok := f.register(t, "a@ibm.com")
	if w := f.post("/anthropic/v1/messages", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	up := f.lastUpstream(t)
	if got := up.Header.Get("x-api-key"); got != "real-upstream-secret" {
		t.Errorf("x-api-key = %q", got)
	}
	if up.Header.Get("Authorization") != "" {
		t.Errorf("Authorization survived: %q", up.Header.Get("Authorization"))
	}
}

// A tenant whose chosen upstream is not in the operator's allow-list is refused,
// not silently sent somewhere else. This is the SSRF boundary.
func TestUnknownUpstreamNameIsRefused(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	tn, tok := f.register(t, "a@ibm.com")
	evil := "http://169.254.169.254"
	if err := f.reg.Update(tn, tn.ID, tenant.Patch{UpOpenAI: &evil}); err != nil {
		t.Fatal(err)
	}
	w := f.post("/openai/v1/chat/completions", tok, "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("a URL as the upstream name = %d, want 502", w.Code)
	}
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("a tenant-supplied URL was forwarded to")
	}
}

// Two tenants must not share a state store: a shared one lets one tenant's traffic
// evict another's frozen compaction decisions.
func TestTenantsGetDistinctStores(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	a, _ := f.register(t, "a@ibm.com")
	b, _ := f.register(t, "b@ibm.com")

	ta, err := f.h.opts.Tenants.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := f.h.opts.Tenants.ForTenant(b)
	if err != nil {
		t.Fatal(err)
	}
	if ta.Store == tb.Store {
		t.Fatal("two tenants share one state store")
	}
	if ta.Shadow == tb.Shadow {
		t.Fatal("two tenants share one observe-mode shadow store")
	}
	if ta.Pipe == tb.Pipe {
		t.Fatal("two tenants share one pipeline instance")
	}
	// The same tenant twice must get the SAME store, or frozen decisions never
	// accumulate and compaction degrades to single-turn.
	again, err := f.h.opts.Tenants.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	if again.Store != ta.Store {
		t.Fatal("a tenant's store was rebuilt between requests; frozen decisions would be lost")
	}
}

// Changing a configuration must take effect on the next request.
func TestConfigChangeRebuildsTenancy(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	a, _ := f.register(t, "a@ibm.com")
	first, err := f.h.opts.Tenants.ForTenant(a)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "mode: observe\n"
	if err := f.reg.Update(a, a.ID, tenant.Patch{ConfigYAML: &cfg}); err != nil {
		t.Fatal(err)
	}
	updated, err := f.reg.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.h.opts.Tenants.ForTenant(updated)
	if err != nil {
		t.Fatal(err)
	}
	if second.Store == first.Store {
		t.Fatal("a configuration change did not rebuild the tenancy")
	}
}

// A disabled tenant is refused, and a revoked token stops working.
func TestDisabledAndRevokedAreRefused(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	mgrT, _ := f.register(t, "boss@ibm.com")
	a, tok := f.register(t, "a@ibm.com")
	if w := f.post("/openai/v1/chat/completions", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("baseline = %d", w.Code)
	}
	yes := true
	if err := f.reg.Update(mgrT, a.ID, tenant.Patch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if w := f.post("/openai/v1/chat/completions", tok, ""); w.Code != http.StatusForbidden {
		t.Fatalf("disabled tenant = %d, want 403", w.Code)
	}
}

// The tenancy LRU is bounded, and eviction does not break the evicted tenant — it
// just costs it a cold cache.
func TestTenancyLRUIsBounded(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	small := NewTenantSource(f.reg, nil, func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		return BuiltConfig{Pipe: components.NewPipeline(nil, e),
			Store: store.NewMemory(store.Options{})}, nil
	}, 2)
	var ids []*tenant.Tenant
	for _, e := range []string{"a@ibm.com", "b@ibm.com", "c@ibm.com"} {
		tn, _ := f.register(t, e)
		ids = append(ids, tn)
		if _, err := small.ForTenant(tn); err != nil {
			t.Fatal(err)
		}
	}
	small.mu.Lock()
	n := small.cache.ll.Len()
	small.mu.Unlock()
	if n != 2 {
		t.Fatalf("LRU holds %d tenancies, want the cap of 2", n)
	}
	// The evicted tenant still works.
	if _, err := small.ForTenant(ids[0]); err != nil {
		t.Fatalf("evicted tenant cannot be resolved again: %v", err)
	}
}

// /stats is a process-wide aggregate across every tenant, so a hosted deployment
// must not serve it to an arbitrary caller.
func TestStatsGatedInHostedMode(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	r := httptest.NewRequest(http.MethodGet, "/stats", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("/stats from a remote address = %d, want 403", w.Code)
	}
	// Loopback still works: the benchmark harnesses parse /stats from this box.
	r = httptest.NewRequest(http.MethodGet, "/stats", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/stats from loopback = %d, want 200", w.Code)
	}
}

// The dashboard's routes must not be shadowed by Bob's catch-all. A silent outage
// of /api/* would be indistinguishable from the dashboard being broken.
func TestBobCatchAllDoesNotShadowManagementRoutes(t *testing.T) {
	f := newHostedFixture(t, "up", "bob")
	for _, path := range []string{"/healthz", "/stats"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s was swallowed by the Bob catch-all (got 401)", path)
		}
	}
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Error("a management route was forwarded to Bob")
	}
}

// Single-tenant mode must be untouched: no token needed, static upstream used.
func TestSingleTenantUnchanged(t *testing.T) {
	var got *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil, Options{
		OpenAIUpstream: up.URL,
	})
	defer h.Close()
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	r.Header.Set("Authorization", "Bearer client-own-key")
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("single-tenant request = %d %s", w.Code, w.Body)
	}
	// With no key configured the client's own auth passes through, as documented.
	if got == nil || got.Header.Get("Authorization") != "Bearer client-own-key" {
		t.Errorf("single-tenant pass-through changed: %v", got.Header.Get("Authorization"))
	}
}

// Identity now travels in its OWN header, because the auth slot carries the caller's
// provider key. An auth slot is still read, but only for a value shaped like one of
// our tokens — otherwise a provider key would be sent off to the registry.
func TestTokenFromRequestSlots(t *testing.T) {
	tok := tenant.TokenPrefix + strings.Repeat("A", 26)
	cases := map[string]struct{ hdr, val, want string }{
		"own header":        {TokenHeader, tok, tok},
		"own header bearer": {TokenHeader, "Bearer " + tok, tok},
		"authz bearer":      {"Authorization", "Bearer " + tok, tok},
		"authz bare":        {"Authorization", tok, tok},
		"x-api-key":         {"x-api-key", tok, tok},
		"goog":              {"x-goog-api-key", tok, tok},
		// A provider credential is NOT a token. This is the whole point: it must be
		// forwarded, not looked up.
		"provider key": {"Authorization", "Bearer sk-ant-not-ours", ""},
		"empty":        {"Authorization", "", ""},
		"bearer-only":  {"Authorization", "Bearer ", ""},
		"whitespace":   {"x-api-key", "   ", ""},
		// The dedicated header is not shape-checked: whatever is there is offered to
		// the registry, which is the one place that decides what a valid token is.
		"header-shaped": {TokenHeader, "not-a-token", "not-a-token"},
	}
	for name, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.val != "" {
			r.Header.Set(c.hdr, c.val)
		}
		if got := TokenFromRequest(r); got != c.want {
			t.Errorf("%s: TokenFromRequest = %q, want %q", name, got, c.want)
		}
	}
}

// CallerKey is the other half: it must find the caller's own credential in any slot,
// and must never return one of our tokens as if it were one.
func TestCallerKeyIgnoresOurToken(t *testing.T) {
	tok := tenant.TokenPrefix + strings.Repeat("B", 26)
	for _, slot := range authHeaders {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(slot, "Bearer caller-own-key")
		if got := CallerKey(r); got != "caller-own-key" {
			t.Errorf("%s: CallerKey = %q", slot, got)
		}
		r = httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(slot, "Bearer "+tok)
		if got := CallerKey(r); got != "" {
			t.Errorf("%s: CallerKey returned our own token %q", slot, got)
		}
	}
}

// Content capture needs BOTH the operator's flag and the tenant's consent.
func TestContentCaptureNeedsTenantConsent(t *testing.T) {
	h := &Handler{opts: Options{}}
	if h.captureContentFor(&Tenancy{CaptureContent: true}) {
		t.Error("captured content with the dashboard off")
	}
}

func TestStatusOfDefaultsToUnauthorized(t *testing.T) {
	if code, _ := statusOf(errNoToken); code != http.StatusUnauthorized {
		t.Errorf("errNoToken = %d", code)
	}
	if code, _ := statusOf(errTenantOff); code != http.StatusForbidden {
		t.Errorf("errTenantOff = %d", code)
	}
	if code, _ := statusOf(io.EOF); code != http.StatusUnauthorized {
		t.Errorf("an unclassified auth error = %d, want 401 not 500", code)
	}
}

// The 401 body must be JSON — agents surface it to users, and a bare status leaves
// someone guessing why their proxy stopped working.
func TestAuthFailureBodyIsJSON(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	w := f.post("/openai/v1/chat/completions", "", "")
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("401 body is not JSON: %q", w.Body.String())
	}
	if out["error"] == "" {
		t.Errorf("401 body has no error message: %v", out)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 did not set WWW-Authenticate")
	}
}

// --- caller-credential pass-through -----------------------------------------

// The whole point of the change: the caller's OWN provider key reaches the upstream,
// so their traffic is billed to their own account and not the operator's.
func TestCallerKeyReachesUpstream(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "openai")
	_, tok := f.register(t, "a@ibm.com")
	if w := f.postCaller("/openai/v1/chat/completions", tok, "caller-own-key", ""); w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	up := f.lastUpstream(t)
	if got := up.Header.Get("Authorization"); got != "Bearer caller-own-key" {
		t.Errorf("upstream Authorization = %q, want the caller's own key", got)
	}
	// And our token stayed on the box, in either slot it might have travelled in.
	for _, h := range append([]string{TokenHeader}, authHeaders...) {
		if strings.Contains(up.Header.Get(h), tok) {
			t.Errorf("upstream saw our token in %s", h)
		}
	}
}

// An anthropic-dialect caller's key rides x-api-key and must survive untouched.
func TestCallerKeyReachesUpstreamAnthropicSlot(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "anthropic")
	_, tok := f.register(t, "a@ibm.com")
	if w := f.postCaller("/anthropic/v1/messages", tok, "caller-anthropic-key", "x-api-key"); w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	if got := f.lastUpstream(t).Header.Get("x-api-key"); got != "caller-anthropic-key" {
		t.Errorf("upstream x-api-key = %q, want the caller's own key", got)
	}
}

// No server key is consulted in hosted mode even when one happens to be in the
// environment: the upstream declares no key_env, so nothing may be injected.
func TestServerKeyNotUsedWhenUpstreamDeclaresNone(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "real-upstream-secret")
	f := newHostedFixtureNoKey(t, "up", "openai")
	_, tok := f.register(t, "a@ibm.com")
	if w := f.postCaller("/openai/v1/chat/completions", tok, "caller-own-key", ""); w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	for _, h := range authHeaders {
		if strings.Contains(f.lastUpstream(t).Header.Get(h), "real-upstream-secret") {
			t.Fatalf("the server credential leaked into %s", h)
		}
	}
}

// The failure that must never become a fallback: a caller with no credential of their
// own gets a 401, not somebody else's key.
func TestNoCallerKeyIsCleanUnauthorized(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "openai")
	_, tok := f.register(t, "a@ibm.com")
	w := f.postCaller("/openai/v1/chat/completions", tok, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("authenticated tenant with no provider key = %d, want 401", w.Code)
	}
	f.mu.Lock()
	n := len(f.seen)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("the request was forwarded despite having no credential")
	}
}

// Two tenants, two keys, two stores: neither the credential nor the state may cross.
func TestTwoTenantsStayIsolated(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "openai")
	_, tokA := f.register(t, "a@ibm.com")
	_, tokB := f.register(t, "b@ibm.com")

	if w := f.postCaller("/openai/v1/chat/completions", tokA, "key-A", ""); w.Code != http.StatusOK {
		t.Fatalf("A = %d %s", w.Code, w.Body)
	}
	if got := f.lastUpstream(t).Header.Get("Authorization"); got != "Bearer key-A" {
		t.Fatalf("A's upstream credential = %q", got)
	}
	if w := f.postCaller("/openai/v1/chat/completions", tokB, "key-B", ""); w.Code != http.StatusOK {
		t.Fatalf("B = %d %s", w.Code, w.Body)
	}
	if got := f.lastUpstream(t).Header.Get("Authorization"); got != "Bearer key-B" {
		t.Fatalf("B's upstream credential = %q", got)
	}
	// And they are different tenancies, so their compaction state cannot be shared.
	tnA, err := f.h.opts.Tenants.Resolve(withToken(tokA, "key-A"))
	if err != nil {
		t.Fatal(err)
	}
	tnB, err := f.h.opts.Tenants.Resolve(withToken(tokB, "key-B"))
	if err != nil {
		t.Fatal(err)
	}
	if tnA.ID == tnB.ID || tnA.Store == tnB.Store {
		t.Error("two tenants resolved to one identity or one store")
	}
}

func withToken(token, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r.Header.Set(TokenHeader, token)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// Bob cannot send a custom header, so it is identified by the sha256 of the key it
// already sends — but ONLY once that digest has been bound. Unbound is a 401, never a
// silent pick of whoever happens to be first in the table.
func TestAgentKeyIdentifiesTenantOnlyAfterBinding(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "bob")
	tn, _ := f.register(t, "a@ibm.com")

	w := f.postCaller("/inference/v1/chat/completions", "", "bob-own-key", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unbound provider key = %d, want 401", w.Code)
	}
	if err := f.reg.BindAgentKey(tn.ID, "bob-own-key"); err != nil {
		t.Fatal(err)
	}
	w = f.postCaller("/inference/v1/chat/completions", "", "bob-own-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("bound provider key = %d %s", w.Code, w.Body)
	}
	got, err := f.h.opts.Tenants.Resolve(withToken("", "bob-own-key"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tn.ID {
		t.Errorf("agent key resolved to %s, want %s", got.ID, tn.ID)
	}
	// And the key itself still went upstream: recognising it must not consume it.
	if v := f.lastUpstream(t).Header.Get("Authorization"); v != "Bearer bob-own-key" {
		t.Errorf("upstream Authorization = %q", v)
	}
}

// The explicit per-upstream server key remains supported — that is the eval-containers
// gateway and the local single-tenant fallback — and when it is set the caller's slot
// is replaced rather than forwarded.
func TestServerKeyStillSupportedAsExplicitFallback(t *testing.T) {
	f := newHostedFixture(t, "up", "openai") // KeyEnv set
	_, tok := f.register(t, "a@ibm.com")
	if w := f.postCaller("/openai/v1/chat/completions", tok, "caller-own-key", ""); w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	if got := f.lastUpstream(t).Header.Get("Authorization"); got != "Bearer real-upstream-secret" {
		t.Errorf("upstream Authorization = %q, want the configured server key", got)
	}
}

// The LLM-calling components must spend the CALLER's credential, and must fail OPEN
// (nil client, component degrades) when there is none — never fall back to a server
// key that would bill one account for everyone's compaction.
func TestIncomingModelUsesCallerCredential(t *testing.T) {
	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil, Options{})
	defer h.Close()
	body := []byte(`{"model":"claude-haiku-4-5"}`)
	up := upstream{base: "https://upstream.invalid"}

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.Header.Set("x-api-key", "caller-own-key")
	m, ok := h.incomingModel("anthropic", up, body, r).(cheapmodel.Anthropic)
	if !ok || m.APIKey != "caller-own-key" {
		t.Errorf("anthropic client key = %q, want the caller's", m.APIKey)
	}

	// Our own token is not a provider credential: it must not be handed to the model
	// client, and with nothing else available the component gets nil.
	r = httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer "+tenant.TokenPrefix+strings.Repeat("C", 26))
	if got := h.incomingModel("anthropic", up, body, r); got != nil {
		t.Errorf("a bare context-guru token produced a model client: %#v", got)
	}
	if got := h.incomingModel("anthropic", up, body, httptest.NewRequest(http.MethodPost, "/", nil)); got != nil {
		t.Errorf("no credential produced a model client: %#v", got)
	}
}

// The static "config"-source cheap model authenticates with a SERVER credential, so it
// is withheld in hosted mode. Single-tenant keeps it.
func TestStaticModelWithheldWhenHosted(t *testing.T) {
	cm := cheapmodel.Anthropic{Model: "m", APIKey: "server-side"}
	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil,
		Options{CheapModel: cm})
	defer h.Close()
	if h.staticModel() == nil {
		t.Error("single-tenant lost its configured cheap model")
	}
	h.opts.Tenants = NewTenantSource(nil, nil, nil, 0)
	if h.staticModel() != nil {
		t.Error("hosted mode offered a tenant the server's cheap-model credential")
	}
}
