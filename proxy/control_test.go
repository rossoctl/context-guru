package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// ctlFixture is a hosted proxy with the control plane mounted.
func ctlFixture(t *testing.T) *hostedFixture {
	t.Helper()
	// Self-registration is off unless the operator opts in (see control.go). These
	// tests exercise the registration FLOW, so they opt in; the gating itself is
	// covered in security_test.go.
	t.Setenv(envRegisterMode, "open")
	f := newHostedFixture(t, "up", "anthropic")
	// newHostedFixture builds the mux before this test's assertions; the control routes
	// are registered by Mux() already, so nothing extra is needed here.
	return f
}

// testPassword is the password every fixture account uses. Not a credential: it never
// leaves this test binary, and the accounts live in an in-memory database discarded
// when the test ends.
const testPassword = "fixture-password-1"

// signUp runs the WHOLE two-phase registration: POST /api/register, read the code out
// of the mail sink, POST /api/verify. It returns the verify response, which is the one
// carrying the session cookie and the first token — so it is a drop-in for what the
// old single-step register returned.
//
// Reading the code from the sink FILE rather than from the registry is deliberate: it
// means these tests fail if the mail path stops producing a usable code, which is the
// half of the flow a test reaching straight into the database would never touch.
func (f *hostedFixture) signUp(t *testing.T, email, label string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w, out := f.do(t, "POST", "/api/register",
		`{"email":"`+email+`","label":"`+label+`","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusCreated {
		return w, out
	}
	return f.do(t, "POST", "/api/verify",
		`{"email":"`+email+`","code":"`+f.lastCode(t)+`"}`, nil)
}

// signIn runs the two-phase login for an already-registered account.
func (f *hostedFixture) signIn(t *testing.T, email string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w, out := f.do(t, "POST", "/api/login",
		`{"email":"`+email+`","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusOK {
		return w, out
	}
	return f.do(t, "POST", "/api/verify",
		`{"email":"`+email+`","code":"`+f.lastCode(t)+`"}`, nil)
}

// lastCode returns the most recent 6-digit code written to the mail sink.
func (f *hostedFixture) lastCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv(envMailDevSink))
	if err != nil {
		t.Fatalf("mail sink: %v", err)
	}
	m := regexp.MustCompile(`\b\d{6}\b`).FindAllString(string(b), -1)
	if len(m) == 0 {
		t.Fatalf("no code in the mail sink:\n%s", b)
	}
	return m[len(m)-1]
}

// do issues a control-plane call, carrying cookies through a jar.
func (f *hostedFixture) do(t *testing.T, method, path, body string, cookies []*http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	r.Header.Set("content-type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestRegisterIssuesTokenOnceAndSignsIn(t *testing.T) {
	f := ctlFixture(t)
	w, out := f.signUp(t, "a@ibm.com", "laptop")
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, "cg_live_") {
		t.Fatalf("no token issued: %v", out)
	}
	if out["warning"] == nil {
		t.Error("the response does not warn that the token is shown once")
	}
	// Registration signs the user in, so the flow can go straight to Setup instead of
	// asking them to paste back the token they just received.
	var jar []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashCookie && c.Value != "" {
			jar = append(jar, c)
		}
	}
	if len(jar) == 0 {
		t.Fatal("registration did not set a session cookie")
	}
	// The cookie must not be readable by script, and must be same-site.
	c := jar[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie is not HttpOnly+Lax: %+v", c)
	}
	if c.Value == tok {
		t.Error("the session cookie IS the proxy token; they must be separate credentials")
	}

	w, me := f.do(t, "GET", "/api/me", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/me = %d %s", w.Code, w.Body)
	}
	if me["base_url"] == nil {
		t.Error("/api/me does not tell the UI which base URL to configure")
	}
	toks, _ := me["tokens"].([]any)
	if len(toks) != 1 {
		t.Errorf("expected 1 token, got %v", me["tokens"])
	}
	// The token list must never carry a usable secret.
	if strings.Contains(w.Body.String(), tok) {
		t.Error("/api/me echoed the plaintext token back")
	}
}

