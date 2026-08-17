package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/store"
)

// A published *Tenancy must be immutable: the request path reads Label,
// CaptureContent, Preset and the Up* names with NO lock, so refreshing them in place
// on the cache-hit path is a data race — one agent with two turns in flight is
// enough. Run with -race; before the fix this fails, after it the refresh publishes a
// new pointer instead.
func TestPublishedTenancyIsNotMutated(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	tn, _ := f.register(t, "a@ibm.com")
	src := f.h.opts.Tenants

	first, err := src.ForTenant(tn)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// The unlocked readers, as the request path has them (captureContentFor,
	// newCapture, upstreamFor).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = first.CaptureContent
					_ = first.Label + first.Preset + first.UpOpenAI + first.UpAnthropic + first.UpBob
					_ = first.Manager
				}
			}
		}()
	}
	// Resolutions that refresh the non-pipeline fields. A copy per call, so the test
	// never races on the tenant row itself.
	for i := 0; i < 300; i++ {
		c := *tn
		c.Label = "laptop-" + strconv.Itoa(i)
		c.CaptureContent = i%2 == 0
		got, err := src.ForTenant(&c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Label != c.Label {
			t.Fatalf("refresh did not take effect: label = %q, want %q", got.Label, c.Label)
		}
	}
	close(stop)
	wg.Wait()

	// The pipeline and store are shared across refreshes, not rebuilt — otherwise a
	// label change would silently drop the tenant's frozen decisions.
	again, err := src.ForTenant(tn)
	if err != nil {
		t.Fatal(err)
	}
	if again.Store != first.Store || again.Pipe != first.Pipe {
		t.Error("a field refresh rebuilt the tenant's pipeline or store")
	}
}

// Bounding the limiter's map must not throw away every OTHER key's rate window.
// Before the fix, entry 10001 replaced the whole map with itself.
func TestLimiterBoundEvictsOneKeyNotAll(t *testing.T) {
	l := NewLimiter(Limits{RequestsPerMinute: 1})
	minuteAtStart := time.Now().Truncate(time.Minute)
	// Fill past the bound, oldest first.
	keys := make([]string, maxLimiterKeys+1)
	for i := range keys {
		keys[i] = "k" + string(rune('a'+i%26)) + "-" + strconv.Itoa(i)
		if _, err := l.Acquire(keys[i]); err != nil {
			t.Fatalf("first request for %s was limited: %v", keys[i], err)
		}
	}
	l.mu.Lock()
	n := l.tenants.ll.Len()
	l.mu.Unlock()
	if n != maxLimiterKeys {
		t.Fatalf("limiter holds %d keys, want the bound of %d", n, maxLimiterKeys)
	}
	// The most recent keys kept their window: a second request in the same minute is
	// refused. If the map had been cleared, every one of these would be allowed again.
	//
	// The window is a WALL-CLOCK minute, so if the fill above straddled a minute
	// boundary every window legitimately reset and the assertion below would fail for a
	// reason that has nothing to do with the bound. The fill takes ~60ms, so this is a
	// ~0.1% flake per run and was worth one branch rather than a rare mystery failure.
	if !time.Now().Truncate(time.Minute).Equal(minuteAtStart) {
		t.Skip("the fill straddled a minute boundary, so every rate window reset legitimately")
	}
	for _, k := range keys[len(keys)-100:] {
		if _, err := l.Acquire(k); err == nil {
			t.Fatalf("%s lost its rate window when the bound was reached", k)
		}
	}

}

// --- registration gating (F2) ------------------------------------------------

// registerVia posts a registration from a given client address.
func registerVia(t *testing.T, f *hostedFixture, email, code, remote string) int {
	t.Helper()
	body := `{"email":"` + email + `","label":"l","password":"` + testPassword + `"`
	if code != "" {
		body += `,"code":"` + code + `"`
	}
	body += `}`
	r := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(body))
	r.Header.Set("content-type", "application/json")
	if remote != "" {
		r.RemoteAddr = remote
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w.Code
}

