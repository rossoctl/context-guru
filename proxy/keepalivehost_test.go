package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/credmask"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/tenant"
)

// The proxy as the keep-alive's host: policy resolution, the audit-sink rule, the caller's
// credential (httpDispatcher), and the wiring into Handler. The keeper's own decisions — timing,
// gates, the hard deadline, the write-instead-of-read guard — are tested in package keepalive.

// A body shaped like the traffic this mechanism exists for: Claude Code's layout, two
// `system` breakpoints and one on the last content block, which is 54.2% of production
// requests and 86.4% of its spend.
const kaBody = `{"model":"aws/claude-sonnet-5","max_tokens":32000,"stream":true,` +
	`"tools":[{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}],` +
	`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}},` +
	`{"type":"text","text":"Current branch: main","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi",` +
	`"cache_control":{"type":"ephemeral"}}]}]}`

// testClock is a settable clock the ping goroutines may read concurrently with the test
// advancing it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	return c.at
}

// inline runs each ping on the sweep's own goroutine, so a test's assertions run after it.
func inline(f func()) { f() }

// testKeeper builds a proxy keeper with a stub clock, an inline runner and a fake upstream
// transport, so the host is tested through the real httpDispatcher without a network.
func testKeeper(t *testing.T, lim Limits) (*keeper, *fakeSender, *testClock) {
	t.Helper()
	// A recorder, because retention REQUIRES an audit sink: an audit control that
	// --dashboard removes is not a control, so with no recorder the keeper holds nothing. Tests
	// about timing and policy need it present; TestNoAuditSinkMeansNoRetention clears it.
	fs := &fakeSender{}
	h := &Handler{opts: Options{}, limiter: NewLimiter(lim), rec: &dash.Recorder{},
		client: &http.Client{Transport: fs}}
	clock := &testClock{at: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	k := newKeeperWith(h, keepalive.Options{Now: clock.now, Go: inline})
	return k, fs, clock
}

// rebuildKeeper replaces a New()-built handler's keeper with one on a test clock and an inline
// runner, sending through fs. The handler's own keeper is stopped first.
func rebuildKeeper(h *Handler, fs *fakeSender, clock *testClock) *keeper {
	h.keeper.Stop()
	if fs != nil {
		h.client = &http.Client{Transport: fs}
	}
	h.keeper = newKeeperWith(h, keepalive.Options{Now: clock.now, Go: inline})
	return h.keeper
}

// fakeSender is the upstream, as an http.RoundTripper: every ping the httpDispatcher sends lands
// here, whatever host the recorded upstream names.
type fakeSender struct {
	mu     sync.Mutex
	calls  []sentPing
	usage  Usage
	status int
	err    error
	// fn, when set, answers instead of usage/status/err.
	fn func(r *http.Request, body []byte) (Usage, int, error)
}

type sentPing struct {
	body []byte
	hdr  http.Header
	up   upstream
}

// n is the call count, read under the lock.
func (f *fakeSender) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, sentPing{body: body, hdr: r.Header.Clone(),
		up: upstream{base: r.URL.Scheme + "://" + r.URL.Host, path: r.URL.Path}})
	fn, u, st, err := f.fn, f.usage, f.status, f.err
	f.mu.Unlock()
	if fn != nil {
		u, st, err = fn(r, body)
	} else if u.CacheRead == 0 && u.Output == 0 && u.FreshInput == 0 && u.CacheWrite == 0 {
		u = Usage{CacheRead: 48576, Output: 1}
	}
	if err != nil {
		return nil, err
	}
	if st == 0 {
		st = http.StatusOK
	}
	js := fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":%d,`+
		`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`,
		u.FreshInput, u.Output, u.CacheRead, u.CacheWrite)
	return &http.Response{StatusCode: st, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(js)), Request: r}, nil
}

func kaPolicy() CachePolicy {
	return CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
}

// recordOne registers one finished request with the keeper, on a session that has already
// sent one — because the gate skips a session's FIRST request.
func recordOne(t *testing.T, k *keeper, pol CachePolicy, body string, at time.Time, up upstream) {
	t.Helper()
	recordTurn(t, k, pol, body, at.Add(-time.Second), up)
	recordTurn(t, k, pol, body, at, up)
}

