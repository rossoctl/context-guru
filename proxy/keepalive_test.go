package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/tidwall/gjson"
)

// fixedPrice prices every model the same, so a cost guard can be tested without a price file.
type fixedPrice struct{ p modelinfo.Price }

func (f fixedPrice) Price(context.Context, string) (modelinfo.Price, bool) { return f.p, true }

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
// advancing it. A plain *time.Time raced: fire() reads k.now() on its own goroutine.
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

// testKeeper builds a keeper with a stub clock and a stub sender, so the whole policy is
// testable without a network and without waiting 280 real seconds.
func testKeeper(t *testing.T, lim Limits) (*keeper, *fakeSender, *testClock) {
	t.Helper()
	h := &Handler{opts: Options{}, limiter: NewLimiter(lim)}
	k := newKeeper(h)
	clock := &testClock{at: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	k.now = clock.now
	fs := &fakeSender{}
	k.send = fs.send
	return k, fs, clock
}

type fakeSender struct {
	mu     sync.Mutex
	calls  []sentPing
	usage  Usage
	status int
	err    error
}

// n is the call count, read under the lock: sweep dispatches each ping on its own goroutine
// (a round trip must not be made under the keeper's map lock), so the counter this fake keeps
// is genuinely concurrent.
func (f *fakeSender) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type sentPing struct {
	body []byte
	hdr  http.Header
	up   upstream
}

func (f *fakeSender) send(j pingJob, body []byte) (Usage, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, sentPing{body: body, hdr: j.hdr.Clone(), up: j.up})
	st := f.status
	if st == 0 {
		st = http.StatusOK
	}
	u := f.usage
	if u == (Usage{}) {
		u = Usage{CacheRead: 48576, Output: 1}
	}
	err := f.err
	f.mu.Unlock()
	return u, st, err
}

func kaPolicy() CachePolicy {
	return CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
}

// recordOne registers one finished request with the keeper, on a session that has already
// sent one — because the gate skips a session's FIRST request and every timing test below is
// about the span AFTER a real turn.
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
	k.record(tn, "sess-1", at, []byte(body), up, r, bschemas.Anthropic, "/v1/messages",
		http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
}

// The headline invariant: a ping must be a pure cache READ. It is a read only if the hashed
// prefix is byte-identical, so the ping may differ from the recorded request in `max_tokens`
// and `stream` and in NOTHING that the provider hashes.
func TestPingBodyLeavesTheHashedPrefixByteIdentical(t *testing.T) {
	out, ok := pingBody([]byte(kaBody))
	if !ok {
		t.Fatal("pingBody refused a well-formed body")
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 1 {
		t.Errorf("max_tokens = %d, want 1 (the cheapest legal output)", got)
	}
	if gjson.GetBytes(out, "stream").Bool() {
		t.Error("stream is still true; the ping must come back as one small JSON body")
	}
	// The prefix the provider hashes, in the order it hashes it. Any difference here is a
	// cache MISS, and a miss on a ping costs 12.5x a read instead of saving 11.5x.
	for _, path := range []string{"model", "tools", "system", "messages", "thinking", "tool_choice"} {
		a := gjson.GetBytes([]byte(kaBody), path).Raw
		b := gjson.GetBytes(out, path).Raw
		if a != b {
			t.Errorf("%s changed:\n before: %s\n  after: %s", path, a, b)
		}
	}
	// And the breakpoints themselves, counted: a ping that added or dropped one would ask
	// the provider for a different entry than the one it is trying to refresh.
	if a, b := strings.Count(kaBody, "cache_control"), strings.Count(string(out), "cache_control"); a != b {
		t.Errorf("breakpoint count changed: %d -> %d", a, b)
	}
}

// The provider bills a request that reports more written than read as a CREATION, which is
// the one outcome that makes this mechanism cost money instead of saving it. It must be
// loud, counted, and terminal for that session.
func TestPingThatWritesInsteadOfReadingStopsTheSession(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	fs.usage = Usage{CacheWrite: 48576, CacheRead: 0, Output: 1}
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})

	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if got := k.wrote.Load(); got != 1 {
		t.Fatalf("wrote_instead_of_read = %d, want 1", got)
	}
	// K is 2, so without the stop a second ping would follow. It must not.
	if n := k.sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("sweep fired %d more pings after a write; the session must be dropped", n)
	}
}