// Self-registration is OPEN by default: a colleague signing themselves up is the point
// of a hosted service, and an account no longer spends the operator's money (users
// forward their own provider key) nor exists without a code mailed to a real address.
// `closed` remains available for a public port or a maintenance window, and this pins
// that it actually refuses.
func TestRegistrationOpenByDefaultAndClosableByTheOperator(t *testing.T) {
	t.Setenv(envRegisterMode, "") // unset restores after the test; empty is the open default
	f := newHostedFixture(t, "up", "openai")
	if code := registerVia(t, f, "a@ibm.com", "", ""); code != http.StatusCreated {
		t.Fatalf("register with no CG_REGISTER = %d, want 201", code)
	}
	t.Setenv(envRegisterMode, "closed")
	if code := registerVia(t, f, "b@ibm.com", "", ""); code != http.StatusForbidden {
		t.Fatalf("register with CG_REGISTER=closed = %d, want 403", code)
	}
}

func TestRegistrationInviteCode(t *testing.T) {
	t.Setenv(envRegisterMode, "invite")
	f := newHostedFixture(t, "up", "openai")

	// Invite mode with no code configured refuses rather than falling through to open.
	t.Setenv(envRegisterCode, "")
	if code := registerVia(t, f, "a@ibm.com", "", ""); code != http.StatusForbidden {
		t.Errorf("invite mode with no configured code = %d, want 403", code)
	}
	// Not the secret itself, just a value the test sets and checks against.
	t.Setenv(envRegisterCode, "let-me-in")
	if code := registerVia(t, f, "a@ibm.com", "wrong", ""); code != http.StatusForbidden {
		t.Errorf("register with a wrong invite code = %d, want 403", code)
	}
	if code := registerVia(t, f, "a@ibm.com", "let-me-in", ""); code != http.StatusCreated {
		t.Errorf("register with the invite code = %d, want 201", code)
	}
}

// Open mode is still not an open faucet: one address cannot mint accounts in a loop.
func TestRegistrationRateLimitedPerIP(t *testing.T) {
	t.Setenv(envRegisterMode, "open")
	f := newHostedFixture(t, "up", "openai")
	limited := false
	for i := 0; i < registrationsPerMinute+3; i++ {
		code := registerVia(t, f, "u"+strconv.Itoa(i)+"@ibm.com", "", "203.0.113.7:5555")
		if code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("an address registered %d+ accounts without being limited", registrationsPerMinute+3)
	}
	// A different address is unaffected — the limit is per client, not global.
	if code := registerVia(t, f, "other@ibm.com", "", "198.51.100.9:4444"); code != http.StatusCreated {
		t.Errorf("a different address was refused = %d, want 201", code)
	}
}

// The METRICS_TOKEN comparison must be constant time. A timing assertion would be
// flaky to the point of uselessness, so this asserts the mechanism instead — which is
// the only thing a reviewer can check by eye anyway.
func TestMetricsTokenComparedInConstantTime(t *testing.T) {
	src, err := os.ReadFile("promexport.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (h *Handler) metricsAllowed(")
	if i < 0 {
		t.Fatal("metricsAllowed is gone; move this check")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j > 0 {
		fn = fn[:j]
	}
	if !strings.Contains(fn, "subtle.ConstantTimeCompare") {
		t.Error("metricsAllowed compares the metrics token without crypto/subtle")
	}
	if strings.Contains(fn, "== tok") || strings.Contains(fn, "tok ==") {
		t.Error("metricsAllowed still compares the metrics token with ==")
	}
}

// And the gate still works: right token in, wrong token out.
func TestMetricsTokenStillGates(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	f.h.opts.MetricsToken = "scrape-token-for-this-test"
	for tok, want := range map[string]int{
		"scrape-token-for-this-test": http.StatusOK,
		"scrape-token-for-this-tesX": http.StatusForbidden,
		"":                           http.StatusForbidden,
	} {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		r.RemoteAddr = "203.0.113.5:1111" // not loopback, so the token is the only way in
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("/metrics with token %q = %d, want %d", tok, w.Code, want)
		}
	}
}

