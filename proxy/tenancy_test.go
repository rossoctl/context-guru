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

func newHostedFixture(t *testing.T, upstreamName, dialect string) *hostedFixture {
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
			Dialect: dialect, BaseURL: f.upstream.URL, KeyEnv: "TEST_UPSTREAM_KEY",
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
	for _, slot := range tokenHeaders {
		t.Run(slot, func(t *testing.T) {
			f := newHostedFixture(t, "up", "openai")
			_, tok := f.register(t, "a@ibm.com")
			if w := f.post("/openai/v1/chat/completions", tok, slot); w.Code != http.StatusOK {
				t.Fatalf("authenticated request = %d %s", w.Code, w.Body)
			}
			up := f.lastUpstream(t)
			for _, h := range tokenHeaders {
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

func TestTokenFromRequestSlots(t *testing.T) {
	cases := map[string]struct{ hdr, val, want string }{
		"bearer":       {"Authorization", "Bearer tok123", "tok123"},
		"bearer-cased": {"Authorization", "bEaReR tok123", "tok123"},
		"bare-authz":   {"Authorization", "tok123", "tok123"},
		"x-api-key":    {"x-api-key", "tok123", "tok123"},
		"goog":         {"x-goog-api-key", "tok123", "tok123"},
		"empty":        {"Authorization", "", ""},
		"bearer-only":  {"Authorization", "Bearer ", ""},
		"whitespace":   {"x-api-key", "   ", ""},
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
