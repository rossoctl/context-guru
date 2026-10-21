package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
)

// manualHost is a single-tenant proxy with a dashboard, a priced model, and one session whose
// keeper holds a pingable main thread. Time is the keeper's test clock, which the rows share.
type manualHost struct {
	h     *Handler
	rec   *dash.Recorder
	fs    *fakeSender
	clock *testClock
	sink  *reportSink
}

// reportSink is the tenant's emitter: it keeps every keep-alive report.
type reportSink struct {
	mu   sync.Mutex
	reps []components.KeepAliveReport
}

func (*reportSink) Component(components.Report) {}
func (*reportSink) Run(components.RunReport)    {}
func (s *reportSink) KeepAlivePing(r components.KeepAliveReport) {
	s.mu.Lock()
	s.reps = append(s.reps, r)
	s.mu.Unlock()
}
func (s *reportSink) all() []components.KeepAliveReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]components.KeepAliveReport(nil), s.reps...)
}

func newManualHost(t *testing.T) *manualHost {
	t.Helper()
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: rec, Prices: fixedPricer{}})
	t.Cleanup(h.Close)
	m := &manualHost{h: h, rec: rec, fs: &fakeSender{}, clock: &testClock{at: time.Now()}, sink: &reportSink{}}
	useFakeUpstream(h, m.fs, m.clock)
	return m
}