func TestControlWritesRequireTheCookieNotAToken(t *testing.T) {
	f := ctlFixture(t)
	_, tok := f.register(t, "a@ibm.com")

	// A proxy token must NOT authenticate account changes: it lives in agent config and
	// CI environments, so accepting it here would mean anything that can send traffic
	// can also rewrite the account.
	r := httptest.NewRequest("PUT", "/api/me", strings.NewReader(`{"capture_content":true}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a proxy token was accepted for an account write: %d", w.Code)
	}
	for _, p := range []string{"/api/me", "/api/me/audit", "/api/options", "/api/tenants",
		"/api/feedback"} {
		w, _ := f.do(t, "GET", p, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a cookie = %d, want 401", p, w.Code)
		}
	}
}

func TestSettingsValidationAndUpstreamAllowList(t *testing.T) {
	f := ctlFixture(t)
	// The fixture's ManagerEmail: config_yaml on PUT /api/me is a manager's field, so a
	// plain account is refused before the validation under test is ever reached.
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	// A config that does not build is refused, and the message names the problem.
	w, out := f.do(t, "PUT", "/api/me", `{"config_yaml":"pipeline: [nope_not_real]\n"}`, jar)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid config = %d, want 400", w.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "nope_not_real") {
		t.Errorf("the error does not name the offending component: %q", msg)
	}

	// An upstream outside the operator's allow-list is refused at SAVE time, not just
	// at request time — otherwise a user breaks their own agent and finds out later.
	w, _ = f.do(t, "PUT", "/api/me", `{"up_anthropic":"http://169.254.169.254"}`, jar)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a URL as the upstream name = %d, want 400", w.Code)
	}

	// A valid save sticks.
	w, out = f.do(t, "PUT", "/api/me", `{"config_yaml":"pipeline: [format]\nmode: sync\n","capture_content":true}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("valid save = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	if tn["capture_content"] != true {
		t.Errorf("capture_content did not stick: %v", tn)
	}
}

// A user must not be able to raise their own cap or grant themselves manager.
func TestSelfEscalationIsRefusedOverHTTP(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()
	me := f.mustMe(t, jar)

	// PUT /api/me has no quota or role field at all, so the strict decoder rejects the
	// attempt outright rather than silently ignoring it.
	w, _ = f.do(t, "PUT", "/api/me", `{"max_rows":999999}`, jar)
	if w.Code != http.StatusBadRequest {
		t.Errorf("self quota raise via /api/me = %d, want 400", w.Code)
	}
	// And the manager route refuses a non-manager.
	w, _ = f.do(t, "PATCH", "/api/tenants/"+me, `{"max_rows":999999}`, jar)
	if w.Code != http.StatusForbidden {
		t.Errorf("self quota raise via the manager route = %d, want 403", w.Code)
	}
	w, _ = f.do(t, "PATCH", "/api/tenants/"+me, `{"role":"manager"}`, jar)
	if w.Code != http.StatusForbidden {
		t.Errorf("self promotion = %d, want 403", w.Code)
	}
}

func (f *hostedFixture) mustMe(t *testing.T, jar []*http.Cookie) string {
	t.Helper()
	_, out := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := out["tenant"].(map[string]any)
	id, _ := tn["id"].(string)
	if id == "" {
		t.Fatalf("could not read own tenant id: %v", out)
	}
	return id
}

func TestManagerCanAdministerAndAuditRecordsIt(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "a@ibm.com", "l")
	userJar := w.Result().Cookies()
	target := f.mustMe(t, userJar)

	w, _ = f.do(t, "GET", "/api/tenants", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager roster = %d", w.Code)
	}
	w, out := f.do(t, "PATCH", "/api/tenants/"+target, `{"max_rows":1250,"disabled":true}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager patch = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	if tn["max_rows"] != 1250.0 || tn["disabled"] != true {
		t.Errorf("patch did not apply: %v", tn)
	}
	// Disabling must also end the browser session, or a disabled user keeps reading
	// their history until the cookie lapses.
	w, _ = f.do(t, "GET", "/api/me", "", userJar)
	if w.Code == http.StatusOK {
		t.Error("a disabled tenant still had a live dashboard session")
	}
	// The change is on the record, with who did it.
	w, out = f.do(t, "GET", "/api/me/audit?tenant="+target, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("audit = %d", w.Code)
	}
	rows, _ := out["audit"].([]any)
	if len(rows) < 2 {
		t.Errorf("audit recorded %d changes, want at least 2", len(rows))
	}
}

// A manager can reissue a token for someone who lost theirs — the only recovery path,
// since tokens are stored hashed.
func TestManagerCanReissueAToken(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "a@ibm.com", "l")
	target := f.mustMe(t, w.Result().Cookies())

	w, out := f.do(t, "POST", "/api/tenants/"+target+"/tokens", `{"label":"reissued"}`, mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("reissue = %d %s", w.Code, w.Body)
	}
	fresh, _ := out["token"].(string)
	if !strings.HasPrefix(fresh, "cg_live_") {
		t.Fatalf("no token returned: %v", out)
	}
	// The reissued token must actually work for traffic.
	if resp := f.post("/anthropic/v1/messages", fresh, ""); resp.Code != http.StatusOK {
		t.Errorf("reissued token could not proxy: %d %s", resp.Code, resp.Body)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()
	if w2, _ := f.do(t, "GET", "/api/me", "", jar); w2.Code != http.StatusOK {
		t.Fatalf("pre-logout /api/me = %d", w2.Code)
	}
	if w2, _ := f.do(t, "POST", "/api/logout", "", jar); w2.Code != http.StatusOK {
		t.Fatalf("logout = %d", w2.Code)
	}
	if w2, _ := f.do(t, "GET", "/api/me", "", jar); w2.Code != http.StatusUnauthorized {
		t.Errorf("the session survived logout: %d", w2.Code)
	}
}

// /api/options must offer only what the server will accept, and must not leak the
// operator's URLs or credential variable names to a tenant.
func TestOptionsExposesNamesNotSecrets(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()
	w, out := f.do(t, "GET", "/api/options", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("options = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "TEST_UPSTREAM_KEY") {
		t.Error("/api/options leaked the credential's environment variable name")
	}
	if strings.Contains(body, "127.0.0.1") || strings.Contains(body, "http://") {
		t.Error("/api/options leaked the upstream base URL")
	}
	if out["default_config"] == nil {
		t.Error("/api/options does not offer the recommended default config")
	}
}

// The single-tenant proxy must not mount the control plane at all: there are no
// accounts, so these routes would be a confusing 401 rather than an honest 404.
func TestControlPlaneAbsentInSingleTenantMode(t *testing.T) {
	h := New(components.NewPipeline(nil, nil), store.NewMemory(store.Options{}), nil, Options{})
	defer h.Close()
	mux := h.Mux()
	for _, p := range []string{"/api/register", "/api/login", "/api/me", "/api/tenants",
		"/api/feedback"} {
		r := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s in single-tenant mode = %d, want 404", p, w.Code)
		}
	}
}

var _ = tenant.DefaultConfigYAML

// The settings page has to be able to tell "I chose this" from "I am following the
// server default", and it must be able to show what is ACTUALLY running in both cases —
// a tracking tenant's stored config is empty, and rendering that as a blank form reads
// as "my configuration is gone".
func TestMeCarriesTheEffectiveConfigAndWhoOwnsIt(t *testing.T) {
	f := ctlFixture(t)
	// A manager, because customising through PUT /api/me is a manager's action now; the
	// inherited/own distinction it asserts is the same one a user's page reads.
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	_, out := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := out["tenant"].(map[string]any)
	if tn["config_yaml"] != "" {
		t.Errorf("config_yaml = %q, want empty for a tenant tracking the default", tn["config_yaml"])
	}
	if tn["config_inherited"] != true {
		t.Errorf("config_inherited = %v, want true", tn["config_inherited"])
	}
	if got := tn["effective_config_yaml"]; got != tenant.DefaultConfigYAML {
		t.Errorf("effective_config_yaml = %q, want the server default", got)
	}

	// Customising stores the tenant's own copy; both fields then agree and nothing is
	// inherited any more.
	mine := "pipeline: [format]\nmode: observe\n"
	body, _ := json.Marshal(map[string]any{"config_yaml": mine})
	w, out = f.do(t, "PUT", "/api/me", string(body), jar)
	if w.Code != http.StatusOK {
		t.Fatalf("customise = %d %s", w.Code, w.Body)
	}
	tn, _ = out["tenant"].(map[string]any)
	if tn["config_yaml"] != mine || tn["effective_config_yaml"] != mine ||
		tn["config_inherited"] != false {
		t.Fatalf("after customising: %v", tn)
	}

	// And clearing it puts them back to tracking, with the effective document still
	// shown so the page is never blank.
	w, out = f.do(t, "PUT", "/api/me", `{"config_yaml":""}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("back to tracking = %d %s", w.Code, w.Body)
	}
	tn, _ = out["tenant"].(map[string]any)
	if tn["config_yaml"] != "" || tn["config_inherited"] != true ||
		tn["effective_config_yaml"] != tenant.DefaultConfigYAML {
		t.Fatalf("after clearing: %v", tn)
	}
}

// A manager reading someone else's row needs the same distinction: a blank
// Configuration column would look like a broken account rather than a tracking one.
func TestManagerRosterShowsTheEffectiveConfig(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	mgrJar := w.Result().Cookies()
	f.signUp(t, "a@ibm.com", "l")

	w, out := f.do(t, "GET", "/api/tenants", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("roster = %d %s", w.Code, w.Body)
	}
	rows, _ := out["tenants"].([]any)
	if len(rows) != 2 {
		t.Fatalf("roster has %d rows", len(rows))
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["config_inherited"] != true ||
			row["effective_config_yaml"] != tenant.DefaultConfigYAML {
			t.Errorf("roster row does not carry the effective config: %v", row)
		}
	}
}

// A zero time must serialise as 0, not as year 1.
//
// time.Time{}.UnixMilli() is -62135596800000, which is truthy in JavaScript — so shipping
// it raw made every freshly minted token render as REVOKED in the settings page, and
// "never used" render as a date in year 1. Cheap bug, expensive symptom.
func TestZeroTimesSerialiseAsZero(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()

	// /api/me rather than /api/whoami: whoami is mounted by dash.API, which this fixture
	// does not enable. Both render token views through the same tokenViews, so this covers
	// the conversion either way.
	_, out := f.do(t, "GET", "/api/me", "", jar)
	toks, _ := out["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("expected one token, got %v", out["tokens"])
	}
	tok, _ := toks[0].(map[string]any)
	if got, _ := tok["revoked_at"].(float64); got != 0 {
		t.Errorf("revoked_at for a live token = %v, want 0 (a negative reads as revoked)", got)
	}
	if got, _ := tok["last_used_at"].(float64); got < 0 {
		t.Errorf("last_used_at = %v; a negative renders as a year-1 date instead of 'never'", got)
	}
	if got, _ := tok["created_at"].(float64); got <= 0 {
		t.Errorf("created_at = %v, want a real timestamp", got)
	}
	tn, _ := out["tenant"].(map[string]any)
	if got, _ := tn["last_seen_at"].(float64); got < 0 {
		t.Errorf("tenant last_seen_at = %v, want 0 or a real timestamp", got)
	}
}

// The gate draws itself from /api/whoami, so the mode it reports has to be the mode the
// server enforces. If these two ever disagree the UI shows a form that 403s on submit,
// or hides one that would have worked.
func TestDashWhoamiReportsRegistrationMode(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	for _, tc := range []struct{ env, want string }{
		{"", "open"},
		{"nonsense", "open"},
		{"closed", "closed"},
		{"invite", "invite"},
		{"INVITE", "invite"},
		{"open", "open"},
		{" open ", "open"},
	} {
		t.Setenv(envRegisterMode, tc.env)
		out, ok := f.h.DashWhoami()(httptest.NewRequest(http.MethodGet, "/api/whoami", nil)).(map[string]any)
		if !ok {
			t.Fatalf("whoami did not return an object")
		}
		if out["register"] != tc.want {
			t.Errorf("CG_REGISTER=%q: whoami register = %v, want %q", tc.env, out["register"], tc.want)
		}
		// Unauthenticated is exactly when the gate needs this, so it must be present.
		if out["authenticated"] != false {
			t.Errorf("CG_REGISTER=%q: expected an unauthenticated probe", tc.env)
		}
	}
}

// The mode is a policy name and never the secret that enforces it.
func TestDashWhoamiNeverLeaksTheInviteCode(t *testing.T) {
	f := newHostedFixture(t, "up", "anthropic")
	t.Setenv(envRegisterMode, "invite")
	t.Setenv(envRegisterCode, "correct-horse-battery-staple")
	out := f.h.DashWhoami()(httptest.NewRequest(http.MethodGet, "/api/whoami", nil))
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "correct-horse-battery-staple") {
		t.Fatalf("the invite code is in the whoami payload: %s", body)
	}
}

