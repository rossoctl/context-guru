package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tenant"
)

// Manager-level control: editing anyone's configuration, A/B variants, purge, delete,
// disable with a reason, and the password paths.
//
// The fixture here differs from ctlFixture in one way that matters: it has a DASHBOARD
// recorder wired in, because half of what a manager does (compare variants, purge storage,
// delete a user's data) reaches into the metrics database — which is a separate file from
// the control database, and the whole reason these tests exist.

// mgrFixture is a hosted proxy with the control plane and a live dashboard recorder.
type mgrFixture struct {
	*hostedFixture
	rec *dash.Recorder
}

func newMgrFixture(t *testing.T) *mgrFixture {
	t.Helper()
	t.Setenv(envRegisterMode, "open")
	var rec *dash.Recorder
	f := newHostedFixtureOpts(t, "up", "anthropic", "TEST_UPSTREAM_KEY", func(o *Options) {
		r, err := dash.NewRecorder(dash.Options{
			DBPath: ":memory:", CaptureContent: true, ContentCap: 4096,
			// One event per batch, flushed immediately: these tests assert on rows, and a
			// 250 ms wait per insert would dominate the suite.
			BatchSize: 1, FlushInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("dash.NewRecorder: %v", err)
		}
		t.Cleanup(func() { r.Close() })
		o.Dashboard = r
		rec = r
	})
	// The same wiring cmd/context-guru-proxy does, so the dashboard's routes authenticate
	// through the one resolver the control plane uses.
	f.h.API().SetAuth(f.h.DashAuth())
	f.h.API().SetWhoami(f.h.DashWhoami())
	f.h.API().SetTenantCapture(func(id string) bool {
		tn, err := f.reg.Get(id)
		return err == nil && tn.CaptureContent
	})
	return &mgrFixture{hostedFixture: f, rec: rec}
}

// signUpJar registers an account and returns its cookie jar and tenant id.
func (f *mgrFixture) signUpJar(t *testing.T, email string) ([]*http.Cookie, string) {
	t.Helper()
	w, out := f.signUp(t, email, "laptop")
	if w.Code != http.StatusCreated {
		t.Fatalf("signUp(%s) = %d %s", email, w.Code, w.Body)
	}
	jar := w.Result().Cookies()
	tn, _ := out["tenant"].(map[string]any)
	id, _ := tn["id"].(string)
	if id == "" {
		t.Fatalf("signUp(%s) returned no tenant id: %v", email, out)
	}
	return jar, id
}

// record writes one captured request for a tenant and waits for the writer to persist it.
// Written straight to the recorder rather than driven through the proxy because these tests
// need CONTENT on the row — a pass-through fixture pipeline rewrites nothing, so a real
// request would capture no transcript to be refused a look at.
func (f *mgrFixture) record(t *testing.T, tenantID, session string, e *dash.Event) int64 {
	t.Helper()
	e.TenantID, e.SessionID = tenantID, session
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if e.Mode == "" {
		e.Mode = dash.ModeActive
	}
	if e.TokenAccounting == "" {
		e.TokenAccounting = dash.AccountingComplete
	}
	f.rec.Record(e)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := f.rec.DB().Requests(dash.Filter{Tenant: tenantID, Session: session}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Requests) > 0 {
			return p.Requests[len(p.Requests)-1].ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("the writer did not persist %s/%s", tenantID, session)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every MOUNTED control-plane route is checked against the scope it declared, by walking
// the table MountControl itself uses.
//
// This is the test a review found MISSING: the dashboard's routes had one, the control
// plane's did not, so a manager route added without its role check would have shipped
// unnoticed. Iterating the real table means a new route either declares a scope and is
// checked here, or fails here.
func TestEveryControlRouteEnforcesItsScope(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")

	refused := func(code int) bool {
		return code == http.StatusUnauthorized || code == http.StatusForbidden
	}
	for _, rt := range f.h.ctlRoutes() {
		method, path := splitPattern(rt.pattern)
		// The caller's own ids, so a route that reaches its handler answers rather than 404s.
		path = strings.ReplaceAll(path, "{id}", userID)
		path = strings.ReplaceAll(path, "{prefix}", "abcdefgh")

		switch rt.scope {
		case ctlPublic:
			// No session needed. The body is empty, so a 400 is the expected answer for most
			// of these — what must NOT happen is a refusal for want of a cookie.
			if w, _ := f.do(t, method, path, "", nil); refused(w.Code) {
				if w, _ = f.do(t, method, path, "", nil); refused(w.Code) {
					t.Errorf("%s is public but refused an anonymous caller: %d %s",
						rt.pattern, w.Code, w.Body)
				}
			}
		case ctlTenant:
			if w, _ := f.do(t, method, path, "", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s without a cookie = %d, want 401", rt.pattern, w.Code)
			}
			if w, _ := f.do(t, method, path, "", userJar); refused(w.Code) {
				t.Errorf("%s refused its own tenant: %d %s", rt.pattern, w.Code, w.Body)
			}
		case ctlManager:
			if w, _ := f.do(t, method, path, "", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s without a cookie = %d, want 401", rt.pattern, w.Code)
			}
			// A plain user, including one who tries to widen their scope in the query string —
			// the shape of attack the dashboard's filter overwrite exists to stop, aimed at
			// the control plane instead.
			for _, q := range []string{"", "?tenant=" + userID, "?tenant=*", "?manager=1"} {
				w, _ := f.do(t, method, path+q, "", userJar)
				if w.Code != http.StatusForbidden {
					t.Errorf("%s%s for a plain tenant = %d, want 403: %s",
						rt.pattern, q, w.Code, w.Body)
				}
			}
			if w, _ := f.do(t, method, path, "", mgrJar); refused(w.Code) {
				t.Errorf("%s refused a manager: %d %s", rt.pattern, w.Code, w.Body)
			}
		default:
			t.Errorf("%s declared an unknown scope %d", rt.pattern, rt.scope)
		}
	}
}

func splitPattern(p string) (method, path string) {
	i := strings.IndexByte(p, ' ')
	return p[:i], p[i+1:]
}

// A manager edits somebody else's WHOLE configuration — pipeline, mode, variant, quota —
// and a document that does not build is a 400 that changes nothing.
func TestManagerEditsAnyTenantsFullConfiguration(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, userID := f.signUpJar(t, "a@ibm.com")

	body, _ := json.Marshal(map[string]any{
		"config_yaml": "pipeline: [format]\nmode: observe\n",
		"variant":     "arm-b",
		"max_rows":    4321,
	})
	w, out := f.do(t, "PATCH", "/api/tenants/"+userID, string(body), mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager edit = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	if tn["variant"] != "arm-b" || tn["config_inherited"] != false ||
		!strings.Contains(tn["effective_config_yaml"].(string), "mode: observe") {
		t.Fatalf("the edit did not apply: %v", tn)
	}

	// An invalid document is refused by the same strict loader the proxy builds with, and
	// the message names the offending key. Nothing may be partially applied.
	w, out = f.do(t, "PATCH", "/api/tenants/"+userID,
		`{"config_yaml":"pipeline: [format]\nnot_a_real_key: 1\n"}`, mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid config = %d, want 400: %s", w.Code, w.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "not_a_real_key") {
		t.Errorf("the error does not name the offending key: %q", msg)
	}
	after, err := f.reg.Get(userID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.ConfigYAML, "mode: observe") || strings.Contains(after.ConfigYAML, "not_a_real_key") {
		t.Errorf("a rejected config partially applied: %q", after.ConfigYAML)
	}
	// An unknown variant character is refused too — the name is a grouping key, not free text.
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+userID, `{"variant":"arm b/../x"}`, mgrJar); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed variant = %d, want 400", w.Code)
	}
}

// NEW RULE, replacing "a manager reads nobody's transcripts": a manager reads any account's
// request diffs and session transcripts, and nobody else reads anyone's but their own.
// Checked through the REAL control-plane authenticator rather than a stub.
func TestManagerReadsAnotherTenantsTranscripts(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, userID := f.signUpJar(t, "a@ibm.com")
	otherJar, _ := f.signUpJar(t, "b@ibm.com")
	const secret = "SECRET-SOURCE-CODE-OF-A"
	id := f.record(t, userID, "sess-a", &dash.Event{
		Model: "m", TokensBefore: 100, TokensAfter: 60,
		Content: []dash.ContentRow{{Path: "0", Before: secret, After: "x"}},
	})
	reqPath := "/api/requests/" + strconv.FormatInt(id, 10)

	for _, path := range []string{
		reqPath,
		"/api/sessions/sess-a/transcript?tenant=" + userID,
		"/api/sessions/sess-a/transcript?tenant=*",
	} {
		w, _ := f.do(t, "GET", path, "", mgrJar)
		if w.Code != http.StatusOK {
			t.Errorf("%s for a manager = %d %s", path, w.Code, w.Body)
			continue
		}
		if !strings.Contains(w.Body.String(), secret) {
			t.Errorf("a manager could not read another tenant's transcript via %s", path)
		}
	}
	// The transcript route reports the content as present, so the manager's drawer renders
	// the diff instead of an explanation of why it is empty.
	w, out := f.do(t, "GET", "/api/sessions/sess-a/transcript?tenant="+userID, "", mgrJar)
	if w.Code != http.StatusOK || out["state"] != dash.TranscriptHot {
		t.Errorf("manager transcript state = %v (%d), want %q", out["state"], w.Code, dash.TranscriptHot)
	}
	// Only the manager branch widened. Another plain account gets nothing for the same
	// request id or session, and is refused rather than shown an empty diff.
	for _, path := range []string{
		reqPath,
		"/api/sessions/sess-a/transcript?tenant=" + userID,
		"/api/sessions/sess-a/transcript?tenant=*",
	} {
		w, _ := f.do(t, "GET", path, "", otherJar)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s for another plain account = %d, want 404: %s", path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("a plain account read someone else's transcript via %s", path)
		}
	}
	// The list and rollup surfaces stay metrics-only for everyone: they carry no content
	// column at all, and a manager who wants the text opens one request.
	for _, path := range []string{
		"/api/requests?tenant=" + userID, "/api/requests?tenant=*",
		"/api/tenants", "/api/variants",
	} {
		w, _ := f.do(t, "GET", path, "", mgrJar)
		if w.Code != http.StatusOK {
			t.Errorf("%s for a manager = %d %s", path, w.Code, w.Body)
			continue
		}
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("%s grew a content field:\n%s", path, w.Body)
		}
	}
}