// --- cross-site writes (finding A) -------------------------------------------

// Every control-plane write is authenticated by the browser COOKIE, with no CSRF token
// and — before this — no check on where the request came from. SameSite=Lax is not the
// boundary it looks like: its unit is the registrable domain, so on a deployment at
// contextguru.<something>.ibm.com any colleague's host under ibm.com is "same site" and
// its cookies ride along. A form post also needs no preflight, and readJSON ignored
// Content-Type, so `<form enctype="text/plain">` reached the JSON decoder with the '='
// landing inside a string value where DisallowUnknownFields does not mind it.
//
// Both halves of the demonstrated attack are pinned here: minting a token on the
// victim's account, and destroying the victim's session.
func TestCrossOriginWritesAreRefused(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()

	// The attacker's shape: another origin under the same registrable domain, the
	// content type a form can send, and the victim's cookie attached by the browser.
	evil := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("content-type", "text/plain")
		r.Header.Set("Origin", "https://evil.ibm.com")
		for _, c := range jar {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, r)
		return rec
	}
	if got := evil("POST", "/api/me/tokens", `{"label":"pwned=1"}`); got.Code != http.StatusForbidden {
		t.Errorf("cross-origin token mint = %d, want 403: %s", got.Code, got.Body)
	}
	// /api/logout reads no body, so it does not pass through readJSON's guard and needs
	// its own — a forced sign-out is the other thing this shape can do.
	if got := evil("POST", "/api/logout", ""); got.Code != http.StatusForbidden {
		t.Errorf("cross-origin logout = %d, want 403: %s", got.Code, got.Body)
	}
	// The victim is still signed in, with exactly the one token they started with.
	w2, me := f.do(t, "GET", "/api/me", "", jar)
	if w2.Code != http.StatusOK {
		t.Fatalf("the victim's session did not survive a cross-origin logout: %d", w2.Code)
	}
	if toks, _ := me["tokens"].([]any); len(toks) != 1 {
		t.Errorf("the token list was polluted cross-origin: %v", me["tokens"])
	}

	// The guard is "present and different", not "required": a same-origin browser write
	// still works, and so does a caller that sends no Origin at all (curl, an agent,
	// every other test in this package).
	sameOrigin := httptest.NewRequest("POST", "/api/me/tokens", strings.NewReader(`{"label":"legit"}`))
	sameOrigin.Header.Set("content-type", "application/json")
	sameOrigin.Header.Set("Origin", "http://"+sameOrigin.Host)
	for _, c := range jar {
		sameOrigin.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, sameOrigin)
	if rec.Code != http.StatusCreated {
		t.Errorf("same-origin mint = %d, want 201: %s", rec.Code, rec.Body)
	}
}

// --- metering (finding B) -----------------------------------------------------

// TENANT_RPM and TENANT_CONCURRENT were enforced in chat() and nowhere else, so the two
// other authenticated entry points ran the box for free: /compact drives the whole
// pipeline (tokenisation, tree-sitter, several passes) over a body up to 32 MiB, and the
// Bob catch-all forwards arbitrary methods and paths. With the limiter at one request a
// minute, chat refused the second call while /compact served twenty.
func TestEveryAuthenticatedRouteIsMetered(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	_, tok := f.register(t, "a@ibm.com")

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(TokenHeader, tok)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, r)
		return rec
	}
	for _, tc := range []struct {
		name string
		call func() *httptest.ResponseRecorder
		ok   int
	}{
		{"chat", func() *httptest.ResponseRecorder {
			return f.post("/openai/v1/chat/completions", tok, "")
		}, http.StatusOK},
		{"compact", func() *httptest.ResponseRecorder { return f.post("/compact", tok, "") }, http.StatusOK},
		{"bob catch-all", func() *httptest.ResponseRecorder { return get("/admin/v1/profile") }, http.StatusOK},
	} {
		// A fresh limiter per route, so each one is measured against its own budget of one.
		f.h.limiter = NewLimiter(Limits{RequestsPerMinute: 1})
		if w := tc.call(); w.Code != tc.ok {
			t.Fatalf("%s: first request = %d, want %d: %s", tc.name, w.Code, tc.ok, w.Body)
		}
		if w := tc.call(); w.Code != http.StatusTooManyRequests {
			t.Errorf("%s: second request past a limit of 1/min = %d, want 429", tc.name, w.Code)
		}
	}
}