// The plaintext token exists for exactly one response, and then only as a hash. This
// test pins all three halves of that claim at once, because each is a different way for
// the same secret to escape:
//
//  1. every authenticated READ of the account afterwards — none may echo it;
//  2. captured transcript content — the redactor must scrub the shape;
//  3. a log record — the logging scrubber must scrub it too, whichever slot it lands in.
//
// The reveal on the Setup tab is the reason this matters more than it used to: the UI now
// renders the plaintext, so a regression that also puts it in a log line would put a live
// credential in Loki.
func TestFirstTokenIsRevealedOnceAndNowhereElse(t *testing.T) {
	f := ctlFixture(t)
	w, out := f.signUp(t, "reveal@ibm.com", "laptop")
	if w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	tok, _ := out["token"].(string)
	if !tenant.LooksLikeToken(tok) {
		t.Fatalf("registration did not reveal a token: %v", out)
	}
	var jar []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashCookie && c.Value != "" {
			jar = append(jar, c)
		}
	}

	// 1. Every read the account can perform. A later read that returns the plaintext
	//    would make "shown once" a lie, and the Setup tab's warning a lie with it.
	for _, path := range []string{"/api/me", "/api/me/sessions", "/api/me/audit", "/api/options"} {
		rw, _ := f.do(t, "GET", path, "", jar)
		if rw.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rw.Code, rw.Body)
		}
		if strings.Contains(rw.Body.String(), tok) {
			t.Errorf("GET %s returned the plaintext token", path)
		}
	}
	// Signing in again is the other way a client could ask for it back.
	rw, again := f.signIn(t, "reveal@ibm.com")
	if rw.Code != http.StatusOK {
		t.Fatalf("sign in = %d %s", rw.Code, rw.Body)
	}
	if again["token"] != nil {
		t.Errorf("signing in handed out a token: %v", again["token"])
	}

	// 2. Captured content. Headers are not recorded at all, but a user who pastes their
	//    own token into a prompt must not have it stored.
	if got := dash.RedactContent("run: curl -H 'x-context-guru-token: "+tok+"' https://x", 0); strings.Contains(got, tok) {
		t.Errorf("the capture redactor stored the token: %s", got)
	}

	// 3. A log record, in the two shapes a mistake takes: interpolated into the message,
	//    and passed as an attribute value.
	var buf bytes.Buffer
	lg := slog.New(logging.New(&buf, slog.LevelInfo, false))
	lg.Info("cg.auth token="+tok, "presented", tok)
	if strings.Contains(buf.String(), tok) {
		t.Errorf("the log scrubber let the token through: %s", buf.String())
	}
}

