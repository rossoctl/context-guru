package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// Email + password + emailed code, over HTTP.
//
// These tests go through the mux rather than the registry, because the properties that
// matter most here are HTTP-layer ones: which phase sets the cookie, what a response
// body is allowed to contain, and whether a failure is distinguishable from another.

func cookiesFor(w *httptest.ResponseRecorder) []*http.Cookie {
	var jar []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashCookie && c.Value != "" {
			jar = append(jar, c)
		}
	}
	return jar
}

// Phase one must create nothing usable: no token to send traffic with, and no session
// to read the dashboard with. If registration alone signed the browser in, the code
// would be decoration.
func TestRegistrationIsNotCompleteUntilTheCodeIsEntered(t *testing.T) {
	f := ctlFixture(t)
	w, out := f.do(t, "POST", "/api/register",
		`{"email":"a@ibm.com","label":"laptop","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	if out["token"] != nil {
		t.Error("registration handed out a proxy token before the address was verified")
	}
	if len(cookiesFor(w)) != 0 {
		t.Error("registration opened a session before the address was verified")
	}
	// The UI needs the deadline to draw a countdown, and it must be an absolute time
	// rather than a duration the client has to start its own clock from.
	exp, ok := out["code_expires_at"].(float64)
	if !ok || exp <= 0 {
		t.Fatalf("no code_expires_at in the response: %v", out)
	}
	if secs, _ := out["code_valid_secs"].(float64); int(secs) != int(tenant.CodeTTL.Seconds()) {
		t.Errorf("code_valid_secs = %v, want %v", secs, tenant.CodeTTL.Seconds())
	}

	// A wrong code is refused, and says nothing about whether the address exists.
	code := f.lastCode(t)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	w, _ = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+wrong+`"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong code = %d, want 401", w.Code)
	}
	w, unknown := f.do(t, "POST", "/api/verify", `{"email":"nobody@ibm.com","code":"`+wrong+`"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an unknown address = %d, want 401", w.Code)
	}
	if unknown["error"] != "that code is not valid" {
		t.Errorf("an unknown address is distinguishable from a wrong code: %v", unknown["error"])
	}

	// The right one completes it: token once, session opened.
	w, out = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+code+`"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("verify = %d %s", w.Code, w.Body)
	}
	if tok, _ := out["token"].(string); !strings.HasPrefix(tok, "cg_live_") {
		t.Fatalf("verification did not mint the first token: %v", out)
	}
	jar := cookiesFor(w)
	if len(jar) == 0 {
		t.Fatal("verification did not open a session")
	}
	if w, _ := f.do(t, "GET", "/api/me", "", jar); w.Code != http.StatusOK {
		t.Fatalf("/api/me after verification = %d", w.Code)
	}
	// One-time: the same code cannot be replayed into a second session.
	w, _ = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+code+`"}`, nil)
	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Error("the registration code was accepted twice")
	}
}

// Signing in is two phases for the same reason: a correct password alone must not be a
// signed-in browser.
func TestLoginNeedsTheSecondFactor(t *testing.T) {
	f := ctlFixture(t)
	if w, _ := f.signUp(t, "a@ibm.com", "laptop"); w.Code != http.StatusCreated {
		t.Fatalf("signUp = %d %s", w.Code, w.Body)
	}
	w, out := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	if len(cookiesFor(w)) != 0 {
		t.Fatal("the password alone opened a session")
	}
	if out["next"] != "verify" {
		t.Errorf("login did not ask for a code: %v", out)
	}
	if out["code_expires_at"] == nil {
		t.Error("login did not tell the UI when the code expires")
	}
	w, _ = f.do(t, "POST", "/api/verify",
		`{"email":"a@ibm.com","code":"`+f.lastCode(t)+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", w.Code, w.Body)
	}
	if len(cookiesFor(w)) == 0 {
		t.Fatal("the second factor did not open a session")
	}
	// A LOGIN code must not mint a second token — that is the registration branch.
	if _, out := f.do(t, "GET", "/api/me", "", cookiesFor(w)); len(out["tokens"].([]any)) != 1 {
		t.Errorf("signing in changed the token count: %v", out["tokens"])
	}
}

// The wrong password is refused, and — the part that matters — it is refused in a way
// that does not say whether the account exists.
func TestWrongPasswordIsRefusedAndDoesNotEnumerate(t *testing.T) {
	f := ctlFixture(t)
	f.signUp(t, "a@ibm.com", "laptop")
	w1, out1 := f.do(t, "POST", "/api/login", `{"email":"a@ibm.com","password":"not-the-password"}`, nil)
	w2, out2 := f.do(t, "POST", "/api/login", `{"email":"ghost@ibm.com","password":"not-the-password"}`, nil)
	if w1.Code != http.StatusUnauthorized || w2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, unknown address = %d, want 401 for both", w1.Code, w2.Code)
	}
	if out1["error"] != out2["error"] {
		t.Errorf("an existing account is distinguishable: %q vs %q", out1["error"], out2["error"])
	}
	if len(cookiesFor(w1)) != 0 {
		t.Fatal("a failed sign-in set a cookie")
	}
}