// hold makes the keeper hold `session`'s thread (two requests: the gate skips a first one) and
// writes the dashboard row the endpoint reads the cache state from.
func (m *manualHost) hold(t *testing.T, tenantID, session, thread string, ago time.Duration) {
	t.Helper()
	at := m.clock.now().Add(-ago)
	pol := kaPolicy()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	for i := 0; i < 2; i++ {
		m.h.keeper.record(&Tenancy{ID: tenantID, Cache: pol, Pipe: components.NewPipeline(nil, m.sink)}, session, thread,
			at.Add(time.Duration(i-1)*time.Second), []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	e := &dash.Event{TS: at.UnixMilli(), TenantID: tenantID, SessionID: session, ThreadID: thread,
		Model: "aws/claude-sonnet-5", Provider: "anthropic", Status: 200,
		CacheRead: 48576, CacheWrite: 1200, UpstreamMs: 1000, TokenAccounting: dash.AccountingComplete}
	m.rec.Record(e)
	waitForRows(t, m.rec, m.rowsSoFar()+1)
	rowsWritten[m.rec]++
}

var rowsWritten = map[*dash.Recorder]int64{}

func (m *manualHost) rowsSoFar() int64 { return rowsWritten[m.rec] }

// useFakeUpstream is rebuildKeeper for a test that SENDS pings. New has already built the
// handler's dispatcher around the real HTTP client, and rebuildKeeper reuses it; dropping it first
// makes the rebuilt keeper's dispatcher use fs and the handler's own price table.
func useFakeUpstream(h *Handler, fs *fakeSender, clock *testClock) {
	h.dispatch = nil
	rebuildKeeper(h, fs, clock)
}

func (m *manualHost) post(body string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest(http.MethodPost, "/api/me/cache/ping", strings.NewReader(body))
	r.Header.Set("content-type", "application/json")
	r.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	m.h.Mux().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestManualPingSendsOnePingAndAnswersWithTheCosts(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 100*time.Second)

	w, out := m.post(`{"session":"sess-a"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ping = %d %s", w.Code, w.Body)
	}
	if m.fs.n() != 1 {
		t.Fatalf("upstream saw %d requests, want 1", m.fs.n())
	}
	// The cheapest request: the ordinary ping builder's shape (max_tokens 1, not streamed), with
	// the recorded prefix byte-identical so it is a cache READ and not a write.
	sent := m.fs.calls[0].body
	if gjson.GetBytes(sent, "max_tokens").Int() != 1 || gjson.GetBytes(sent, "stream").Bool() {
		t.Errorf("manual ping is not max_tokens 1 / stream false: %s", sent)
	}
	for _, path := range []string{"model", "tools", "system", "messages", "thinking", "tool_choice"} {
		if a, b := gjson.Get(kaBody, path).Raw, gjson.GetBytes(sent, path).Raw; a != b {
			t.Errorf("%s differs from the recorded request (would miss the cache):\n%s\n%s", path, a, b)
		}
	}
	// fixedPricer: write 2.5e-6, read 2e-7. The prefix is 48576+1200 tokens.
	wantRewrite := 49776 * 2.5e-6
	if out["sent"] != true || out["cache_read"].(float64) != 48576 {
		t.Errorf("body = %v", out)
	}
	ping, rewrite, saved := out["ping_usd"].(float64), out["rewrite_usd"].(float64), out["saved_usd"].(float64)
	if d := rewrite - wantRewrite; d > 1e-9 || d < -1e-9 {
		t.Errorf("rewrite_usd = %v, want %v (prefix x cache-write price)", rewrite, wantRewrite)
	}
	if ping <= 0 || saved != rewrite-ping {
		t.Errorf("ping_usd %v saved_usd %v rewrite_usd %v: saved must be rewrite - ping", ping, saved, rewrite)
	}
	if got, want := int64(out["warm_until_ms"].(float64)), m.clock.now().Add(300*time.Second).UnixMilli(); got != want {
		t.Errorf("warm_until_ms = %d, want ping start + ttl %d", got, want)
	}
}

// The ping is reported through the tenant's emitter as a MANUAL ping carrying the write it avoided;
// the dashboard's emitter (cmd/context-guru-proxy) turns that into the keepalive row.
func TestManualPingIsReportedAsManualWithTheAvoidedWrite(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 100*time.Second)
	w, out := m.post(`{"session":"sess-a"}`)
	if w.Code != 200 {
		t.Fatalf("ping = %d %s", w.Code, w.Body)
	}
	reps := m.sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	r := reps[0]
	if !r.Manual || r.Strategy != "manual" || r.Session != "sess-a" || r.Thread != "" ||
		r.RewriteUSD != out["rewrite_usd"].(float64) || r.CostUSD != out["ping_usd"].(float64) {
		t.Errorf("report = %+v for response %v", r, out)
	}
}

func TestManualPingCold409(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 10*time.Minute) // a 5-minute cache last touched 10 minutes ago
	w, out := m.post(`{"session":"sess-a"}`)
	if w.Code != http.StatusConflict || out["error"] != "cold" {
		t.Fatalf("a cold cache = %d %v, want 409 cold", w.Code, out)
	}
	if m.fs.n() != 0 {
		t.Error("a cold-cache ping reached the provider")
	}
}

func TestManualPingUnknownSession404(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 100*time.Second)
	for _, body := range []string{`{"session":"nope"}`, `{"session":"sess-a","thread":"a:ghost"}`} {
		w, out := m.post(body)
		if w.Code != http.StatusNotFound || out["error"] != "unknown session" {
			t.Errorf("%s = %d %v, want 404 unknown session", body, w.Code, out)
		}
	}
	if w, _ := m.post(`{}`); w.Code != http.StatusBadRequest {
		t.Errorf("no session = %d, want 400", w.Code)
	}
	if m.fs.n() != 0 {
		t.Error("a refused ping reached the provider")
	}
}

func TestManualPingRateLimitedPerThread429(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 50*time.Second)
	if w, _ := m.post(`{"session":"sess-a"}`); w.Code != 200 {
		t.Fatalf("first ping = %d %s", w.Code, w.Body)
	}
	m.clock.advance(10 * time.Second)
	w, out := m.post(`{"session":"sess-a","thread":"main"}`)
	if w.Code != http.StatusTooManyRequests || out["error"] != "too soon" {
		t.Fatalf("second ping after 10 s = %d %v, want 429 too soon", w.Code, out)
	}
	if got := out["retry_after_s"].(float64); got < 19 || got > 21 {
		t.Errorf("retry_after_s = %v, want about 20", got)
	}
	if m.fs.n() != 1 {
		t.Errorf("upstream saw %d requests, want 1", m.fs.n())
	}
}

// A subagent has its own cache entry and its own rate limit.
func TestManualPingAddressesOneThread(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 50*time.Second)
	m.hold(t, "", "sess-a", "a:sub1", 50*time.Second)
	if w, _ := m.post(`{"session":"sess-a"}`); w.Code != 200 {
		t.Fatalf("main = %d %s", w.Code, w.Body)
	}
	if w, _ := m.post(`{"session":"sess-a","thread":"a:sub1"}`); w.Code != 200 {
		t.Fatalf("the subagent was limited by the main thread's ping: %d %s", w.Code, w.Body)
	}
	if m.fs.n() != 2 {
		t.Errorf("upstream saw %d requests, want 2", m.fs.n())
	}
}

// With keep-alive off the keeper holds no body or credential, so there is nothing to ping with.
func TestManualPingWithNothingHeldIsNotHeld409(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 50*time.Second)
	m.h.keeper.RetireSession("", "sess-a")
	w, out := m.post(`{"session":"sess-a"}`)
	if w.Code != http.StatusConflict || out["error"] != "not held" {
		t.Fatalf("nothing held = %d %v, want 409 not held", w.Code, out)
	}
}

// A request off loopback to a single-tenant proxy is refused, like every /api/adapt/* route.
func TestManualPingSingleTenantAnswersOnlyOnLoopback(t *testing.T) {
	m := newManualHost(t)
	m.hold(t, "", "sess-a", "", 50*time.Second)
	r := httptest.NewRequest(http.MethodPost, "/api/me/cache/ping", strings.NewReader(`{"session":"sess-a"}`))
	r.RemoteAddr = "198.51.100.7:4000"
	w := httptest.NewRecorder()
	m.h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || m.fs.n() != 0 {
		t.Errorf("remote caller = %d (sent %d), want 403 and nothing sent", w.Code, m.fs.n())
	}
}

// Hosted: a caller reaches only its own tenant's sessions. Another account's session id, with or
// without its tenant prefix, addresses nothing.
func TestManualPingReachesOnlyTheCallersOwnSessions(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetAuth(f.h.DashAuth())
	clock := &testClock{at: time.Now()}
	fs := &fakeSender{}
	useFakeUpstream(f.h, fs, clock)
	aJar, aID := f.signUpJar(t, "a@ibm.com")
	bJar, bID := f.signUpJar(t, "b@ibm.com")

	pol := kaPolicy()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-a")
	for i := 0; i < 2; i++ {
		f.h.keeper.record(&Tenancy{ID: aID, Cache: pol}, aID+":s1", "", clock.now().Add(time.Duration(i-60)*time.Second),
			[]byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	f.rec.Record(&dash.Event{TS: clock.now().Add(-60 * time.Second).UnixMilli(), TenantID: aID, SessionID: aID + ":s1",
		Model: "aws/claude-sonnet-5", Status: 200, CacheRead: 48576, CacheWrite: 1200, UpstreamMs: 1000})
	waitForRows(t, f.rec, 1)

	for _, c := range []struct {
		who  string
		jar  []*http.Cookie
		body string
		want int
	}{
		{"b by a's full id", bJar, `{"session":"` + aID + `:s1"}`, http.StatusNotFound},
		{"b by a's bare id", bJar, `{"session":"s1"}`, http.StatusNotFound},
		{"nobody", nil, `{"session":"` + aID + `:s1"}`, http.StatusUnauthorized},
		{"a by its full id", aJar, `{"session":"` + aID + `:s1"}`, http.StatusOK},
	} {
		w, _ := f.do(t, "POST", "/api/me/cache/ping", c.body, c.jar)
		if w.Code != c.want {
			t.Errorf("%s = %d %s, want %d", c.who, w.Code, w.Body, c.want)
		}
	}
	if fs.n() != 1 {
		t.Errorf("upstream saw %d pings, want only a's own", fs.n())
	}
	_ = bID
}

// The account's own proxy token is accepted (the plugin and the /cli bridge have no cookie), still
// only for its own tenant, and a request with no credential is a 401.
func TestManualPingAcceptsTheAccountsProxyToken(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetAuth(f.h.DashAuth())
	clock := &testClock{at: time.Now()}
	fs := &fakeSender{}
	useFakeUpstream(f.h, fs, clock)
	a, aTok := f.register(t, "a@ibm.com")
	b, bTok := f.register(t, "b@ibm.com")
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-a")
	for i := 0; i < 2; i++ {
		f.h.keeper.record(&Tenancy{ID: a.ID, Cache: kaPolicy()}, a.ID+":s1", "", clock.now().Add(time.Duration(i-60)*time.Second),
			[]byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	f.rec.Record(&dash.Event{TS: clock.now().Add(-60 * time.Second).UnixMilli(), TenantID: a.ID, SessionID: a.ID + ":s1",
		Model: "aws/claude-sonnet-5", Status: 200, CacheRead: 48576, CacheWrite: 1200, UpstreamMs: 1000})
	waitForRows(t, f.rec, 1)
	post := func(tok, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/me/cache/ping", strings.NewReader(body))
		if tok != "" {
			req.Header.Set("x-context-guru-token", tok)
		}
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, req)
		return w.Code
	}
	if c := post("", `{"session":"s1"}`); c != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", c)
	}
	if c := post(bTok, `{"session":"`+a.ID+`:s1"}`); c != http.StatusNotFound {
		t.Errorf("b's token on a's session = %d, want 404", c)
	}
	// ?tenant= must not widen a token.
	if c := post(bTok+"", `{"session":"s1"}`); c != http.StatusNotFound {
		t.Errorf("b's token, bare id = %d, want 404", c)
	}
	if c := post(aTok, `{"session":"s1"}`); c != http.StatusOK {
		t.Errorf("a's own token = %d, want 200", c)
	}
	_ = b
}
