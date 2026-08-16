package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The refusal counters, which exist because every way a request could be turned away
// used to be invisible: a rate-limited or over-budget tenant saw failures while the
// dashboard showed a healthy service.
//
// Every assertion here is a DELTA, not an absolute. The counters are process-wide, so
// any other test in this package that trips a limit would otherwise change the answer.

type refusalDelta struct {
	totals   map[refusalReason]int64
	byTenant map[string]map[refusalReason]int64
}

// measure runs fn and returns only what it changed.
func measure(fn func()) refusalDelta {
	beforeT, beforeTn := refusalSnapshot()
	fn()
	afterT, afterTn := refusalSnapshot()
	d := refusalDelta{totals: map[refusalReason]int64{}, byTenant: map[string]map[refusalReason]int64{}}
	for _, r := range refusalReasons {
		if n := afterT[r] - beforeT[r]; n != 0 {
			d.totals[r] = n
		}
	}
	for tn, rs := range afterTn {
		for r, n := range rs {
			if n -= beforeTn[tn][r]; n != 0 {
				if d.byTenant[tn] == nil {
					d.byTenant[tn] = map[refusalReason]int64{}
				}
				d.byTenant[tn][r] = n
			}
		}
	}
	return d
}

func (d refusalDelta) wantTotals(t *testing.T, want map[refusalReason]int64) {
	t.Helper()
	for _, r := range refusalReasons {
		if got := d.totals[r]; got != want[r] {
			t.Errorf("cg_refused_requests_total{reason=%q} moved by %d, want %d", r, got, want[r])
		}
	}
}

// resetRefusals clears the per-tenant breakdown. Test-only: the cap test below would
// otherwise leave the process at its key ceiling for every later test.
func resetRefusals() {
	refusalByTenant.Range(func(k, _ any) bool {
		refusalByTenant.Delete(k)
		return true
	})
	refusalKeyCount.Store(0)
}

// Each refusal path moves its own counter, by exactly one, and moves nothing else.
func TestRefusalCountersMoveOncePerPath(t *testing.T) {
	// Rate limit and concurrency are both 429 and must NOT collapse into one series:
	// they have different fixes.
	d := measure(func() {
		l := NewLimiter(Limits{RequestsPerMinute: 1})
		rel, _ := l.Acquire("t-rate")
		rel()
		rel, err := l.Acquire("t-rate")
		rel()
		if err == nil {
			t.Fatal("the second request was not rate limited")
		}
	})
	d.wantTotals(t, map[refusalReason]int64{refuseRateLimit: 1})
	if got := d.byTenant["t-rate"][refuseRateLimit]; got != 1 {
		t.Errorf("per-tenant rate_limit moved by %d, want 1", got)
	}

	d = measure(func() {
		l := NewLimiter(Limits{Concurrent: 1})
		held, _ := l.Acquire("t-conc")
		rel, err := l.Acquire("t-conc")
		rel()
		held()
		if err == nil {
			t.Fatal("the second concurrent request was not refused")
		}
	})
	d.wantTotals(t, map[refusalReason]int64{refuseConcurrency: 1})
	if got := d.byTenant["t-conc"][refuseConcurrency]; got != 1 {
		t.Errorf("per-tenant concurrency moved by %d, want 1", got)
	}

	// Auth, disabled account, and a route with no upstream — all through failAuth.
	for _, tc := range []struct {
		err  error
		want refusalReason
	}{
		{errNoToken, refuseAuth},
		{errBadToken, refuseAuth},
		{errTenantOff, refuseForbidden},
		{errNoUpstreamFor, refuseNoUpstream},
	} {
		d := measure(func() { failAuth(httptest.NewRecorder(), tc.err) })
		d.wantTotals(t, map[refusalReason]int64{tc.want: 1})
		if len(d.byTenant) != 0 {
			// An unauthenticated caller has no identity we are willing to label a series
			// with, and it must not be able to create one.
			t.Errorf("failAuth(%v) created per-tenant series %v", tc.err, d.byTenant)
		}
	}
}