// Deleting a tenant has to clear BOTH databases. The control database cascades; the metrics
// database has no foreign key reaching into it, so it is the half that used to be left
// behind — including the child rows, which no tenant-scoped query would ever show.
func TestDeleteTenantRemovesRowsFromBothDatabases(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, mgrID := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")
	_, tok := f.register(t, "agent-a@ibm.com")

	f.record(t, userID, "sess-a", &dash.Event{
		Model: "m", TokensBefore: 100, TokensAfter: 60, CostUSD: 0.5, BaselineCostUSD: 1,
		Components: []dash.CompRow{{Component: "format", Acted: true, SavedUnique: 40}},
		Content:    []dash.ContentRow{{Path: "0", Before: "before", After: "after"}},
	})
	// A second tenant's row, to prove the delete is scoped.
	f.record(t, mgrID, "sess-boss", &dash.Event{Model: "m",
		Components: []dash.CompRow{{Component: "format"}},
		Content:    []dash.ContentRow{{Path: "0", Before: "boss", After: "b"}}})

	// No confirmation, no deletion.
	if w, _ := f.do(t, "DELETE", "/api/tenants/"+userID, `{}`, mgrJar); w.Code != http.StatusBadRequest {
		t.Fatalf("delete without confirmation = %d, want 400", w.Code)
	}
	if w, _ := f.do(t, "DELETE", "/api/tenants/"+userID, `{"confirm":"someone@else.com"}`, mgrJar); w.Code != http.StatusBadRequest {
		t.Fatalf("delete with the wrong confirmation = %d, want 400", w.Code)
	}
	if _, err := f.reg.Get(userID); err != nil {
		t.Fatalf("an unconfirmed delete removed the account: %v", err)
	}
	// A manager cannot delete themselves: the manager routes are the only way to appoint
	// another one.
	if w, _ := f.do(t, "DELETE", "/api/tenants/"+mgrID, `{"confirm":"boss@ibm.com"}`, mgrJar); w.Code != http.StatusForbidden {
		t.Fatalf("manager self-delete = %d, want 403", w.Code)
	}

	w, out := f.do(t, "DELETE", "/api/tenants/"+userID, `{"confirm":"a@ibm.com"}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	purged, _ := out["purged"].(map[string]any)
	if n, _ := purged["requests"].(float64); n < 1 {
		t.Errorf("the reply does not report what it deleted: %v", out)
	}

	// Control database: the account, its tokens and its session.
	if _, err := f.reg.Get(userID); err == nil {
		t.Error("the account row survived the delete")
	}
	if w, _ := f.do(t, "GET", "/api/me", "", userJar); w.Code == http.StatusOK {
		t.Error("the deleted tenant's dashboard session still works")
	}
	if toks, err := f.reg.Tokens(userID); err != nil || len(toks) != 0 {
		t.Errorf("token rows survived: %v %v", toks, err)
	}
	// Metrics database: their rows, and no orphans anywhere.
	if has, err := f.rec.DB().TenantHasRows(userID); err != nil || has {
		t.Errorf("metrics rows survived the delete (has=%v err=%v)", has, err)
	}
	comps, content, err := f.rec.DB().OrphanRows()
	if err != nil {
		t.Fatal(err)
	}
	if comps != 0 || content != 0 {
		t.Errorf("delete left orphans: %d component rows, %d content rows", comps, content)
	}
	// The other tenant is untouched.
	if has, err := f.rec.DB().TenantHasRows(mgrID); err != nil || !has {
		t.Errorf("the delete took another tenant's rows too (has=%v err=%v)", has, err)
	}
	// A pre-existing token for a DIFFERENT account still proxies, so the delete did not
	// break authentication generally.
	if resp := f.post("/anthropic/v1/messages", tok, ""); resp.Code != http.StatusOK {
		t.Errorf("an unrelated token stopped working after a delete: %d %s", resp.Code, resp.Body)
	}
}

// Purge clears the data and leaves the account working. That is the whole difference from
// delete, and the thing worth a test: a purge that quietly broke the account would look
// identical in the roster.
func TestPurgeLeavesTheAccountWorking(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")
	tok := f.tokenFor(t, userJar)
	f.record(t, userID, "sess-a", &dash.Event{Model: "m", TokensBefore: 10, TokensAfter: 5,
		Components: []dash.CompRow{{Component: "format", Acted: true}},
		Content:    []dash.ContentRow{{Path: "0", Before: "x", After: "y"}}})

	if w, _ := f.do(t, "POST", "/api/tenants/"+userID+"/purge", `{}`, mgrJar); w.Code != http.StatusBadRequest {
		t.Fatalf("purge without confirmation = %d, want 400", w.Code)
	}
	w, _ := f.do(t, "POST", "/api/tenants/"+userID+"/purge", `{"confirm":"`+userID+`"}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", w.Code, w.Body)
	}
	if has, err := f.rec.DB().TenantHasRows(userID); err != nil || has {
		t.Errorf("purge left rows behind (has=%v err=%v)", has, err)
	}
	comps, content, err := f.rec.DB().OrphanRows()
	if err != nil {
		t.Fatal(err)
	}
	if comps != 0 || content != 0 {
		t.Errorf("purge left orphans: %d component rows, %d content rows", comps, content)
	}
	// Still an account, still signed in, still able to send traffic.
	if _, err := f.reg.Get(userID); err != nil {
		t.Errorf("purge deleted the account: %v", err)
	}
	if w, _ := f.do(t, "GET", "/api/me", "", userJar); w.Code != http.StatusOK {
		t.Errorf("purge signed the tenant out: %d", w.Code)
	}
	if resp := f.post("/anthropic/v1/messages", tok, ""); resp.Code != http.StatusOK {
		t.Errorf("purge broke the tenant's token: %d %s", resp.Code, resp.Body)
	}
}