// The provider measures the lifetime "from the start of the request that writes or reads the
// cache entry, not from the end of its response", so the idle clock starts at the recorded
// request's START and a ping fires exactly X after it — not sooner.
func TestTimingIsMeasuredFromRequestStart(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start, upstream{base: "http://up", path: "/v1/messages"})

	for _, elapsed := range []time.Duration{0, 100 * time.Second, 279 * time.Second} {
		if n := k.sweep(start.Add(elapsed)); n != 0 {
			t.Fatalf("pinged after %s idle; X is 280s", elapsed)
		}
	}
	if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("no ping at exactly X=280s (fired %d)", n)
	}
}

// K bounds one idle span, and the second ping's deadline is measured from the FIRST ping's
// start — a ping is itself a request that reads the entry, so it restarts the lifetime.
func TestMaxPingsAndPingRestartsTheClock(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start, upstream{base: "http://up", path: "/v1/messages"})

	if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("first ping did not fire (%d)", n)
	}
	// 279s after the ping is 559s after the request: too early, because the ping refreshed
	// the entry and the clock restarted at it.
	if n := k.sweep(start.Add(559 * time.Second)); n != 0 {
		t.Fatalf("second ping fired %d times before X elapsed from the FIRST ping", n)
	}
	if n := k.sweep(start.Add(560 * time.Second)); n != 1 {
		t.Fatalf("second ping did not fire (%d)", n)
	}
	// K = 2.
	if n := k.sweep(start.Add(1000 * time.Second)); n != 0 {
		t.Fatalf("fired a third ping; K is 2 (%d)", n)
	}
}

// A ping is a real request against the tenant's budget, and it must lose to real traffic
// rather than displace it. With a concurrency limit of 1 there is no slack at all, so no
// ping may be sent.
func TestPingNeverUsesTheLastConcurrencySlot(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{Concurrent: 1})
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	// Hold the only slot, as an in-flight agent request would.
	release, err := k.h.limiter.Acquire("t1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	k.sweep(clock.advance(281 * time.Second))
	waitSkipped(t, k, 1)
	if fs.n() != 0 {
		t.Fatalf("sent %d pings while the tenant had no spare concurrency", fs.n())
	}
}

// Fail open and quiet: a transport error is logged, counted and forgotten. It must not panic,
// must not retry inside the sweep, and must still count against K so a broken upstream cannot
// be pinged forever.
func TestPingErrorFailsOpen(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	fs.err = errors.New("dial tcp: connection refused")
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})

	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if got := k.failed.Load(); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if fs.n() != 1 {
		t.Errorf("sent %d pings, want exactly 1 — a failure must not be retried inside the sweep", fs.n())
	}
}

// A 4xx repeats identically, so the session is dropped rather than spending the rest of K
// learning the same thing.
func TestClientErrorStopsTheSession(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	k.send = func(pingJob, []byte) (Usage, int, error) {
		return Usage{}, http.StatusBadRequest, nil
	}
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if n := k.sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("kept pinging a session the upstream refused with 400 (%d)", n)
	}
}