// --- account enumeration (finding C) ------------------------------------------

// /api/register answered 409 for an address that already has a VERIFIED account and 201
// for one that does not, unauthenticated, so anyone could ask the service which of their
// colleagues have accounts. ctlLogin and ctlVerify were deliberately built not to leak
// that (one message for unknown/wrong/no-password, and VerifyLogin spends argon2 against
// a decoy hash so the timings match); this endpoint gave it away for free.
//
// The reply is now the same either way, and the difference lands in the mailbox — which
// only the address's owner can read.
func TestRegisterDoesNotRevealWhetherTheAddressIsTaken(t *testing.T) {
	f := ctlFixture(t)
	if w, _ := f.signUp(t, "known@ibm.com", "l"); w.Code != http.StatusCreated {
		t.Fatalf("fixture account: %d %s", w.Code, w.Body)
	}
	// A distinct client address per probe: the point here is the answer, not the per-IP
	// rate limit that TestRegistrationRateLimitedPerIP covers.
	reg := func(email, remote string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(
			`{"email":"`+email+`","label":"l","password":"`+testPassword+`"}`))
		r.Header.Set("content-type", "application/json")
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	takenCode, takenBody := reg("known@ibm.com", "203.0.113.1:1111")
	freeCode, freeBody := reg("nobody-here@ibm.com", "203.0.113.2:2222")
	if takenCode != freeCode {
		t.Errorf("register answers %d for a taken address and %d for a free one", takenCode, freeCode)
	}
	if takenCode != http.StatusCreated {
		t.Errorf("register on a taken address = %d, want the same 201 as a fresh one", takenCode)
	}
	// The body must not be the tell either: same fields, same "what happens next".
	if takenBody["next"] != freeBody["next"] || takenBody["email"] != "known@ibm.com" ||
		len(takenBody) != len(freeBody) {
		t.Errorf("the bodies differ:\n taken: %v\n free:  %v", takenBody, freeBody)
	}

	// What the OWNER gets is a notice, not a code: nothing an attacker could enter, and
	// the one place the "this address has an account" fact appears.
	b, err := os.ReadFile(os.Getenv(envMailDevSink))
	if err != nil {
		t.Fatal(err)
	}
	mails := strings.Split(string(b), "--- ")
	// The last mail is the free address's real code; the one before it is the notice.
	if len(mails) < 3 {
		t.Fatalf("expected a mail per registration, got %d:\n%s", len(mails)-1, b)
	}
	notice := mails[len(mails)-2]
	if !strings.Contains(notice, "known@ibm.com") {
		t.Errorf("the notice did not go to the address's owner:\n%s", notice)
	}
	if regexp.MustCompile(`\b\d{6}\b`).MatchString(notice) {
		t.Errorf("a verification code was mailed for an address the caller does not own:\n%s", notice)
	}
}

// --- the second credential exit (finding D) -----------------------------------