// The trap this whole design is arranged around: the 429 path counts at the limit that
// decided it and then falls through to failAuth, which writes the status. If failAuth
// also counted, every refusal would be counted twice.
func TestRefusalNotCountedTwiceWhenWritten(t *testing.T) {
	d := measure(func() {
		l := NewLimiter(Limits{RequestsPerMinute: 1})
		rel, _ := l.Acquire("t-twice")
		rel()
		rel, err := l.Acquire("t-twice")
		rel()
		failAuth(httptest.NewRecorder(), err) // the real chat() sequence
	})
	d.wantTotals(t, map[refusalReason]int64{refuseRateLimit: 1})
}

// The registration limiter keys on CLIENT IP. Its refusals are real and must be counted,
// but an IP is attacker-supplied and must never reach a label.
func TestAnonymousLimiterNeverLabelsItsKey(t *testing.T) {
	d := measure(func() {
		l := newAnonLimiter(Limits{RequestsPerMinute: 1})
		rel, _ := l.Acquire("198.51.100.7")
		rel()
		rel, err := l.Acquire("198.51.100.7")
		rel()
		if err == nil {
			t.Fatal("the anonymous limiter did not refuse")
		}
	})
	d.wantTotals(t, map[refusalReason]int64{refuseRateLimit: 1})
	if len(d.byTenant) != 0 {
		t.Errorf("a client IP became a metric label: %v", d.byTenant)
	}
}