// `thinking.type: enabled` cannot be pinged: the API requires max_tokens above the thinking
// budget (32,000 on this traffic), and changing the thinking parameters invalidates the
// message-level prefix, so neither a cheap ping nor a matching one is possible.
func TestThinkingEnabledIsNotTracked(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	body := strings.Replace(kaBody, `"stream":true`,
		`"stream":true,"thinking":{"type":"enabled","budget_tokens":32000}`, 1)
	recordOne(t, k, kaPolicy(), body, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d thinking-enabled sessions, want 0", got)
	}
	// `adaptive` carries no budget and is unaffected — it is 81% of the addressable traffic.
	adaptive := strings.Replace(kaBody, `"stream":true`,
		`"stream":true,"thinking":{"type":"adaptive"}`, 1)
	recordOne(t, k, kaPolicy(), adaptive, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k.Stats().Live; got != 1 {
		t.Fatalf("adaptive thinking tracked %d sessions, want 1", got)
	}
}

// A request the provider refused left no entry behind, so there is nothing to keep alive and
// no reason to hold its body or its credential.
func TestFailedRequestIsNotTracked(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	for _, status := range []int{0, http.StatusBadRequest, http.StatusBadGateway} {
		k.record(tn, "s", clock.now(), []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", status, Usage{}, false)
	}
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d sessions whose request the provider refused", got)
	}
}

// Off by default: an account that has not opted in is not tracked at all, so no body and no
// credential is held for it.
func TestDisabledPolicyHoldsNothing(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	recordOne(t, k, CachePolicy{}, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d sessions with the keep-alive off", got)
	}
}

// The gate: a session's FIRST request is never pinged, and a prefix below the floor is never
// pinged. Together these drop 8.7x the pings for 3.3% of the money, which is what makes the
// policy deployable rather than merely profitable.
func TestGateSkipsFirstRequestAndSmallPrefixes(t *testing.T) {
	t.Run("the session's first request is not pinged", func(t *testing.T) {
		k, _, clock := testKeeper(t, Limits{})
		recordTurn(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
		if n := k.sweep(clock.advance(281 * time.Second)); n != 0 {
			t.Fatalf("pinged a single-request session (%d); those are 79%% of pings and 0.9%% of the value", n)
		}
	})
	t.Run("the second request is", func(t *testing.T) {
		k, _, clock := testKeeper(t, Limits{})
		recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
		if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
			t.Fatalf("did not ping a session on its second turn (%d)", n)
		}
	})
	t.Run("a prefix below the floor is not pinged", func(t *testing.T) {
		k, _, clock := testKeeper(t, Limits{})
		tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		for i := 0; i < 2; i++ {
			// 19,999 billed tokens: one short of the 20,000 floor.
			k.record(tn, "small", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
				"/v1/messages", http.StatusOK, Usage{CacheRead: 19_999}, true)
		}
		if n := k.sweep(clock.advance(281 * time.Second)); n != 0 {
			t.Fatalf("pinged a session whose billed prefix is under the floor (%d)", n)
		}
	})
}

// The per-ping cost guard. Ping cost is bimodal — p50 $0.0004 against a p99 of $0.2275 and a
// max of $0.3780 — so the outlier to refuse is an individual ping, not a session's total.
func TestPerPingCostGuard(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	k.h.opts.Prices = fixedPrice{modelinfo.Price{Input: 3.8e-6, Output: 19e-6,
		CacheRead: 3.8e-7, CacheWrite: 4.75e-6}}
	pol := kaPolicy()
	pol.MaxUSDPerPing = 0.05
	tn := &Tenancy{ID: "t1", Cache: pol}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	// 400k billed tokens at opus's read rate is $0.152 a ping — over the $0.05 guard. Refused
	// at RECORD time, which is also the security-relevant answer: no body and no credential is
	// held for a session we have already decided not to ping.
	big := func(tn *Tenancy, i int) {
		k.record(tn, "big", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
			"/v1/messages", http.StatusOK, Usage{CacheRead: 400_000}, true)
	}
	big(tn, 0)
	big(tn, 1)
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("held %d sessions whose projected ping exceeds the budget", got)
	}
	if n := k.sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("sent %d pings above the per-ping budget", n)
	}
	// The same traffic under a budget that allows it IS pinged, so the refusal above is the
	// guard and not some other gate.
	pol.MaxUSDPerPing = 0.5
	big(&Tenancy{ID: "t1", Cache: pol}, 2)
	// 300s, not 281s: the clock has already moved on, so the new entry's own deadline is 280s
	// after ITS record time rather than after the start of the test.
	if n := k.sweep(clock.advance(300 * time.Second)); n != 1 {
		t.Fatalf("did not ping once the budget allowed it (%d)", n)
	}
}