// setUpstreamAuth says it "is the ONE place that decides" what credential leaves the
// box, and incomingModel disagreed with it: on a route whose upstream injects a
// server-held key, setUpstreamAuth deletes the caller's auth slots and injects the
// server's, while the component's own LLM client was built preferring CallerKey and
// pointed at the same up.base. So one request carried two different credentials to one
// host. Both now read the same resolved decision, up.setKey.
func TestIncomingModelFollowsTheRoutesCredentialDecision(t *testing.T) {
	// Gateway mode: the operator holds the key, the caller holds a placeholder.
	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil,
		Options{AnthropicKey: "gateway-key-for-this-test"})
	defer h.Close()
	body := []byte(`{"model":"claude-haiku-4-5"}`)
	up := upstream{base: "https://upstream.invalid", path: "/v1/messages",
		setKey: headerKey("x-api-key", h.opts.AnthropicKey)}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.Header.Set("x-api-key", "caller-placeholder")

	m, ok := h.incomingModel("anthropic", up, body, r).(cheapmodel.Anthropic)
	if !ok {
		t.Fatalf("gateway route produced no model client")
	}
	if m.APIKey == "caller-placeholder" {
		t.Errorf("the component's client carries the CALLER's credential to a route that " +
			"replaces it with the server's")
	}
	if m.APIKey != h.opts.AnthropicKey {
		t.Errorf("model client key = %q, want the same credential setUpstreamAuth injects", m.APIKey)
	}

	// Hosted: the injector holds its key in a closure that cannot be read back, and a
	// server credential must not fund a tenant's compaction anyway (the reason
	// staticModel is withheld there). So the component degrades to deterministic.
	h.opts.Tenants = NewTenantSource(nil, nil, nil, 0)
	if got := h.incomingModel("anthropic", up, body, r); got != nil {
		t.Errorf("hosted mode handed a component a server-held credential: %#v", got)
	}

	// And the hosted DEFAULT is unchanged: no server key on the route, so the caller's
	// own credential is what both exits use — the whole point of that path.
	noKey := upstream{base: "https://upstream.invalid", path: "/v1/messages"}
	m2, ok := h.incomingModel("anthropic", noKey, body, r).(cheapmodel.Anthropic)
	if !ok || m2.APIKey != "caller-placeholder" {
		t.Errorf("a route that forwards the caller's key did not give the component the same: %#v", m2)
	}
}

// --- upstream names on the manager's patch (the "also") ----------------------

// PUT /api/me validates upstream names against the operator's allow-list; PATCH
// /api/tenants/{id} did not. Not an SSRF hole (a name is never a URL, and upstreamFor
// looks it up in the same map), but a manager could set a tenant's up_* to a name the
// proxy refuses at request time — breaking someone's agent with no feedback where the
// change was made.
func TestManagerPatchValidatesUpstreamNames(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "a@ibm.com", "l")
	target := f.mustMe(t, w.Result().Cookies())

	w, _ = f.do(t, "PATCH", "/api/tenants/"+target, `{"up_anthropic":"not-in-the-allow-list"}`, mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a manager set an upstream the proxy will refuse = %d, want 400", w.Code)
	}
	// An allow-listed name still saves.
	w, _ = f.do(t, "PATCH", "/api/tenants/"+target, `{"up_anthropic":"up"}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Errorf("a valid upstream name = %d, want 200: %s", w.Code, w.Body)
	}
}

// --- the browser cookie must not leave the box -------------------------------

// The Bob catch-all forwards verbatim, and copyHeaders stripped only hop-by-hop headers
// and our own x-context-guru-* prefix. So a client that presented both a context-guru
// token and its cg_dash DASHBOARD cookie shipped a live browser session to a third-party
// host: the upstream received `Cookie: cg_dash=<session>`.
//
// Both credential branches are covered, because the strip has to live in copyHeaders for
// the gateway one to be covered at all — setUpstreamAuth's gateway branch deletes the auth
// slots and nothing else, and gateway mode is what the hosted deployment runs.
func TestCookieNeverReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) *hostedFixture
	}{
		{"gateway key", func(t *testing.T) *hostedFixture { return newHostedFixture(t, "up", "openai") }},
		{"caller pays", func(t *testing.T) *hostedFixture { return newHostedFixtureNoKey(t, "up", "openai") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.open(t)
			_, tok := f.register(t, "a@ibm.com")
			// The reduced chat route and the verbatim catch-all: one forwards a rewritten
			// body, the other anything at all, and both go through copyHeaders.
			for _, path := range []string{"/openai/v1/chat/completions", "/v1/anything"} {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(
					`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set(TokenHeader, tok)
				r.Header.Set("Authorization", "Bearer caller-own-key")
				r.AddCookie(&http.Cookie{Name: dashCookie, Value: "dash-session-canary"})
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("%s = %d %s", path, w.Code, w.Body)
				}
				if got := f.lastUpstream(t).Header.Get("Cookie"); got != "" {
					t.Errorf("%s: the upstream received the browser session cookie: %q", path, got)
				}
			}
		})
	}
}