// recordTurn registers exactly one request, without pre-seeding a turn.
func recordTurn(t *testing.T, k *keeper, pol CachePolicy, body string, at time.Time, up upstream) {
	t.Helper()
	tn := &Tenancy{ID: "t1", Cache: pol}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	k.record(tn, "sess-1", "", at, []byte(body), up, r, bschemas.Anthropic, "/v1/messages",
		http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
}

// waitPings waits until n pings are counted. With the inline runner it returns at once.
func waitPings(t *testing.T, k *keeper, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k.Stats().Pings >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pings = %d after 2s, want %d", k.Stats().Pings, n)
}

func waitSkipped(t *testing.T, k *keeper, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k.Stats().Skipped >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("skipped = %d after 2s, want %d", k.Stats().Skipped, n)
}

// syncBuffer is a log sink a background goroutine may write while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// unmasked returns a plaintext COPY, so a test can assert on the value without disturbing the
// masked buffer being held.
func unmasked(b []byte) []byte {
	out := append([]byte(nil), b...)
	credmask.Mask(out)
	return out
}

// heldFor is what the dispatcher holds for one key, read under its lock.
func heldFor(k *keeper, key keepalive.Key) (heldUpstream, bool) {
	k.d.mu.Lock()
	defer k.d.mu.Unlock()
	h, ok := k.d.held[key]
	return h, ok
}

// pingWireBody is what a ping of body puts on the wire: the keeper's pingBody for the Messages
// route — max_tokens 1, stream false, nothing else touched. Restated here because pingBody is
// keepalive's; package keepalive pins pingBody itself.
func pingWireBody(t *testing.T, body []byte) []byte {
	t.Helper()
	// Byte-level, not via a map round trip, which would reorder keys.
	out := bytes.Replace(body, []byte(`"max_tokens":32000`), []byte(`"max_tokens":1`), 1)
	out = bytes.Replace(out, []byte(`"stream":true`), []byte(`"stream":false`), 1)
	return out
}

// A ping is a real request against the tenant's budget, and it must lose to real traffic
// rather than displace it. With a concurrency limit of 1 there is no slack at all, so no
// ping may be sent: the proxy's limiter is what the keeper's Spare asks.
func TestPingNeverUsesTheLastConcurrencySlot(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{Concurrent: 1})
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	// Hold the only slot, as an in-flight agent request would.
	release, err := k.h.limiter.Acquire("t1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	k.Sweep(clock.advance(281 * time.Second))
	waitSkipped(t, k, 1)
	if fs.n() != 0 {
		t.Fatalf("sent %d pings while the tenant had no spare concurrency", fs.n())
	}
}

// The property dash/keepalivestrategy.go's per-strategy ledger is built on: arrive() must
// report the SAME strategy id the ping that rescued this session was itself tagged with,
// because that id is what lets a real request's credit be attributed to one strategy instead
// of the tenant's whole lifetime credit repeating under every strategy that ever touched them.
func TestArriveReportsTheStrategyThatSentThePing(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	k.setStrategies([]tenant.Strategy{{
		ID: "s1", Active: true, Target: tenant.Target{Mode: tenant.TargetAll},
		Windows:     []tenant.Window{{Start: "00:00", End: "23:59"}},
		IdleSeconds: 280, MaxPings: 2, MaxUSDPerPing: 0.25, MinPrefixTokens: 20000,
	}})
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)

	_, _, strategyID := k.arrive("t1", "sess-1", "")
	if strategyID != "s1" {
		t.Errorf("strategyID = %q, want %q — a real request's credit must trace back to the "+
			"strategy that sent the ping which earned it", strategyID, "s1")
	}
}

