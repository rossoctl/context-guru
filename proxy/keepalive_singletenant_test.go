package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"log/slog"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	acmetrics "github.com/rossoctl/accountant/metrics"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
)

// #1457: "no audit sink, no retention" is a hosted rule. A single-tenant proxy holds only its own
// user's credential, in memory, and needs no dashboard to ping.

const fakeSingleTenantKey = "sk-test-SECRET"

// bookSink is the tenant's emitter as proxyapp wires it with no dashboard: every ping goes to BookPing.
type bookSink struct {
	*reportSink
	st  *PingStore
	obs *countObs
}

type countObs struct{ n, ops int }

func (c *countObs) Observe(o acmetrics.Observation) { c.n++; c.ops += len(o.Result.Ops) }

func (b *bookSink) KeepAlivePing(r components.KeepAliveReport) {
	b.reportSink.KeepAlivePing(r)
	BookPing(r, b.st, b.obs)
}

// noDashHost is a single-tenant proxy with no dashboard and a held thread.
type noDashHost struct {
	h     *Handler
	fs    *fakeSender
	clock *testClock
	sink  *bookSink
}

func newNoDashHost(t *testing.T) *noDashHost {
	t.Helper()
	h := New(nil, nil, metrics.NewAggregator(), Options{Prices: fixedPricer{}})
	t.Cleanup(h.Close)
	if h.rec != nil {
		t.Fatal("the test host has a recorder")
	}
	n := &noDashHost{h: h, fs: &fakeSender{}, clock: &testClock{at: time.Now()},
		sink: &bookSink{reportSink: &reportSink{}, st: NewPingStore(), obs: &countObs{}}}
	useFakeUpstream(h, n.fs, n.clock)
	return n
}