// A registry refusal from the bind route is the USER's to fix, and answering 500 for all of
// them points them at the operator instead. ErrForbidden in particular is the anti-hijack
// case — the digest is already bound to a different account — and it has to say who can
// undo it.
//
// This reads the source rather than driving the handler, the same way
// TestMetricsTokenComparedInConstantTime does, and for the same kind of reason: the errors
// can only be PRODUCED by the registry (BindAgentKey is a concrete *tenant.Registry with no
// seam to inject a failure), and the ErrForbidden case is produced by a tenant/ change that
// is not on this branch. The behavioural test belongs with that change; this pins the
// mapping so it cannot be collapsed back into 500 while the two land separately.
func TestBindAgentKeyMapsRegistryErrorsToUserStatuses(t *testing.T) {
	src, err := os.ReadFile("control.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (h *Handler) ctlBindAgentKey(")
	if i < 0 {
		t.Fatal("ctlBindAgentKey is gone; move this check")
	}
	fn := body[i:]
	if j := strings.Index(fn[1:], "\nfunc "); j > 0 {
		fn = fn[:j]
	}
	for _, want := range []string{
		"tenant.ErrForbidden", "http.StatusForbidden",
		"tenant.ErrNotFound", "http.StatusNotFound",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("ctlBindAgentKey does not map %s; every registry error still reads as 500", want)
		}
	}
}

// --- the agent-key oracle (proxy auth failures) -------------------------------

// A failed proxy authentication used to be free, and one of them is an oracle: an agent
// that can set no header of ours is identified by the sha256 of the provider key it already
// sends, so "is this string a bound agent key" was answerable at whatever rate the front
// end allows — and a hit is impersonation, not just confirmation.
//
// The shipped bound is asserted, not an injected one: a limiter that is only wired in a
// test is not a control.
func TestFailedProxyAuthIsThrottledPerClientAddress(t *testing.T) {
	f := newHostedFixtureNoKey(t, "up", "bob")
	tn, _ := f.register(t, "a@ibm.com")
	if err := f.reg.BindAgentKey(tn.ID, "bob-own-fake-key-for-tests"); err != nil {
		t.Fatal(err)
	}
	try := func(key, remote string) int {
		r := httptest.NewRequest(http.MethodPost, "/inference/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		return w.Code
	}
	const guesser = "203.0.113.9:5555"
	throttled := 0
	for i := 0; i < authFailuresPerMinute+3; i++ {
		switch code := try("unbound-guess-"+strconv.Itoa(i), guesser); code {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusUnauthorized: // an honest refusal, while the budget lasts
		default:
			t.Fatalf("guess %d = %d, want 401 or 429", i, code)
		}
	}
	if throttled == 0 {
		t.Errorf("one address made %d unbound-key attempts unthrottled", authFailuresPerMinute+3)
	}

	// The charge is on FAILURE, so a legitimate bound key from the very same address is
	// unaffected — otherwise the control would be a way to lock a colleague out.
	if code := try("bob-own-fake-key-for-tests", guesser); code != http.StatusOK {
		t.Errorf("a bound key from a throttled address = %d, want 200", code)
	}
	// And the bucket is per address, not global: nobody else's guessing spends someone
	// else's budget, in either direction.
	if code := try("still-unbound", "198.51.100.4:4444"); code != http.StatusUnauthorized {
		t.Errorf("a different address = %d, want its own 401", code)
	}
}