// The credential rules the proxy owns: the dispatcher holds the caller's credential, MASKED, and
// only for a caller-pays upstream, and every release ZEROIZES it. The keeper's side — that it
// asks for a hold only while tracking and releases on every path — is tested in keepalive.
func TestCredentialRetentionRules(t *testing.T) {
	const caller = "Bearer sk-caller-secret"
	s := keepalive.Key{Tenant: "t1", Session: "s"}

	held := func(t *testing.T, up upstream) (*keeper, *fakeSender, *testClock) {
		t.Helper()
		k, fs, clock := testKeeper(t, Limits{})
		tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", caller)
		r.Header.Add("x-api-key", "cg_live_0123456789abcdef0123456789abcdef")
		// Twice, and over the prefix floor: the gate refuses a session's first request, and the
		// subject here is the credential rather than the gate.
		for i := 0; i < 2; i++ {
			k.record(tn, "s", "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
		if _, ok := k.Entry(s); !ok {
			t.Fatal("session not tracked")
		}
		return k, fs, clock
	}

	t.Run("caller pays: the credential is held MASKED, never plaintext", func(t *testing.T) {
		k, _, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		h, ok := heldFor(k, s)
		if !ok {
			t.Fatal("the dispatcher holds nothing for a tracked caller-pays session")
		}
		var auth *credmask.Header
		for i := range h.auth {
			if strings.EqualFold(h.auth[i].Name, "Authorization") {
				auth = &h.auth[i]
			}
			// Our own token is not a provider credential and must never be retained.
			if bytes.Contains(unmasked(h.auth[i].Val), []byte("cg_live_")) {
				t.Error("retained a context-guru token as a provider credential")
			}
		}
		if auth == nil {
			t.Fatal("no Authorization retained; a caller-pays ping has no other credential")
		}
		// The bytes AT REST must not contain the credential — that is what a heap dump or a
		// string scan over a core file would see.
		if bytes.Contains(auth.Val, []byte("sk-caller-secret")) {
			t.Error("the credential is sitting in memory in plaintext")
		}
		// And it must still be recoverable, or the ping cannot authenticate.
		if got := string(unmasked(auth.Val)); got != caller {
			t.Errorf("unmasking gave %q, want the caller's own credential back", got)
		}
	})

	t.Run("server key: nothing is retained", func(t *testing.T) {
		k, fs, clock := held(t, upstream{base: "http://up", path: "/v1/messages",
			setKey: func(h http.Header) { h.Set("x-api-key", "operator") }})
		h, _ := heldFor(k, s)
		if len(h.auth) != 0 {
			t.Errorf("retained %d auth headers although the operator holds the key", len(h.auth))
		}
		// And the ping goes out with the operator's key and none of the caller's auth slots.
		if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
			t.Fatalf("no ping fired (%d)", n)
		}
		fs.mu.Lock()
		hdr := fs.calls[0].hdr
		fs.mu.Unlock()
		if got := hdr.Get("x-api-key"); got != "operator" {
			t.Errorf("ping x-api-key = %q, want the operator's key", got)
		}
		if got := hdr.Get("Authorization"); got != "" {
			t.Errorf("ping carried the caller's Authorization %q on a server-key upstream", got)
		}
	})

	t.Run("release ZEROIZES rather than dropping the reference", func(t *testing.T) {
		k, _, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		h, _ := heldFor(k, s)
		if len(h.auth) == 0 || len(h.auth[0].Val) == 0 {
			t.Fatal("nothing held to release")
		}
		// Our own handle on the exact buffer, so the assertion is about the BYTES and not about
		// whether the map still points at them.
		cred := h.auth[0].Val
		k.retireSession("t1", "s")
		for i := range cred {
			if cred[i] != 0 {
				t.Fatalf("released credential still holds data at byte %d; a dump taken now "+
					"would still yield it", i)
			}
		}
		if _, ok := heldFor(k, s); ok {
			t.Error("the dispatcher still holds the session after retirement")
		}
	})

	t.Run("forget zeroizes one tenant's credential", func(t *testing.T) {
		k, _, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		h, _ := heldFor(k, s)
		cred := h.auth[0].Val
		k.forget("t1")
		for i := range cred {
			if cred[i] != 0 {
				t.Fatalf("the forgotten tenant's credential still holds data at byte %d", i)
			}
		}
	})

	t.Run("Stop drops every held credential", func(t *testing.T) {
		k, _, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		k.Stop()
		if _, ok := heldFor(k, s); ok {
			t.Fatal("the dispatcher still holds a credential after Stop")
		}
	})
}

// End to end against a real HTTP server: the ping actually goes out, carries the caller's
// credential, and reaches the upstream as the exact body the keeper handed the dispatcher.
func TestSendPingHitsTheUpstream(t *testing.T) {
	// The handler runs on the test server's goroutine; the HTTP round trip is not a
	// happens-before edge, so what it saw comes back over a buffered channel rather than
	// through a captured variable the test goroutine would read unsynchronised.
	type seen struct {
		body []byte
		auth string
		path string
	}
	seenCh := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenCh <- seen{b, r.Header.Get("Authorization"), r.URL.Path}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":0,"output_tokens":1,` +
			`"cache_read_input_tokens":48576,"cache_creation_input_tokens":0},"stop_reason":"max_tokens"}`))
	}))
	defer srv.Close()

	d := newHTTPDispatcher(srv.Client())
	key := keepalive.Key{Tenant: "t1", Session: "s"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-caller")
	d.hold(key, upstream{base: srv.URL, path: "/v1/messages"}, r)
	body := pingWireBody(t, []byte(kaBody))
	u, status, err := d.Dispatch(context.Background(), keepalive.DispatchRequest{Key: key,
		Provider: bschemas.Anthropic, Route: "/v1/messages", Body: body,
		Headers: keepalive.PingHeaders(r.Header)})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Dispatch: status=%d err=%v", status, err)
	}
	if u.CacheRead != 48576 || u.CacheWrite != 0 {
		t.Errorf("usage read=%d write=%d; a ping must READ the prefix, not write it", u.CacheRead, u.CacheWrite)
	}
	got := <-seenCh
	if got.auth != "Bearer sk-caller" {
		t.Errorf("upstream saw Authorization %q", got.auth)
	}
	if got.path != "/v1/messages" {
		t.Errorf("upstream path = %q", got.path)
	}
	if !bytes.Equal(got.body, body) {
		t.Errorf("the upstream did not receive the dispatched body byte for byte")
	}
	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("the upstream did not receive valid JSON: %v", err)
	}
	if sent["max_tokens"] != float64(1) || sent["stream"] != false {
		t.Errorf("upstream received max_tokens=%v stream=%v", sent["max_tokens"], sent["stream"])
	}
}