func (n *noDashHost) hold(session string, ago time.Duration) {
	at := n.clock.now().Add(-ago)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+fakeSingleTenantKey)
	for i := 0; i < 2; i++ {
		n.h.keeper.record(&Tenancy{ID: "", Cache: kaPolicy(), Pipe: components.NewPipeline(nil, n.sink)}, session, "",
			at.Add(time.Duration(i-1)*time.Second), []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
}

func (n *noDashHost) post(body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/me/cache/ping", strings.NewReader(body))
	r.Header.Set("content-type", "application/json")
	r.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	n.h.Mux().ServeHTTP(w, r)
	return w
}

// Single-tenant, no dashboard: the thread is held, a scheduled ping fires, its spend is booked, and the
// Ping button works. The credential reaches no log and no file.
func TestSingleTenantWithNoDashboardHoldsAndPings(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("HOME", dir)
	var logs syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	n := newNoDashHost(t)
	n.hold("sess-a", 0)
	if got := n.h.keeper.Stats().Live; got != 1 {
		t.Fatalf("held %d threads with no dashboard in single-tenant mode, want 1", got)
	}
	if !n.h.keeper.canPing(&Tenancy{ID: "", Cache: kaPolicy()}, "sess-a", "", bschemas.Anthropic) {
		t.Error("canPing is false for a single-tenant proxy with no dashboard")
	}
	n.h.keeper.Sweep(n.clock.advance(281 * time.Second))
	waitPings(t, n.h.keeper, 1)
	if n.fs.n() != 1 {
		t.Fatalf("upstream saw %d pings, want 1", n.fs.n())
	}
	if got := n.fs.calls[0].hdr.Get("Authorization"); got != "Bearer "+fakeSingleTenantKey {
		t.Errorf("the ping did not carry the caller's own credential: %q", got)
	}
	if n.sink.obs.n != 1 || n.sink.obs.ops != 1 {
		t.Errorf("savings booked %d observations with %d ops, want 1 and 1", n.sink.obs.n, n.sink.obs.ops)
	}

	// The Ping button, on a thread held a moment ago.
	n.hold("sess-b", 100*time.Second)
	w := n.post(`{"session":"sess-b"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("manual ping with no dashboard = %d %s, want 200", w.Code, w.Body)
	}
	// A cold thread is refused by the keeper's own facts, as the dashboard's table would refuse it.
	n.hold("sess-c", 10*time.Minute)
	if w := n.post(`{"session":"sess-c"}`); w.Code != http.StatusConflict {
		t.Errorf("a cold thread = %d %s, want 409", w.Code, w.Body)
	}
	if w := n.post(`{"session":"nope"}`); w.Code != http.StatusNotFound {
		t.Errorf("an unknown session = %d, want 404", w.Code)
	}

	// The credential lives in memory only.
	if strings.Contains(logs.String(), "SECRET") {
		t.Errorf("the credential reached the logs:\n%s", logs.String())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("a held ping wrote %d entries into the state dir, want none: %v", len(ents), ents)
	}
}

// Hosted, no recorder: unchanged. Nothing is held, no ping is sent, and the Ping route refuses.
func TestHostedWithNoRecorderHoldsNothing(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetAuth(f.h.DashAuth())
	clock := &testClock{at: time.Now()}
	fs := &fakeSender{}
	useFakeUpstream(f.h, fs, clock)
	a, aTok := f.register(t, "a@ibm.com")
	f.h.rec = nil // hosted, with no audit sink
	if f.h.keeper.mayHold() {
		t.Fatal("a hosted proxy with no recorder may hold a credential")
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+fakeSingleTenantKey)
	for i := 0; i < 2; i++ {
		f.h.keeper.record(&Tenancy{ID: a.ID, Cache: kaPolicy()}, a.ID+":s1", "", clock.now().Add(time.Duration(i-60)*time.Second),
			[]byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	if got := f.h.keeper.Stats().Live; got != 0 {
		t.Fatalf("held %d threads in hosted mode with no recorder, want 0", got)
	}
	if f.h.keeper.canPing(&Tenancy{ID: a.ID, Cache: kaPolicy()}, a.ID+":s1", "", bschemas.Anthropic) {
		t.Error("canPing is true for a hosted proxy with no recorder")
	}
	f.h.keeper.Sweep(clock.advance(281 * time.Second))
	if fs.n() != 0 {
		t.Errorf("a hosted proxy with no recorder sent %d pings", fs.n())
	}
	req := httptest.NewRequest(http.MethodPost, "/api/me/cache/ping", strings.NewReader(`{"session":"s1"}`))
	req.Header.Set("x-context-guru-token", aTok)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("the Ping route in hosted mode with no recorder = %d, want 404", w.Code)
	}
}

// Hosted with a recorder: a thread is held, as before.
func TestHostedWithARecorderStillHolds(t *testing.T) {
	f := newMgrFixture(t)
	clock := &testClock{at: time.Now()}
	useFakeUpstream(f.h, &fakeSender{}, clock)
	a, _ := f.register(t, "a@ibm.com")
	if !f.h.keeper.mayHold() {
		t.Fatal("a hosted proxy with a recorder may not hold a credential")
	}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-a")
	for i := 0; i < 2; i++ {
		f.h.keeper.record(&Tenancy{ID: a.ID, Cache: kaPolicy()}, a.ID+":s1", "", clock.now().Add(time.Duration(i-60)*time.Second),
			[]byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	if got := f.h.keeper.Stats().Live; got != 1 {
		t.Fatalf("held %d threads in hosted mode with a recorder, want 1", got)
	}
}

// Fail open: when the hosted signal cannot be read, the answer is the old rule (a sink is needed).
func TestMayHoldCredentialsFailsToTheOldRule(t *testing.T) {
	boom := func() bool { panic("signal unreadable") }
	if keepalive.MayHoldCredentials(boom, false) { // the panic case: no sink, so the old rule says no
		t.Error("an unreadable hosted signal with no sink may hold")
	}
	if !keepalive.MayHoldCredentials(boom, true) {
		t.Error("an unreadable hosted signal with a sink may not hold")
	}
	for _, c := range []struct{ hosted, sink, want bool }{
		{false, false, true}, {false, true, true}, {true, true, true}, {true, false, false},
	} {
		if got := keepalive.MayHoldCredentials(func() bool { return c.hosted }, c.sink); got != c.want {
			t.Errorf("hosted=%v sink=%v = %v, want %v", c.hosted, c.sink, got, c.want)
		}
	}
	var nilK *keeper
	if nilK.mayHold() {
		t.Error("a nil keeper may hold")
	}
}

// A 1-hour-tier entry reports a 59-minute-class lifetime from the keeper, with no dashboard.
func TestWarmFactsReportsTheOneHourTier(t *testing.T) {
	n := newNoDashHost(t)
	at := n.clock.now()
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+fakeSingleTenantKey)
	body := strings.ReplaceAll(kaBody, `{"type":"ephemeral"}`, `{"type":"ephemeral","ttl":"1h"}`)
	for i := 0; i < 2; i++ {
		n.h.keeper.record(&Tenancy{ID: "", Cache: kaPolicy(), Pipe: components.NewPipeline(nil, n.sink)}, "sess-h", "",
			at.Add(time.Duration(i-1)*time.Second), []byte(body), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}, true)
	}
	_, ttl, ok := n.h.keeper.Keeper.WarmFacts(keepalive.Key{Tenant: "", Session: "sess-h"})
	if !ok || ttl != time.Hour {
		t.Fatalf("WarmFacts = ttl %v ok %v, want 1h true", ttl, ok)
	}
	// 30 minutes after the last touch the press is not cold on a 1-hour entry.
	n.clock.advance(30 * time.Minute)
	// The press must succeed: a 404, a 409 "not held" or a 409 "cold" all fail here.
	if w := n.post(`{"session":"sess-h"}`); w.Code != http.StatusOK {
		t.Fatalf("press on a 1-hour entry 30 minutes old = %d %s, want 200", w.Code, w.Body)
	}
	if n.fs.n() != 1 {
		t.Errorf("upstream saw %d requests, want the one press", n.fs.n())
	}
}

// Keep-alive off, header on, single-tenant, no dashboard: the thread is held for the button only.
func TestHeaderHoldWorksWithNoDashboard(t *testing.T) {
	n := newNoDashHost(t)
	at := n.clock.now().Add(-40 * time.Second)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer "+fakeSingleTenantKey)
		r.Header.Set(ManualHoldHeader, "1")
		n.h.keeper.record(&Tenancy{ID: "", Cache: CachePolicy{}, Pipe: components.NewPipeline(nil, n.sink)}, "sess-off", "",
			at.Add(time.Duration(i)*time.Second), []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	if got := n.h.keeper.Stats().Live; got != 1 {
		t.Fatalf("held %d threads, want 1", got)
	}
	if w := n.post(`{"session":"sess-off"}`); w.Code != http.StatusOK {
		t.Fatalf("press = %d %s, want 200", w.Code, w.Body)
	}
	n.clock.advance(10 * time.Minute)
	n.h.keeper.Sweep(n.clock.now())
	if n.fs.n() != 1 {
		t.Errorf("upstream saw %d requests, want only the press", n.fs.n())
	}
}