// A real request cancels the pending ping and reports what the span's pings did, and the
// entry — with its body and its credential — is dropped at that moment.
func TestArriveCancelsAndReports(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)

	pings, refreshed := k.arrive("t1", "sess-1")
	if pings != 1 {
		t.Errorf("pings = %d, want 1", pings)
	}
	if refreshed != 48576 {
		t.Errorf("refreshed = %d, want the ping's own cache_read of 48576", refreshed)
	}
	if got := k.Stats().Live; got != 0 {
		t.Errorf("still tracking %d sessions after the next request arrived", got)
	}
	if p, r := k.arrive("t1", "sess-1"); p != 0 || r != 0 {
		t.Errorf("arrive reported %d/%d for an untracked session, want 0/0", p, r)
	}
}

// The credential rules, all of them. This is the hardening bar the retention had to clear
// before it could ship, and each subtest is one line of it.
func TestCredentialRetentionRules(t *testing.T) {
	const caller = "Bearer sk-caller-secret"

	held := func(t *testing.T, up upstream) (*keeper, *kaEntry, *testClock) {
		t.Helper()
		k, _, clock := testKeeper(t, Limits{})
		tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", caller)
		r.Header.Add("x-api-key", "cg_live_0123456789abcdef0123456789abcdef")
		// Twice, and over the prefix floor: the gate refuses a session's first request, and the
		// subject here is the credential rather than the gate.
		for i := 0; i < 2; i++ {
			k.record(tn, "s", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
		k.mu.Lock()
		e := k.live[kaKey("t1", "s")]
		k.mu.Unlock()
		return k, e, clock
	}

	t.Run("caller pays: the credential is held MASKED, never plaintext", func(t *testing.T) {
		_, e, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		if e == nil {
			t.Fatal("session not tracked")
		}
		var auth *maskedHeader
		for i := range e.auth {
			if strings.EqualFold(e.auth[i].name, "Authorization") {
				auth = &e.auth[i]
			}
			// Our own token is not a provider credential and must never be retained.
			if bytes.Contains(unmasked(e.auth[i].val), []byte("cg_live_")) {
				t.Error("retained a context-guru token as a provider credential")
			}
		}
		if auth == nil {
			t.Fatal("no Authorization retained; a caller-pays ping has no other credential")
		}
		// The bytes AT REST must not contain the credential — that is what a heap dump or a
		// string scan over a core file would see.
		if bytes.Contains(auth.val, []byte("sk-caller-secret")) {
			t.Error("the credential is sitting in memory in plaintext")
		}
		// And it must still be recoverable, or the ping cannot authenticate.
		if got := string(unmasked(auth.val)); got != caller {
			t.Errorf("unmasking gave %q, want the caller's own credential back", got)
		}
	})

	t.Run("server key: nothing is retained", func(t *testing.T) {
		_, e, _ := held(t, upstream{base: "http://up", path: "/v1/messages",
			setKey: func(h http.Header) { h.Set("x-api-key", "operator") }})
		if e == nil {
			t.Fatal("session not tracked")
		}
		if len(e.auth) != 0 {
			t.Errorf("retained %d auth headers although the operator holds the key", len(e.auth))
		}
		for _, slot := range authHeaders {
			if v := e.hdr.Get(slot); v != "" {
				t.Errorf("retained %s=%q in the plain header set", slot, v)
			}
		}
	})

	t.Run("release ZEROIZES rather than dropping the reference", func(t *testing.T) {
		k, e, _ := held(t, upstream{base: "http://up", path: "/v1/messages"})
		if e == nil {
			t.Fatal("session not tracked")
		}
		// Keep our own handles on the exact buffers, so the assertion is about the BYTES and not
		// about whether the struct still points at them.
		body, cred := e.body, e.auth[0].val
		if len(body) == 0 || len(cred) == 0 {
			t.Fatal("nothing held to release")
		}
		k.retire(kaKey("t1", "s"))
		for _, b := range [][]byte{body, cred} {
			for i := range b {
				if b[i] != 0 {
					t.Fatalf("released buffer still holds data at byte %d; a dump taken now "+
						"would still yield it", i)
					return
				}
			}
		}
		if got := k.Stats().Live; got != 0 {
			t.Errorf("%d sessions still tracked after retirement", got)
		}
	})

	t.Run("the hard deadline fires with no other activity", func(t *testing.T) {
		// A real scheduled deadline, not a sweep check: nothing else runs in this subtest, no
		// request arrives, no sweep is called, and the material must still be gone.
		k, _, clock := testKeeper(t, Limits{})
		pol := kaPolicy()
		pol.Idle = 20 * time.Millisecond // deadline is (K+1) x Idle = 60ms
		tn := &Tenancy{ID: "t1", Cache: pol}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", caller)
		for i := 0; i < 2; i++ {
			k.record(tn, "s", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
				"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
		if k.Stats().Live != 1 {
			t.Fatal("nothing held to expire")
		}
		deadline := time.Now().Add(2 * time.Second)
		for k.Stats().Live != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("%d sessions still held past the hard deadline with nothing else running", got)
		}
	})

	t.Run("withdrawn consent retires what is already held", func(t *testing.T) {
		k, _, clock := testKeeper(t, Limits{})
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", caller)
		on := &Tenancy{ID: "t1", Cache: kaPolicy()}
		for i := 0; i < 2; i++ {
			k.record(on, "s", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
				"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
		if k.Stats().Live != 1 {
			t.Fatal("nothing held to revoke")
		}
		// The account turns the setting off. Its very next request must drop the hold — a stale
		// flag from when the hold began must never be able to extend it.
		off := &Tenancy{ID: "t1", Cache: CachePolicy{}}
		k.record(off, "s", clock.now().Add(3*time.Second), []byte(kaBody),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
			"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("%d sessions still held after consent was withdrawn", got)
		}
	})

	t.Run("the kill switch stops RETENTION, not just pinging", func(t *testing.T) {
		t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
		k, _, clock := testKeeper(t, Limits{})
		recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("held %d sessions with the kill switch set; a switch that keeps the "+
				"material while refusing to use it is the worst of both", got)
		}
	})

	t.Run("Stop drops every held body and credential", func(t *testing.T) {
		k, _, clock := testKeeper(t, Limits{})
		k.start()
		recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
		k.Stop()
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("%d sessions still held after Stop", got)
		}
	})
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
// masked buffer the keeper is holding.
func unmasked(b []byte) []byte {
	out := append([]byte(nil), b...)
	xorMask(out)
	return out
}

// The memory bound. Holding one body per live session is what makes a ping possible; holding
// an unbounded number is what makes a proxy get OOM-killed.
func TestSessionBoundEvicts(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	for i := 0; i < maxKeepAliveSessions+10; i++ {
		k.record(tn, "sess-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+time.Duration(i).String(),
			clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 1}, true)
	}
	if got := k.Stats().Live; got > maxKeepAliveSessions {
		t.Fatalf("tracking %d sessions, bound is %d", got, maxKeepAliveSessions)
	}
}

// A body too large to hold is refused rather than kept: one multi-million-token request must
// not spend a fifth of the whole memory budget.
func TestOversizedBodyIsNotHeld(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	big := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxKeepAliveBodyBytes) + `"}]}`
	recordOne(t, k, kaPolicy(), big, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("held %d oversized sessions", got)
	}
}

// The kill switch, which is an environment variable precisely so it works when the control
// plane is what is broken.
func TestKillSwitch(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	k, _, clock := testKeeper(t, Limits{})
	k.start()
	defer k.Stop()
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	// The sweep loop never starts, so nothing is ever due. sweep itself is still callable —
	// the switch stops the timer, and the test asserts the timer is what is stopped.
	select {
	case <-k.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep goroutine is running with the kill switch set")
	}
}

// End to end against a real HTTP server: the ping actually goes out, carries the caller's
// credential, and reaches the upstream as a max_tokens:1 non-streaming POST of the same body.
func TestSendPingHitsTheUpstream(t *testing.T) {
	var got struct {
		body []byte
		auth string
		path string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		got.body, got.auth, got.path = b[:n], r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":0,"output_tokens":1,` +
			`"cache_read_input_tokens":48576,"cache_creation_input_tokens":0},"stop_reason":"max_tokens"}`))
	}))
	defer srv.Close()

	h := &Handler{opts: Options{}, limiter: NewLimiter(Limits{}), client: srv.Client()}
	k := newKeeper(h)
	body, _ := pingBody([]byte(kaBody))
	u, status, err := k.sendPing(pingJob{
		up:  upstream{base: srv.URL, path: "/v1/messages"},
		hdr: http.Header{"Authorization": {"Bearer sk-caller"}, "Content-Type": {"application/json"}},
	}, body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("sendPing: status=%d err=%v", status, err)
	}
	if u.CacheRead != 48576 || u.CacheWrite != 0 {
		t.Errorf("usage read=%d write=%d; a ping must READ the prefix, not write it", u.CacheRead, u.CacheWrite)
	}
	if got.auth != "Bearer sk-caller" {
		t.Errorf("upstream saw Authorization %q", got.auth)
	}
	if got.path != "/v1/messages" {
		t.Errorf("upstream path = %q", got.path)
	}
	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("the upstream did not receive valid JSON: %v", err)
	}
	if sent["max_tokens"] != float64(1) || sent["stream"] != false {
		t.Errorf("upstream received max_tokens=%v stream=%v", sent["max_tokens"], sent["stream"])
	}
}