// Item 6 of the hardening bar: the retained credential and the retained request body must be
// unreachable from every output surface. The body matters as much as the key — it holds the
// user's whole conversation.
//
// Debug logging is ON, because that is the realistic leak path: production runs at debug and
// produces roughly 8x the line volume, so a leak that only appears there is a leak that only
// appears in production.
func TestRetainedMaterialReachesNoOutputSurface(t *testing.T) {
	const cred = "sk-caller-DO-NOT-LEAK-9f3a"
	const secretInBody = "MY-PRIVATE-SOURCE-CODE-MARKER"

	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A REAL recorder on an in-memory database, wired through New() like the dashboard tests do.
	// Retention requires an audit sink, and a recorder assigned after construction is only
	// half-wired — /metrics would dereference its absent database. This also makes the surfaces
	// under test the fully-wired ones a deployment actually serves.
	sink, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: sink})
	defer h.Close()
	clock := &testClock{at: time.Now()}
	k := rebuildKeeper(h, &fakeSender{}, clock)

	body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+secretInBody+`"`, 1)
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+cred)
	for i := 0; i < 2; i++ {
		k.record(tn, "leaky", "", clock.now().Add(time.Duration(i)*time.Second), []byte(body),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
			"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if k.Stats().Live != 1 {
		t.Fatal("nothing retained, so nothing is being tested")
	}
	// Drive a ping, so the log carries whatever that path emits.
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)

	surfaces := map[string]string{}
	rec := httptest.NewRecorder()
	h.stats(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	surfaces["/stats"] = rec.Body.String()
	rec = httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	surfaces["/metrics"] = rec.Body.String()
	// The keeper's own snapshot, which /stats embeds and a future consumer might render alone.
	surfaces["KeepAliveStats"] = fmt.Sprintf("%+v", k.Stats())
	// A panic trace over the held material: a struct dumped into an error message is the
	// classic accidental disclosure, and %+v on what is held must not spell out either secret.
	key := keepalive.Key{Tenant: "t1", Session: "leaky"}
	ev, _ := k.Entry(key)
	surfaces["entry %+v"] = fmt.Sprintf("%+v", ev)
	hu, _ := heldFor(k, key)
	surfaces["held upstream %+v"] = fmt.Sprintf("%+v", hu)
	surfaces["log sink (debug)"] = logs.String()

	for name, out := range surfaces {
		if out == "" && name != "log sink (debug)" {
			t.Errorf("%s produced no output, so this assertion proves nothing", name)
		}
		if strings.Contains(out, cred) {
			t.Errorf("%s LEAKS the retained credential", name)
		}
		if strings.Contains(out, secretInBody) {
			t.Errorf("%s LEAKS the retained request body", name)
		}
	}
	if !strings.Contains(logs.String(), "keep-alive ping") {
		t.Error("the debug log carries no keep-alive line, so the leak check never saw this path")
	}
}