// Without a cap, a 6-digit code is a formality: 10^6 is minutes of guessing. The cap
// lives in tenant.VerifyCode; this pins that the HTTP layer surfaces it and that the
// code is dead afterwards.
func TestCodeGuessesAreCappedOverHTTP(t *testing.T) {
	f := ctlFixture(t)
	f.do(t, "POST", "/api/register",
		`{"email":"a@ibm.com","label":"l","password":"`+testPassword+`"}`, nil)
	good := f.lastCode(t)
	wrong := "000000"
	if wrong == good {
		wrong = "111111"
	}
	for i := 0; i < tenant.MaxCodeAttempts; i++ {
		w, _ := f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+wrong+`"}`, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i+1, w.Code)
		}
	}
	// The real code is now void too — the challenge was destroyed, not just refused.
	w, out := f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+good+`"}`, nil)
	if w.Code == http.StatusCreated {
		t.Fatal("the code survived the attempt cap")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "not valid") {
		t.Errorf("unhelpful message after lockout: %q", msg)
	}
	if len(cookiesFor(w)) != 0 {
		t.Fatal("a locked-out verification set a cookie")
	}
}

// spendSignInBudget signs an account up and burns its sign-in budget, returning the
// fixture whose limiter has just refused an attempt.
//
// The retry inside is not flake tolerance. It is the limiter's actual shape: the window is
// a FIXED calendar minute — `tenantLimiter.windowStart` is `now.Truncate(time.Minute)` and
// the count resets when the minute rolls over (see limits.go) — not a sliding minute
// starting at the first attempt. A probe placed across a boundary therefore spends its
// attempts out of two consecutive budgets, four and then four, and neither half reaches
// the bound of five. It observes "unlimited" from a limiter behaving exactly as designed,
// and the only way to tell that apart from a real bypass is to notice the crossing. A
// fresh fixture gets a fresh limiter, and two crossings in a row does not happen.
//
// Worth stating what this makes true of the limiter itself, since a reader could take the
// assertion below for something stronger: a fixed window permits up to TWICE the nominal
// rate in a burst straddling a boundary — ten attempts in a couple of seconds, not five.
// On a sign-in path where every attempt already pays a 64 MiB argon2 and the mailed code
// is destroyed after five wrong guesses, that factor of two is not what decides whether
// brute force is viable, so it is documented rather than designed out.
func spendSignInBudget(t *testing.T) *hostedFixture {
	t.Helper()
	const attempts = passwordAttemptsPerMinute + 3
	for try := 0; ; try++ {
		f := ctlFixture(t)
		f.signUp(t, "a@ibm.com", "laptop")
		minute, start := time.Now().Truncate(time.Minute), time.Now()
		for i := 0; i < attempts; i++ {
			w, _ := f.do(t, "POST", "/api/login",
				`{"email":"a@ibm.com","password":"wrong-one-here"}`, nil)
			if w.Code == http.StatusTooManyRequests {
				return f
			}
		}
		elapsed := time.Since(start)
		// The limiter is charged BEFORE the argon2 verify (deliberately — 64 MiB per
		// attempt is itself an amplifier), so each allowed attempt pays a full KDF. A
		// machine contended enough to spend a whole minute on these is reporting itself,
		// not the code.
		if elapsed >= time.Minute {
			t.Skipf("inconclusive: %d attempts took %v, longer than the window itself. "+
				"Re-run on a less loaded machine.", attempts, elapsed)
		}
		if !time.Now().Truncate(time.Minute).Equal(minute) && try < 2 {
			t.Logf("the minute rolled over mid-probe (%v), so the budget reset underneath "+
				"it; retrying on a fresh limiter", elapsed)
			continue
		}
		t.Fatalf("%d password attempts went unlimited in %v, inside one window", attempts, elapsed)
	}
}