// tokenFor mints a fresh proxy token for the account behind a cookie jar.
func (f *mgrFixture) tokenFor(t *testing.T, jar []*http.Cookie) string {
	t.Helper()
	w, out := f.do(t, "POST", "/api/me/tokens", `{"label":"agent"}`, jar)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	tok, _ := out["token"].(string)
	return tok
}

// A disabled account must fail CLEANLY and say why, on every door: the agent's request, and
// the dashboard sign-in. Without the reason, "disabled" is indistinguishable from a bug in
// the proxy to the person whose work just stopped.
func TestDisabledTenantIsRefusedWithTheReason(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")
	tok := f.tokenFor(t, userJar)
	const reason = "paused pending the finance review"

	w, out := f.do(t, "PATCH", "/api/tenants/"+userID,
		`{"disabled":true,"disabled_reason":"`+reason+`"}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	tn, _ := out["tenant"].(map[string]any)
	if tn["disabled"] != true || tn["disabled_reason"] != reason {
		t.Fatalf("disable did not stick: %v", tn)
	}
	resp := f.post("/anthropic/v1/messages", tok, "")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("a disabled tenant's agent got %d, want 403: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body.String(), reason) {
		t.Errorf("the 403 does not carry the reason, so it is undiagnosable: %s", resp.Body)
	}
	// Signing in says the same thing.
	w, _ = f.do(t, "POST", "/api/login", `{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reason) {
		t.Errorf("sign-in refusal = %d %s, want 403 naming the reason", w.Code, w.Body)
	}
	// Re-enabling clears the note: a stale reason on a live account is acted on by the next
	// manager who reads the roster.
	w, out = f.do(t, "PATCH", "/api/tenants/"+userID, `{"disabled":false}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable = %d %s", w.Code, w.Body)
	}
	tn, _ = out["tenant"].(map[string]any)
	if tn["disabled"] != false || tn["disabled_reason"] != "" {
		t.Errorf("re-enabling left a stale reason: %v", tn)
	}
	if resp := f.post("/anthropic/v1/messages", tok, ""); resp.Code != http.StatusOK {
		t.Errorf("a re-enabled tenant is still refused: %d %s", resp.Code, resp.Body)
	}
}

// Changing your own password requires the old one — a stolen cookie must not be
// convertible into permanent ownership of the account.
//
// Split across two tests on purpose: every password check here spends one of
// passwordAttemptsPerMinute (5, per email AND per address), and a single test that made six
// of them would fail on the limiter rather than on the behaviour it is asserting. Keep each
// of these under five.
func TestPasswordChangeRequiresTheOldPassword(t *testing.T) {
	f := newMgrFixture(t)
	jar, _ := f.signUpJar(t, "a@ibm.com")

	w, _ := f.do(t, "POST", "/api/me/password",
		`{"old_password":"not-the-password","new_password":"a-longer-new-password"}`, jar)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong old password = %d, want 401: %s", w.Code, w.Body)
	}
	// A short new password is refused too.
	if w, _ := f.do(t, "POST", "/api/me/password",
		`{"old_password":"`+testPassword+`","new_password":"short"}`, jar); w.Code != http.StatusBadRequest {
		t.Errorf("short new password = %d, want 400", w.Code)
	}
	// And the original password still works, so neither refusal half-applied.
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("the original password stopped working after a refused change: %d %s", w.Code, w.Body)
	}
}

// The successful change: the new password works, the old one does not, the browser that
// made the change stays signed in, and the event is on the record.
func TestPasswordChangeReplacesTheOldPassword(t *testing.T) {
	f := newMgrFixture(t)
	jar, _ := f.signUpJar(t, "a@ibm.com")
	const next = "a-longer-new-password"

	w, _ := f.do(t, "POST", "/api/me/password",
		`{"old_password":"`+testPassword+`","new_password":"`+next+`"}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("change = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "GET", "/api/me", "", jar); w.Code != http.StatusOK {
		t.Errorf("changing my password signed me out of the browser I did it in: %d", w.Code)
	}
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the old password still signs in: %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+next+`"}`, nil); w.Code != http.StatusOK {
		t.Errorf("the new password does not sign in: %d %s", w.Code, w.Body)
	}
	_, out := f.do(t, "GET", "/api/me/audit", "", jar)
	if !strings.Contains(mustJSON(t, out), `"password"`) {
		t.Errorf("the password change is not in the audit trail: %v", out)
	}
}

// A manager may START a reset and nothing more: they cannot learn the password, cannot set
// one, and the account keeps working until its owner finishes. A manager who could set a
// password could sign in AS that user and act in their name, which is the boundary this
// service still promises — a manager READING transcripts is now allowed, impersonating an
// account is not.
func TestManagerResetNeitherSetsNorRevealsAPassword(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, userID := f.signUpJar(t, "a@ibm.com")

	w, out := f.do(t, "POST", "/api/tenants/"+userID+"/password-reset", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("manager reset = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if regexp.MustCompile(`\b\d{6}\b`).MatchString(body) {
		t.Errorf("the manager's reply contains a code: %s", body)
	}
	if strings.Contains(body, testPassword) {
		t.Errorf("the manager's reply contains a password: %s", body)
	}
	if out["note"] == nil {
		t.Error("the reply does not say that the manager cannot finish this")
	}
	// The account is untouched: the old password still signs in, because a reset a manager
	// started must not be a way to lock someone out.
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+testPassword+`"}`, nil); w.Code != http.StatusOK {
		t.Errorf("a manager-initiated reset changed the password: %d %s", w.Code, w.Body)
	}
	// Only the OWNER, holding the mailed code, can finish it.
	const next = "recovered-password-1"
	w, _ = f.do(t, "POST", "/api/password-reset/verify",
		`{"email":"a@ibm.com","code":"000000","new_password":"`+next+`"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong reset code = %d, want 401: %s", w.Code, w.Body)
	}
	code := f.resetCode(t)
	w, _ = f.do(t, "POST", "/api/password-reset/verify",
		`{"email":"a@ibm.com","code":"`+code+`","new_password":"`+next+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("completing the reset = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+next+`"}`, nil); w.Code != http.StatusOK {
		t.Errorf("the reset password does not sign in: %d %s", w.Code, w.Body)
	}
	// A reset code is single use, and it is not a login second factor: spending it again,
	// or spending it at /api/verify, fails.
	if w, _ := f.do(t, "POST", "/api/password-reset/verify",
		`{"email":"a@ibm.com","code":"`+code+`","new_password":"`+next+`x"}`, nil); w.Code == http.StatusOK {
		t.Error("a reset code was reusable")
	}
	if w, _ := f.do(t, "POST", "/api/verify", `{"email":"a@ibm.com","code":"`+code+`"}`, nil); w.Code == http.StatusOK {
		t.Error("a reset code was accepted as a login second factor")
	}
}

// Self-service recovery: the flow for someone who has forgotten their password and has no
// manager to ask. It must not become a directory of who has an account here.
func TestSelfServiceResetIsNotAnAccountOracle(t *testing.T) {
	f := newMgrFixture(t)
	f.signUpJar(t, "a@ibm.com")

	w1, out1 := f.do(t, "POST", "/api/password-reset", `{"email":"a@ibm.com"}`, nil)
	w2, out2 := f.do(t, "POST", "/api/password-reset", `{"email":"nobody@ibm.com"}`, nil)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("statuses differ or are not 200: %d / %d", w1.Code, w2.Code)
	}
	// Same fields, and an expiry that is present in both — a zero for the unknown address
	// would be exactly the tell the status no longer is.
	for _, k := range []string{"next", "email", "code_expires_at", "code_valid_secs"} {
		if _, ok := out1[k]; !ok {
			t.Errorf("the reply for a real address is missing %q", k)
		}
		if _, ok := out2[k]; !ok {
			t.Errorf("the reply for an unknown address is missing %q", k)
		}
	}
	if exp, _ := out2["code_expires_at"].(float64); exp <= 0 {
		t.Errorf("code_expires_at for an unknown address = %v, which distinguishes it", exp)
	}
	// The owner really can recover.
	const next = "self-recovered-pass"
	code := f.resetCode(t)
	if w, _ := f.do(t, "POST", "/api/password-reset/verify",
		`{"email":"a@ibm.com","code":"`+code+`","new_password":"`+next+`"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("self-service reset = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "POST", "/api/login",
		`{"email":"a@ibm.com","password":"`+next+`"}`, nil); w.Code != http.StatusOK {
		t.Errorf("the recovered password does not sign in: %d %s", w.Code, w.Body)
	}
}

// resetCode reads the newest code out of the mail sink. Reading the SINK rather than the
// registry is the same choice signUp makes: it means these tests fail if the mail path
// stops producing a usable code.
func (f *mgrFixture) resetCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv(envMailDevSink))
	if err != nil {
		t.Fatalf("mail sink: %v", err)
	}
	// The reset mail is the one that says so, so a login or registration code left in the
	// sink by an earlier step cannot be picked up by mistake.
	parts := strings.Split(string(b), "--- ")
	for i := len(parts) - 1; i >= 0; i-- {
		if !strings.Contains(parts[i], "reset") {
			continue
		}
		if m := regexp.MustCompile(`\b\d{6}\b`).FindString(parts[i]); m != "" {
			return m
		}
	}
	t.Fatalf("no reset code in the mail sink:\n%s", b)
	return ""
}

// The A/B rollup: per-variant totals folded from the per-tenant aggregates, and the
// caveats that keep it from being quoted as a causal result.
func TestVariantsFoldPerTenantMetricsAndStateTheirLimits(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, aID := f.signUpJar(t, "a@ibm.com")
	// A third account would exceed the per-IP registration limit, so the second arm is a
	// token-only account — which is also the older account shape, and worth covering.
	bT, _ := f.register(t, "b@ibm.com")
	bID := bT.ID

	for _, id := range []string{aID, bID} {
		variant := "arm-a"
		if id == bID {
			variant = "arm-b"
		}
		w, _ := f.do(t, "PATCH", "/api/tenants/"+id, `{"variant":"`+variant+`"}`, mgrJar)
		if w.Code != http.StatusOK {
			t.Fatalf("assign %s = %d %s", variant, w.Code, w.Body)
		}
	}
	f.record(t, aID, "sess-a", &dash.Event{Model: "m", TokensBefore: 1000, TokensAfter: 600,
		SavedUnique: 400, CostUSD: 1, BaselineCostUSD: 2, CacheRead: 50,
		Components: []dash.CompRow{{Component: "format", Acted: true, SavedUnique: 400}},
		Content:    []dash.ContentRow{{Path: "0", Before: "SECRET-A", After: "x"}}})
	f.record(t, bID, "sess-b", &dash.Event{Model: "m", TokensBefore: 1000, TokensAfter: 900,
		SavedUnique: 100, CostUSD: 1.5, BaselineCostUSD: 2,
		Components: []dash.CompRow{{Component: "format", Acted: true, SavedUnique: 100},
			{Component: "dedup", Reverted: true}}})

	w, out := f.do(t, "GET", "/api/variants", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("variants = %d %s", w.Code, w.Body)
	}
	if caveats, _ := out["caveats"].([]any); len(caveats) < 4 {
		t.Errorf("the rollup reports %d caveats; a cost delta with no confounds named is worse "+
			"than none", len(caveats))
	}
	rows, _ := out["variants"].([]any)
	byName := map[string]map[string]any{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		byName[row["variant"].(string)] = row
	}
	a, b := byName["arm-a"], byName["arm-b"]
	if a == nil || b == nil {
		t.Fatalf("both arms should be present: %v", mustJSON(t, out))
	}
	if a["requests"] != 1.0 || b["requests"] != 1.0 {
		t.Errorf("request counts did not fold: a=%v b=%v", a["requests"], b["requests"])
	}
	if a["saved_unique"] != 400.0 || b["saved_unique"] != 100.0 {
		t.Errorf("savings did not fold: a=%v b=%v", a["saved_unique"], b["saved_unique"])
	}
	// The denominators the panel must never omit.
	for _, want := range []string{"tenants", "reporting", "incomplete_rows", "sessions", "configs"} {
		if _, ok := a[want]; !ok {
			t.Errorf("a variant row has no %q, so its numbers cannot be judged", want)
		}
	}
	// Per-component outcomes, which is what says WHICH change did it.
	comps, _ := b["components"].([]any)
	if len(comps) == 0 {
		t.Fatalf("no per-component rows for arm-b: %v", b)
	}
	found := false
	for _, c := range comps {
		row, _ := c.(map[string]any)
		if row["component"] == "dedup" && row["reverted"] == 1.0 {
			found = true
		}
	}
	if !found {
		t.Errorf("reverted components did not fold into the variant: %v", comps)
	}
	// The unassigned group is reported too: traffic outside the experiment is part of
	// reading it. The manager's own account is in it.
	if byName[""] == nil {
		t.Error("the unassigned group is missing, so the panel cannot show what is outside the test")
	}
	// And still no transcripts.
	if strings.Contains(w.Body.String(), "SECRET-A") {
		t.Error("the A/B rollup carried a tenant's transcript content")
	}
}

// A user cannot assign themselves a variant or write their own disabled reason: both are
// manager fields, and PUT /api/me has no such field at all — the strict decoder refuses
// rather than silently ignoring it.
func TestUserCannotSetManagerOnlyFields(t *testing.T) {
	f := newMgrFixture(t)
	jar, id := f.signUpJar(t, "a@ibm.com")
	for _, body := range []string{`{"variant":"arm-a"}`, `{"disabled_reason":"nope"}`} {
		if w, _ := f.do(t, "PUT", "/api/me", body, jar); w.Code != http.StatusBadRequest {
			t.Errorf("PUT /api/me %s = %d, want 400", body, w.Code)
		}
		if w, _ := f.do(t, "PATCH", "/api/tenants/"+id, body, jar); w.Code != http.StatusForbidden {
			t.Errorf("PATCH own tenant %s = %d, want 403", body, w.Code)
		}
	}
}

// The compaction configuration is the manager's, per user. A user's own PUT /api/me is
// refused for config_yaml and for nothing else: the fields they legitimately own — their
// machine label, their upstreams, their capture consent — still save in the same request
// shape the settings page sends.
func TestUserCannotSetTheirOwnCompaction(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")

	// A manager parks a configuration on them first, so the refusal below is provably a
	// refusal to CHANGE something rather than a refusal to write to an empty field.
	const managed = "pipeline: [format]\nmode: observe\n"
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+userID,
		mustJSON(t, map[string]any{"config_yaml": managed}), mgrJar); w.Code != http.StatusOK {
		t.Fatalf("manager could not set the config: %d %s", w.Code, w.Body)
	}
	// And it is audited, with the manager as the actor — this is the path that replaces
	// the user's own editing, so "who changed my pipeline" has to have an answer.
	entries, err := f.reg.Audit(userID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, e := range entries {
		if e.Field == "config_yaml" && e.Actor != userID && e.After == managed {
			audited = true
		}
	}
	if !audited {
		t.Errorf("the manager's config edit was not audited: %+v", entries)
	}

	w, out := f.do(t, "PUT", "/api/me", `{"config_yaml":"pipeline: [dedup]\n"}`, userJar)
	if w.Code != http.StatusForbidden {
		t.Fatalf("user PUT /api/me config_yaml = %d, want 403: %s", w.Code, w.Body)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "manager") {
		t.Errorf("the refusal does not say who sets it: %q", msg)
	}
	after, err := f.reg.Get(userID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ConfigYAML != managed {
		t.Errorf("the refused write changed the stored config: %q", after.ConfigYAML)
	}

	// What is theirs still saves.
	if w, _ := f.do(t, "PUT", "/api/me",
		`{"label":"desktop","capture_content":true,"up_anthropic":"up"}`, userJar); w.Code != http.StatusOK {
		t.Fatalf("user PUT /api/me own fields = %d, want 200: %s", w.Code, w.Body)
	}
	if after, _ = f.reg.Get(userID); !after.CaptureContent || after.Label != "desktop" {
		t.Errorf("the user's own fields did not save: %+v", after)
	}

	// A manager's own settings page is unchanged.
	if w, _ := f.do(t, "PUT", "/api/me", `{"config_yaml":"mode: observe\n"}`, mgrJar); w.Code != http.StatusOK {
		t.Fatalf("manager PUT /api/me config_yaml = %d, want 200: %s", w.Code, w.Body)
	}
}

// Promotion through the dashboard, end to end: the role reaches the registry, is audited,
// and the promoted account's very next request carries manager scope.
func TestPromotingAUserGrantsManagerScope(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, mgrID := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "a@ibm.com")

	if w, _ := f.do(t, "GET", "/api/tenants", "", userJar); w.Code != http.StatusForbidden {
		t.Fatalf("a plain user reached the roster: %d", w.Code)
	}
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+userID, `{"role":"manager"}`, mgrJar); w.Code != http.StatusOK {
		t.Fatalf("promotion = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "GET", "/api/tenants", "", userJar); w.Code != http.StatusOK {
		t.Fatalf("the promoted account still has no manager scope: %d %s", w.Code, w.Body)
	}
	entries, err := f.reg.Audit(userID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Field == "role" && e.Actor == mgrID && e.Before == "user" && e.After == "manager" {
			found = true
		}
	}
	if !found {
		t.Errorf("the promotion was not audited: %+v", entries)
	}
	// And back down, which is only allowed because the promoter is still a manager.
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+userID, `{"role":"user"}`, mgrJar); w.Code != http.StatusOK {
		t.Fatalf("demotion of a second manager = %d %s", w.Code, w.Body)
	}
}

// The lockout guard: the LAST manager may not be demoted or disabled, because only a
// manager can hand the role out again and the dashboard is the only place it happens.
func TestLastManagerCannotBeDemotedOrDisabled(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, mgrID := f.signUpJar(t, "boss@ibm.com")
	f.signUpJar(t, "a@ibm.com")

	for _, body := range []string{`{"role":"user"}`, `{"disabled":true,"disabled_reason":"oops"}`} {
		w, out := f.do(t, "PATCH", "/api/tenants/"+mgrID, body, mgrJar)
		if w.Code == http.StatusOK {
			t.Fatalf("PATCH self %s was allowed; the deployment has no manager left", body)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "last manager") {
			t.Errorf("the refusal does not explain itself: %q", msg)
		}
	}
	if still, err := f.reg.Get(mgrID); err != nil || !still.IsManager() || still.Disabled {
		t.Fatalf("the last manager was changed anyway: %+v %v", still, err)
	}

	// With a second manager in place, both are allowed again.
	_, otherID := f.signUpJar(t, "b@ibm.com")
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+otherID, `{"role":"manager"}`, mgrJar); w.Code != http.StatusOK {
		t.Fatalf("promotion = %d %s", w.Code, w.Body)
	}
	if w, _ := f.do(t, "PATCH", "/api/tenants/"+mgrID, `{"role":"user"}`, mgrJar); w.Code != http.StatusOK {
		t.Fatalf("demotion with a spare manager = %d %s", w.Code, w.Body)
	}
}

// The Grafana gate names the owner of the SESSION, never whoever the request claims to be.
//
// Grafana signs in whoever X-Cg-Grafana-User names, so that header is a complete
// authentication and this endpoint is where its value is decided. Two properties, and the
// second is the one an attacker goes for: a refusal carries no identity at all, and a
// forged header on the request never becomes the identity on the response — nginx copies
// onto the proxied request only what comes back from here (see nginx.conf), so a client
// value that survived this would be an admin bypass.
func TestGrafanaAuthzNamesTheSessionOwnerNotTheRequestsHeader(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com") // matches the fixture's ManagerEmail
	userJar, _ := f.signUpJar(t, "a@ibm.com")

	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
		forge   bool
		code    int
		want    string // the identity the answer may carry; "" for none at all
	}{
		{"anonymous", nil, false, http.StatusUnauthorized, ""},
		{"anonymous, forged header", nil, true, http.StatusUnauthorized, ""},
		{"plain user", userJar, false, http.StatusForbidden, ""},
		{"plain user, forged header", userJar, true, http.StatusForbidden, ""},
		{"manager", mgrJar, false, http.StatusNoContent, "boss@ibm.com"},
		{"manager, forged header", mgrJar, true, http.StatusNoContent, "boss@ibm.com"},
	} {
		r := httptest.NewRequest("GET", "/api/authz/grafana", nil)
		for _, c := range tc.cookies {
			r.AddCookie(c)
		}
		if tc.forge {
			r.Header.Set(grafanaUserHeader, "attacker@ibm.com")
		}
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)

		if w.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.code)
		}
		if got := w.Header().Get(grafanaUserHeader); got != tc.want {
			t.Errorf("%s: %s = %q, want %q", tc.name, grafanaUserHeader, got, tc.want)
		}
		// An authorization carries nothing but the identity: a body here would describe
		// what is behind the gate. (A REFUSAL keeps the control plane's own error body,
		// which nginx discards — it reads the status only.)
		if tc.code == http.StatusNoContent && w.Body.Len() != 0 {
			t.Errorf("%s: the gate answered with a body: %s", tc.name, w.Body)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var _ = tenant.PurposeReset