// waitPings waits for the sweep's fire goroutines to finish n pings. The sweep dispatches
// asynchronously on purpose (a ping is a round trip and must not be made under the map lock),
// so a test that asserts immediately after sweep races it.
func waitPings(t *testing.T, k *keeper, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k.pings.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pings = %d after 2s, want %d", k.pings.Load(), n)
}

func waitSkipped(t *testing.T, k *keeper, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k.skipped.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("skipped = %d after 2s, want %d", k.skipped.Load(), n)
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

	agg := metrics.NewAggregator()
	h := New(nil, nil, agg, Options{})
	defer h.Close()
	k := h.keeper
	clock := &testClock{at: time.Now()}
	k.now = clock.now
	fs := &fakeSender{}
	k.send = fs.send

	body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+secretInBody+`"`, 1)
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+cred)
	for i := 0; i < 2; i++ {
		k.record(tn, "leaky", clock.now().Add(time.Duration(i)*time.Second), []byte(body),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
			"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if k.Stats().Live != 1 {
		t.Fatal("nothing retained, so nothing is being tested")
	}
	// Drive a ping and a panic-recovery path, so the log carries whatever those emit.
	k.sweep(clock.advance(281 * time.Second))
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
	// A panic trace over the held entry: a struct dumped into an error message is the classic
	// accidental disclosure, and %+v on a keeper entry must not spell out either secret.
	k.mu.Lock()
	surfaces["entry %+v"] = fmt.Sprintf("%+v", *k.live[kaKey("t1", "leaky")])
	k.mu.Unlock()
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
