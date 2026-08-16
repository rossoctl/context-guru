package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A published *Tenancy must be immutable: the request path reads MonthlyCapUSD,
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
	// The unlocked readers, as the request path has them (spendgate, captureContentFor,
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
					_ = first.MonthlyCapUSD + 1
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
		c.Label = "laptop-" + string(rune('a'+i%26))
		c.MonthlyCapUSD = float64(i)
		c.CaptureContent = i%2 == 0
		got, err := src.ForTenant(&c)
		if err != nil {
			t.Fatal(err)
		}
		if got.MonthlyCapUSD != float64(i) {
			t.Fatalf("refresh did not take effect: cap = %v, want %v", got.MonthlyCapUSD, float64(i))
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

// Bounding the limiter's map must not throw away every OTHER key's rate window and
// cached spend. Before the fix, entry 10001 replaced the whole map with itself.
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

	c := newSpendCache(time.Minute)
	load := func(string) (float64, error) { return 1, nil }
	for i := 0; i < maxLimiterKeys+1; i++ {
		if _, err := c.get("t"+strconv.Itoa(i), load); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	n = c.values.ll.Len()
	c.mu.Unlock()
	if n != maxLimiterKeys {
		t.Fatalf("spend cache holds %d entries, want the bound of %d", n, maxLimiterKeys)
	}
	calls := 0
	counted := func(string) (float64, error) { calls++; return 1, nil }
	for i := maxLimiterKeys - 100; i < maxLimiterKeys; i++ {
		if _, err := c.get("t"+strconv.Itoa(i), counted); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d recently cached spend values were discarded by the bound", calls)
	}
}

// --- registration gating (F2) ------------------------------------------------

// registerVia posts a registration from a given client address.
func registerVia(t *testing.T, f *hostedFixture, email, code, remote string) int {
	t.Helper()
	body := `{"email":"` + email + `","label":"l"`
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

// An account is a spending credential against the operator's key, so minting one is
// off unless the operator turned it on. Default-closed is the fix: before it, any
// caller that could reach the port could create unlimited accounts.
func TestRegistrationClosedByDefault(t *testing.T) {
	t.Setenv(envRegisterMode, "") // unset restores after the test; empty is the closed default
	f := newHostedFixture(t, "up", "openai")
	if code := registerVia(t, f, "a@ibm.com", "", ""); code != http.StatusForbidden {
		t.Fatalf("register with no CG_REGISTER = %d, want 403", code)
	}
	// Nothing was created, so the same email is still free once registration is opened.
	t.Setenv(envRegisterMode, "open")
	if code := registerVia(t, f, "a@ibm.com", "", ""); code != http.StatusCreated {
		t.Fatalf("register in open mode = %d, want 201", code)
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