// The audit row is what makes the credential arrangement reviewable, so it cannot be optional.
// An audit control that `--dashboard` silently removes is not a control: with no recorder there
// is no per-ping record, so nothing is retained at all.
func TestNoAuditSinkMeansNoRetention(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	k.h.rec = nil // as a deployment without --dashboard has it

	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("held %d sessions with no audit sink; a credential we cannot account for "+
			"must not be held", got)
	}
	if _, ok := heldFor(k, keepalive.Key{Tenant: "t1", Session: "sess-1"}); ok {
		t.Fatal("the dispatcher holds a credential with no audit sink")
	}
}

// The OpenAI ping cadence is the host's to resolve: record runs an OpenAI request on
// OpenAIIdle (or the 28-minute default), not on the Anthropic Idle. (Which OpenAI models are
// eligible at all is keepalive's TestOpenAIKeepAliveOnlyKnownModelsAreEligible.)
func TestOpenAIKeepAliveUsesItsOwnLifetime(t *testing.T) {
	if modelinfo.OpenAIDefaultKeepAliveIdle != time.Duration(config.DefaultKeepAliveOpenAIIdle)*time.Second {
		t.Fatal("proxy and config disagree about the default OpenAI ping interval")
	}
	for _, tc := range []struct {
		name string
		idle time.Duration
	}{
		{"default", modelinfo.OpenAIDefaultKeepAliveIdle},
		{"configured", 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			pol := kaPolicy()
			pol.MinPrefixTokens = 1000
			pol.MaxPings = 1
			if tc.name == "configured" {
				pol.OpenAIIdle = tc.idle
			}
			body := []byte(`{"model":"azure/gpt-5.6-luna","input":[{"role":"user","content":"hello"}],"stream":true}`)
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
			req.Header.Set("Authorization", "Bearer test-key")
			up := upstream{base: "http://up", path: "/v1/responses"}
			for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
				k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", at, body, up, req,
					bschemas.OpenAI, up.path, http.StatusOK,
					Usage{CacheRead: 4000, CacheWrite: 100}, true)
			}
			start := clock.now()
			if got := k.Sweep(start.Add(tc.idle - time.Second)); got != 0 {
				t.Fatalf("ping fired early: %d", got)
			}
			if got := k.Sweep(start.Add(tc.idle)); got != 1 {
				t.Fatalf("ping did not fire at %s: %d", tc.idle, got)
			}
		})
	}
}

func TestOpenAIKeepAliveIgnoresAnthropicIntervalControls(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.setStrategies([]tenant.Strategy{{
		ID: "short", Active: true, IdleSeconds: 60, MaxPings: 2,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}})
	armOn(t, k, "openai-session", 60*time.Second, 2, now.Add(time.Hour))
	pol := kaPolicy()
	pol.KeepAlive = false // the manual arm must enable this one session
	pol.OpenAIIdle = 28 * time.Minute
	pol.MinPrefixTokens = 1000
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	up := upstream{base: "http://up", path: "/v1/responses"}
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now, body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now.Add(time.Second), body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	e, ok := k.Entry(keepalive.Key{Tenant: "t1", Session: "openai-session"})
	if !ok {
		t.Fatal("eligible OpenAI entry was not retained")
	}
	if !e.Policy.KeepAlive || e.Policy.Idle != 28*time.Minute || e.Policy.MaxPings != 1 ||
		e.Policy.MinPrefixTokens != 0 || e.Tag != "" {
		t.Fatalf("manual OpenAI override was ineffective or imported an Anthropic cadence: policy=%+v strategy=%q",
			e.Policy, e.Tag)
	}
	if hold := time.Duration(e.Policy.MaxPings+1) * e.Policy.Idle; hold > maxOverrideHold {
		t.Fatalf("OpenAI override retained credentials for %s, beyond %s", hold, maxOverrideHold)
	}
	k.disarm("t1", "openai-session")
	k.record(&Tenancy{ID: "t1", Cache: pol}, "openai-session", "", now.Add(2*time.Second), body, up, req,
		bschemas.OpenAI, up.path, http.StatusOK, Usage{CacheRead: 4000}, true)
	if _, ok := k.Entry(keepalive.Key{Tenant: "t1", Session: "openai-session"}); ok {
		t.Fatal("disarmed OpenAI session remained retained on an account with keep-alive off")
	}
}