// And the rate limit is the second layer: it bounds how fast an attacker can request
// fresh codes to buy more guesses. Applied per email AND per client address.
func TestSignInAttemptsAreRateLimited(t *testing.T) {
	f := spendSignInBudget(t)
	// A DIFFERENT address from the same client is limited too, because the client
	// address has its own bucket — otherwise one host grinds the whole directory.
	w, _ := f.do(t, "POST", "/api/login", `{"email":"b@ibm.com","password":"wrong-one-here"}`, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a second address from a limited client = %d, want 429", w.Code)
	}
	// A different client address gets its own budget — a fresh address as well, since
	// a@ibm.com's own email bucket is spent and would refuse from anywhere.
	r := httptest.NewRequest("POST", "/api/login",
		strings.NewReader(`{"email":"c@ibm.com","password":"wrong-one-here"}`))
	r.Header.Set("content-type", "application/json")
	r.RemoteAddr = "203.0.113.44:1111"
	rw := httptest.NewRecorder()
	f.mux.ServeHTTP(rw, r)
	if rw.Code == http.StatusTooManyRequests {
		t.Error("the limit is global rather than per client address")
	}
}

// The owner's fourth requirement: a user signs in from several machines at once, sees
// them, and revokes one.
func TestSeveralMachinesAndPerSessionRevokeOverHTTP(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "laptop")
	first := cookiesFor(w)
	if len(first) == 0 {
		t.Fatal("registration did not sign in")
	}
	w, _ = f.signIn(t, "a@ibm.com")
	second := cookiesFor(w)
	if len(second) == 0 {
		t.Fatal("the second sign-in did not open a session")
	}
	if first[0].Value == second[0].Value {
		t.Fatal("the second machine got the first machine's cookie")
	}
	// Both work at once: signing in on a desktop must not sign the laptop out.
	for i, jar := range [][]*http.Cookie{first, second} {
		if w, _ := f.do(t, "GET", "/api/me", "", jar); w.Code != http.StatusOK {
			t.Fatalf("session %d = %d, want 200", i, w.Code)
		}
	}

	w, out := f.do(t, "GET", "/api/me/sessions", "", second)
	if w.Code != http.StatusOK {
		t.Fatalf("sessions = %d %s", w.Code, w.Body)
	}
	list, _ := out["sessions"].([]any)
	if len(list) != 2 {
		t.Fatalf("%d sessions listed, want 2: %v", len(list), out)
	}
	var target string
	currents := 0
	for _, raw := range list {
		s, _ := raw.(map[string]any)
		if s["current"] == true {
			currents++
			continue
		}
		target, _ = s["id"].(string)
		if s["last_seen_at"] == nil || s["created_at"] == nil {
			t.Errorf("a session row is missing its timestamps: %v", s)
		}
	}
	if currents != 1 {
		t.Errorf("%d sessions marked current, want exactly 1", currents)
	}
	if target == "" {
		t.Fatal("the other session has no revocable id")
	}
	// A session id must never be usable AS a session.
	if strings.Contains(w.Body.String(), first[0].Value) || strings.Contains(w.Body.String(), second[0].Value) {
		t.Fatal("the session list echoed a live cookie")
	}

	if w, _ := f.do(t, "DELETE", "/api/me/sessions/"+target, "", second); w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "GET", "/api/me", "", first); w.Code != http.StatusUnauthorized {
		t.Errorf("the revoked machine is still signed in = %d", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/me", "", second); w.Code != http.StatusOK {
		t.Error("revoking the other machine signed this one out too")
	}
	if w, _ := f.do(t, "DELETE", "/api/me/sessions/"+target, "", second); w.Code != http.StatusNotFound {
		t.Error("revoking an already-revoked session did not 404")
	}
}

// A session belongs to its tenant. Guessing another account's handle must not revoke it.
func TestRevokingAnotherTenantsSessionIsRefused(t *testing.T) {
	f := ctlFixture(t)
	victim, _ := f.signUp(t, "victim@ibm.com", "laptop")
	attacker, _ := f.signUp(t, "attacker@ibm.com", "laptop")
	vJar, aJar := cookiesFor(victim), cookiesFor(attacker)
	_, out := f.do(t, "GET", "/api/me/sessions", "", vJar)
	list, _ := out["sessions"].([]any)
	sid, _ := list[0].(map[string]any)["id"].(string)

	if w, _ := f.do(t, "DELETE", "/api/me/sessions/"+sid, "", aJar); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke = %d, want 404", w.Code)
	}
	if w, _ := f.do(t, "GET", "/api/me", "", vJar); w.Code != http.StatusOK {
		t.Fatal("another account revoked this session")
	}
}

// Once an account has a password, a proxy token must not be a shortcut past the second
// factor — a token is the credential most likely to be sitting in a CI log.
func TestTokenSignInIsRefusedOnceAPasswordExists(t *testing.T) {
	f := ctlFixture(t)
	_, out := f.signUp(t, "a@ibm.com", "laptop")
	tok, _ := out["token"].(string)
	if tok == "" {
		t.Fatal("no token to test with")
	}
	w, _ := f.do(t, "POST", "/api/login", `{"token":"`+tok+`"}`, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("token sign-in on a password account = %d, want 403", w.Code)
	}
	if len(cookiesFor(w)) != 0 {
		t.Fatal("a refused token sign-in still set a cookie")
	}
	// But the token still works for what it is FOR: sending traffic to the proxy. The
	// two credentials stay separate rather than one replacing the other.
	if _, err := f.reg.Resolve(tok); err != nil {
		t.Errorf("the proxy token stopped working: %v", err)
	}
}