// End to end: an unauthenticated POST is refused, and the refusal is visible on
// /metrics with the name and labels the dashboards query.
func TestRefusalExportedOnMetrics(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")

	d := measure(func() {
		if w := f.post("/openai/v1/chat/completions", "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated POST = %d, want 401", w.Code)
		}
	})
	d.wantTotals(t, map[refusalReason]int64{refuseAuth: 1})

	// A refusal that DOES belong to a tenant, so the per-tenant family is exercised too.
	tn, tok := f.register(t, "user@ibm.com")
	f.h.limiter = NewLimiter(Limits{RequestsPerMinute: 1})
	f.post("/openai/v1/chat/completions", tok, "")
	if w := f.post("/openai/v1/chat/completions", tok, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST past a limit of 1 = %d, want 429", w.Code)
	}

	body := f.h.renderMetrics()
	for _, want := range []string{
		"# TYPE cg_refused_requests_total counter",
		`cg_refused_requests_total{reason="rate_limit"}`,
		`cg_refused_requests_total{reason="concurrency"}`,
		`cg_refused_requests_total{reason="auth"}`,
		`cg_refused_requests_total{reason="forbidden"}`,
		`cg_refused_requests_total{reason="no_upstream"}`,
		`cg_refused_requests_total{reason="upstream_error"}`,
		"# TYPE cg_tenant_refused_requests_total counter",
		`cg_tenant_refused_requests_total{tenant="` + tn.ID + `",label="laptop",reason="rate_limit"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

// A label must never carry an error string or an email. The error messages are ours
// today, but a message is a sentence with numbers in it — one refactor away from
// carrying a caller-supplied value — and /metrics is typically the least
// access-controlled surface in an organisation.
func TestRefusalLabelsCarryNoErrorTextOrEmail(t *testing.T) {
	f := newHostedFixture(t, "up", "openai")
	_, tok := f.register(t, "person@ibm.com")
	f.h.limiter = NewLimiter(Limits{RequestsPerMinute: 1})
	f.post("/openai/v1/chat/completions", tok, "")
	f.post("/openai/v1/chat/completions", tok, "")

	for _, line := range strings.Split(f.h.renderMetrics(), "\n") {
		if !strings.HasPrefix(line, "cg_refused_requests_total") &&
			!strings.HasPrefix(line, "cg_tenant_refused_requests_total") {
			continue
		}
		for _, bad := range []string{"@", "rate limit:", "retry in", "concurrency limit:",
			"spend cap", "token", "Bearer"} {
			if strings.Contains(line, bad) {
				t.Errorf("refusal series leaks %q: %s", bad, line)
			}
		}
	}
}

// The per-tenant breakdown is bounded even if the tenant set somehow is not, and the
// process-wide totals keep counting after the ceiling.
func TestRefusalPerTenantSeriesAreBounded(t *testing.T) {
	resetRefusals()
	t.Cleanup(resetRefusals)
	for i := 0; i < maxRefusalKeys+64; i++ {
		recordRefusal(refuseAuth, "tenant-"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+strconv.Itoa(i))
	}
	n := 0
	refusalByTenant.Range(func(any, any) bool { n++; return true })
	if n > maxRefusalKeys {
		t.Errorf("per-tenant refusal series = %d, want at most %d", n, maxRefusalKeys)
	}
	d := measure(func() { recordRefusal(refuseAuth, "one-more") })
	if d.totals[refuseAuth] != 1 {
		t.Error("the process-wide total stopped counting once the per-tenant cap was hit")
	}
}

// A refusal has to be findable by tenant, and the one refusal a USER can fix needs its
// own series.
//
// errNoProviderKey means "your account is fine, but you sent no provider credential of
// your own". It was invisible twice over: the cg.refused line carried NO tenant, because
// chat attached the per-request logger only after resolving the upstream and refuse()
// reads its logger out of the request context — with the tenant sitting in hand three
// lines above. And the count landed in reason="auth" together with every unknown token.
// That is the refusal a deployment sees from every user who has not yet added their own
// key, so both blind spots hide exactly the population that needs contacting.
func TestNoProviderKeyRefusalIsAttributedAndCounted(t *testing.T) {
	// No server-held key on the upstream, so the caller's own credential is required.
	f := newHostedFixtureNoKey(t, "up", "openai")
	tn, tok := f.register(t, "user@ibm.com")

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	d := measure(func() {
		if w := f.postCaller("/openai/v1/chat/completions", tok, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("a tenant with no provider key of its own = %d, want 401", w.Code)
		}
	})

	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "cg.refused") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("nothing logged a cg.refused line:\n%s", logs.String())
	}
	if !strings.Contains(line, "tenant="+tn.ID) {
		t.Errorf("the refusal is not findable by tenant: %s", line)
	}

	// Its own reason, with the tenant on it — which is what turns "N users broke" into
	// "these users broke" — and counted EXACTLY once: the SLO panel divides an unlabelled
	// sum of this family by refusals + requests, so a request counted under two reasons
	// would inflate the error-rate SLI (see failAuthAs).
	if got := d.totals[refuseAuth]; got != 0 {
		t.Errorf(`the same refusal also moved reason="auth" by %d; it must be counted once`, got)
	}
	if got := d.totals[refuseNoProviderKey]; got != 1 {
		t.Errorf(`cg_refused_requests_total{reason="no_provider_key"} moved by %d, want 1`, got)
	}
	if got := d.byTenant[tn.ID][refuseNoProviderKey]; got != 1 {
		t.Errorf("the per-tenant no_provider_key series moved by %d, want 1", got)
	}
	body := f.h.renderMetrics()
	for _, want := range []string{
		`cg_refused_requests_total{reason="no_provider_key"}`,
		`cg_tenant_refused_requests_total{tenant="` + tn.ID + `",label="laptop",reason="no_provider_key"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

// failAuthAs must answer byte-for-byte as failAuth does. It exists only to count a
// different reason (see refuseRoute), and a refusal that starts replying differently on
// one route than on the others is a bug an agent surfaces to a user as gibberish.
//
// This is the guard on a deliberate duplication: failAuth lives in tenancy.go, which
// another agent owns this cycle, so the reason-specific variant sits in proxy.go until it
// can collapse into failAuth's switch.
func TestFailAuthAsMatchesFailAuth(t *testing.T) {
	for _, err := range []error{errNoProviderKey, errNoToken, errTenantOff, errNoUpstreamFor} {
		a, b := httptest.NewRecorder(), httptest.NewRecorder()
		failAuth(a, err)
		failAuthAs(b, err, refuseAuth, "")
		if a.Code != b.Code {
			t.Errorf("%v: status %d vs %d", err, a.Code, b.Code)
		}
		if a.Body.String() != b.Body.String() {
			t.Errorf("%v: body %q vs %q", err, a.Body, b.Body)
		}
		for _, h := range []string{"Content-Type", "WWW-Authenticate"} {
			if a.Header().Get(h) != b.Header().Get(h) {
				t.Errorf("%v: %s %q vs %q", err, h, a.Header().Get(h), b.Header().Get(h))
			}
		}
	}
}