// The settings page posts FIELDS, and the round trip has to survive the document shape that
// broke it before: a pipeline written as a YAML block sequence. The browser rewrote that line
// in place and orphaned the items under it, so every save answered
// "config: yaml: line 3: did not find expected key" and the stored document stayed broken for
// the next attempt. Two accounts were stuck there.
func TestSettingsFieldsSaveOverABlockSequencePipeline(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	// Store the shape the old writer could not edit.
	w, out := f.do(t, "PUT", "/api/me",
		`{"config_yaml":"mode: sync\npipeline:\n  - format\n  - extract\n"}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("storing a block-sequence pipeline = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	eff, _ := tn["effective_config"].(map[string]any)
	if eff == nil {
		t.Fatal("no effective_config for the settings page to render")
	}
	if got := eff["pipeline"]; fmt.Sprint(got) != "[format extract]" {
		t.Fatalf("the block sequence did not reach the form as fields: %v", got)
	}

	// Now save through the fields, turning the compaction model on.
	body := `{"config":{"pipeline":["format","extract"],"mode":"sync","extract_llm":` +
		`{"per_output":true,"cold_enabled":true,"size_trigger":false,"min_tokens":2000,` +
		`"max_per_request":2,"max_per_session":20,"aggressiveness":"medium","context":"recent",` +
		`"context_messages":7,"cold_min_tokens":1000}}}`
	w, out = f.do(t, "PUT", "/api/me", body, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("field save = %d %s", w.Code, w.Body)
	}
	tn, _ = out["tenant"].(map[string]any)
	eff, _ = tn["effective_config"].(map[string]any)
	x, _ := eff["extract_llm"].(map[string]any)
	if x == nil || x["per_output"] != true || x["cold_enabled"] != true {
		t.Errorf("the switches did not stick: %v", eff)
	}
	if !strings.Contains(fmt.Sprint(eff["pipeline"]), "extract_llm") {
		t.Errorf("the component is configured but not in the pipeline: %v", eff["pipeline"])
	}

	// A bad value is a 400 naming the field, not a key the component silently ignores.
	w, out = f.do(t, "PUT", "/api/me",
		`{"config":{"mode":"sync","extract_llm":{"per_output":true,"aggressiveness":"very","context":"recent"}}}`, jar)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a bad aggressiveness = %d, want 400", w.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "aggressiveness") {
		t.Errorf("the error does not name the field: %q", msg)
	}
}

// A plain account cannot set the compaction configuration through the fields either — a
// second route to a manager's field would be the same hole with a different name.
func TestSettingsFieldsAreAManagersField(t *testing.T) {
	f := ctlFixture(t)
	_, _ = f.register(t, "boss@ibm.com") // the manager exists but is not who we are
	w, _ := f.signUp(t, "user@ibm.com", "l")
	jar := w.Result().Cookies()
	w, _ = f.do(t, "PUT", "/api/me", `{"config":{"mode":"observe"}}`, jar)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a plain account set the configuration through fields: %d %s", w.Code, w.Body)
	}
}

// The two fields that decide whether extract_llm can act at all have to make the round trip
// like any other. They were in stored documents and NOT on the page, which is how an account
// whose extract_llm was fully configured watched it run 251 times and act zero times: its
// traffic is prompt-cached (so the economic gate hard-declines by default) and its
// model.source was `config`, which on this service resolves to no model whatsoever.
func TestTheFieldsThatDecideWhetherExtractLLMRunsAtAllRoundTrip(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	body := `{"config":{"pipeline":["format"],"mode":"sync","extract_llm":{"per_output":true,` +
		`"cold_enabled":true,"min_tokens":2000,"max_per_request":2,"max_per_session":20,` +
		`"aggressiveness":"medium","context":"recent","context_messages":7,"cold_min_tokens":1000,` +
		`"allow_on_caching_backend":true,"model_source":"incoming","strategy":"code",` +
		`"every_n_requests":1,"trigger_min_tokens":3000}}}`
	w, out := f.do(t, "PUT", "/api/me", body, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("field save = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	eff, _ := tn["effective_config"].(map[string]any)
	x, _ := eff["extract_llm"].(map[string]any)
	if x == nil {
		t.Fatalf("no extract_llm on the form: %v", eff)
	}
	if x["allow_on_caching_backend"] != true {
		t.Error("allow_on_caching_backend did not survive the round trip")
	}
	if x["model_source"] != "incoming" {
		t.Errorf("model_source = %v, want incoming", x["model_source"])
	}
	// And it is really in the document, where the component reads it.
	doc, _ := tn["config_yaml"].(string)
	for _, want := range []string{"allow_on_caching_backend: true", "source: incoming"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the document does not carry %q:\n%s", want, doc)
		}
	}

	// The settings page needs to know whether `source: config` can resolve to anything here,
	// because on this service it cannot and the choice would otherwise read as available.
	w, out = f.do(t, "GET", "/api/options", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("options = %d", w.Code)
	}
	if out["compaction_model"] != false {
		t.Errorf("compaction_model = %v; a multi-tenant deployment withholds the operator's "+
			"compaction model, so the page must be told", out["compaction_model"])
	}
}