// A pre-password account (created before this flow existed, or by a manager) still
// signs in with its token, because there is nothing else it has.
func TestTokenSignInStillWorksWithoutAPassword(t *testing.T) {
	f := ctlFixture(t)
	_, tok := f.register(t, "legacy@ibm.com")
	w, _ := f.do(t, "POST", "/api/login", `{"token":"`+tok+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy token sign-in = %d %s", w.Code, w.Body)
	}
	if len(cookiesFor(w)) == 0 {
		t.Fatal("legacy token sign-in did not open a session")
	}
}

// Nothing anywhere may print a password, a password hash, or a verification code. The
// log is the realistic leak: it is shipped, aggregated, and read by people who are not
// the account's owner.
func TestNoPasswordOrCodeIsEverLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := ctlFixture(t)
	var bodies strings.Builder
	record := func(w *httptest.ResponseRecorder) { bodies.WriteString(w.Body.String()) }

	w, _ := f.do(t, "POST", "/api/register",
		`{"email":"a@ibm.com","label":"l","password":"`+testPassword+`"}`, nil)
	record(w)
	regCode := f.lastCode(t)
	w, _ = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+regCode+`"}`, nil)
	record(w)
	jar := cookiesFor(w)

	// A failed attempt too: an error path is where a value normally slips into a log.
	w, _ = f.do(t, "POST", "/api/login", `{"email":"a@ibm.com","password":"the-wrong-password"}`, nil)
	record(w)
	w, _ = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"000000"}`, nil)
	record(w)
	w, _ = f.do(t, "POST", "/api/login", `{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil)
	record(w)
	loginCode := f.lastCode(t)
	w, _ = f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+loginCode+`"}`, nil)
	record(w)
	w, _ = f.do(t, "GET", "/api/me/sessions", "", jar)
	record(w)

	// Sanity: the sink really did receive codes, so a clean log is not just an empty one.
	sink, err := os.ReadFile(os.Getenv(envMailDevSink))
	if err != nil || !strings.Contains(string(sink), regCode) {
		t.Fatalf("the mail sink did not receive the code, so this test proves nothing: %v", err)
	}
	for what, secret := range map[string]string{
		"the password":          testPassword,
		"a rejected password":   "the-wrong-password",
		"the registration code": regCode,
		"the login code":        loginCode,
		"a password hash":       "$argon2id$",
	} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%s appears in the server log", what)
		}
		if strings.Contains(bodies.String(), secret) {
			t.Errorf("%s appears in an HTTP response body", what)
		}
	}
}

// With neither a relay nor a dev sink, registration must FAIL and say so. Silently
// creating an account whose code went nowhere is the worst of the three outcomes.
func TestRegistrationRefusesWhenThereIsNoMailPath(t *testing.T) {
	f := ctlFixture(t)
	t.Setenv(envMailDevSink, "")
	t.Setenv(envSMTPHost, "")
	w, out := f.do(t, "POST", "/api/register",
		`{"email":"a@ibm.com","label":"l","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("register with no mail path = %d, want 503", w.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, envSMTPHost) {
		t.Errorf("the error does not name the missing setting: %q", msg)
	}
}

// The dev sink writes a file only the owner can read. It is a development escape
// hatch, and a world-readable file of live second factors is not one.
func TestMailSinkFileIsOwnerOnly(t *testing.T) {
	f := ctlFixture(t)
	f.do(t, "POST", "/api/register",
		`{"email":"a@ibm.com","label":"l","password":"`+testPassword+`"}`, nil)
	st, err := os.Stat(os.Getenv(envMailDevSink))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mail sink mode = %o, want 600", perm)
	}
}

// Verifying is rate-limited by client address as well, so a botnet-free attacker cannot
// spend an unbounded number of codes from one host.
func TestVerifyAttemptsAreRateLimited(t *testing.T) {
	f := ctlFixture(t)
	f.signUp(t, "a@ibm.com", "laptop")
	limited := false
	for i := 0; i < codeAttemptsPerMinute+4; i++ {
		w, _ := f.do(t, "POST", "/api/verify",
			`{"email":"u`+strconv.Itoa(i)+`@ibm.com","code":"000000"}`, nil)
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("%d+ code submissions went unlimited", codeAttemptsPerMinute+4)
	}
}
