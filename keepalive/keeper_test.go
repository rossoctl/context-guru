package keepalive

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
)

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

// testKeeper builds a keeper with a stub clock, a fake dispatcher and an inline runner, so the
// whole policy is testable without a network and without waiting 280 real seconds.
func testKeeper(t *testing.T) (*Keeper, *fakeDispatcher, *testClock) {
	t.Helper()
	return testKeeperWith(t, nil)
}

// testKeeperWith is testKeeper with a hook to set further Options (Prices, Spare).
func testKeeperWith(t *testing.T, tweak func(*Options)) (*Keeper, *fakeDispatcher, *testClock) {
	t.Helper()
	fd := newFake(okPing)
	clock := &testClock{at: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	o := Options{Dispatcher: fd, Now: clock.now, Go: func(f func()) { f() }}
	if tweak != nil {
		tweak(&o)
	}
	return New(o), fd, clock
}

func kaPolicy() Policy {
	return Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
}

// sess1 is the key recordOne and recordTurn use.
var sess1 = Key{Tenant: "t1", Session: "sess-1"}

// fakeOf is the fake dispatcher a testKeeper was built with.
func fakeOf(k *Keeper) *fakeDispatcher { return k.o.Dispatcher.(*fakeDispatcher) }

// kaHeader is the request header a recorded test request carries.
func kaHeader() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-caller-secret")
	h.Set("Anthropic-Version", "2023-06-01")
	return h
}

// rec records one finished Anthropic request on key, with the given billed usage, through the
// keeper's own fake as the host.
func rec(k *Keeper, pol Policy, key Key, at time.Time, body string, status int, u Usage, usageOK bool) {
	k.Record(Record{Key: key, StartedAt: at, Body: []byte(body), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: status, Usage: u, UsageOK: usageOK, Policy: pol,
		Header: kaHeader(), Hold: fakeOf(k).hold(key)})
}

// recordOne registers one finished request with the keeper, on a session that has already
// sent one — because the gate skips a session's FIRST request and every timing test below is
// about the span AFTER a real turn.
func recordOne(t *testing.T, k *Keeper, pol Policy, body string, at time.Time) {
	t.Helper()
	recordTurn(t, k, pol, body, at.Add(-time.Second))
	recordTurn(t, k, pol, body, at)
}

// recordTurn registers exactly one request, without pre-seeding a turn.
func recordTurn(t *testing.T, k *Keeper, pol Policy, body string, at time.Time) {
	t.Helper()
	rec(k, pol, sess1, at, body, http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
}

// The headline invariant: a ping must be a pure cache READ. It is a read only if the hashed
// prefix is byte-identical, so the ping may differ from the recorded request in `max_tokens`
// and `stream` and in NOTHING that the provider hashes.
func TestPingBodyLeavesTheHashedPrefixByteIdentical(t *testing.T) {
	out, ok := pingBody([]byte(kaBody), "/v1/messages")
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
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		// A zero read is a first miss now; see TestARealMissIsNotAFailedPing.
		return Usage{CacheWrite: 48576, CacheRead: 1024, Output: 1}, http.StatusOK, nil
	})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())

	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if got := k.wrote.Load(); got != 1 {
		t.Fatalf("wrote_instead_of_read = %d, want 1", got)
	}
	// K is 2, so without the stop a second ping would follow. It must not.
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("sweep fired %d more pings after a write; the session must be dropped", n)
	}
}

// The provider measures the lifetime "from the start of the request that writes or reads the
// cache entry, not from the end of its response", so the idle clock starts at the recorded
// request's START and a ping fires exactly X after it — not sooner.
func TestTimingIsMeasuredFromRequestStart(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start)

	for _, elapsed := range []time.Duration{0, 100 * time.Second, 279 * time.Second} {
		if n := k.Sweep(start.Add(elapsed)); n != 0 {
			t.Fatalf("pinged after %s idle; X is 280s", elapsed)
		}
	}
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("no ping at exactly X=280s (fired %d)", n)
	}
}

// K bounds one idle span, and the second ping's deadline is measured from the FIRST ping's
// start — a ping is itself a request that reads the entry, so it restarts the lifetime.
func TestMaxPingsAndPingRestartsTheClock(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start)

	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("first ping did not fire (%d)", n)
	}
	// 279s after the ping is 559s after the request: too early, because the ping refreshed
	// the entry and the clock restarted at it.
	if n := k.Sweep(start.Add(559 * time.Second)); n != 0 {
		t.Fatalf("second ping fired %d times before X elapsed from the FIRST ping", n)
	}
	if n := k.Sweep(start.Add(560 * time.Second)); n != 1 {
		t.Fatalf("second ping did not fire (%d)", n)
	}
	// K = 2. By 1000s the entry has also spent MaxPings and is past Idle since the second
	// ping's own start (560s + 280s = 840s < 1000s), so Sweep's eager-drop branch removes it —
	// and must release the held credential there, not just at the hard deadline.
	if n := k.Sweep(start.Add(1000 * time.Second)); n != 0 {
		t.Fatalf("fired a third ping; K is 2 (%d)", n)
	}
	if got := fd.holds(sess1); got != 0 {
		t.Errorf("credential still held %d time(s) after Sweep's eager drop", got)
	}
}

// A ping is a real request against the tenant's budget, and it must lose to real traffic
// rather than displace it. When the host's Spare finds no slack, no ping may be sent. (The
// proxy's own limiter — that a concurrency limit of 1 leaves no slack — is tested there.)
func TestPingNeverUsesTheLastConcurrencySlot(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	k, fd, clock := testKeeperWith(t, func(o *Options) {
		o.Spare = func(tenant string, frac float64) (func(), error) {
			mu.Lock()
			asked = append(asked, tenant)
			mu.Unlock()
			if frac != reserveFrac {
				t.Errorf("Spare asked to reserve %v, want %v", frac, reserveFrac)
			}
			return nil, errors.New("no spare concurrency")
		}
	})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())

	k.Sweep(clock.advance(281 * time.Second))
	waitSkipped(t, k, 1)
	if fd.n() != 0 {
		t.Fatalf("sent %d pings while the tenant had no spare concurrency", fd.n())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "t1" {
		t.Errorf("Spare was asked for tenants %v, want t1", asked)
	}
}

// Fail open and quiet: a transport error is logged and counted. It must not panic and must not
// retry inside the sweep. A fast failure is given back for ONE retry at a later sweep tick
// (giveBackFailedPing); the retried flag, not K, is what stops a broken upstream from being pinged
// forever.
func TestPingErrorFailsOpen(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{}, 0, errors.New("dial tcp: connection refused")
	})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())

	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if got := k.failed.Load(); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
	if fd.n() != 1 {
		t.Errorf("sent %d pings, want exactly 1 — a failure must not be retried inside the sweep", fd.n())
	}
}

// A 4xx repeats identically, so the session is dropped rather than spending the rest of K
// learning the same thing.
func TestClientErrorStopsTheSession(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{}, http.StatusBadRequest, nil
	})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("kept pinging a session the upstream refused with 400 (%d)", n)
	}
}

// A 5xx is transient: the status comes from the host's Response, and the next due ping still goes.
func TestServerErrorKeepsTheSession(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{}, http.StatusServiceUnavailable, nil
	})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("a 503 stopped the session: second sweep sent %d pings, want 1", n)
	}
}

// `thinking.type: enabled` cannot be pinged: the API requires max_tokens above the thinking
// budget (32,000 on this traffic), and changing the thinking parameters invalidates the
// message-level prefix, so neither a cheap ping nor a matching one is possible.
func TestThinkingEnabledIsNotTracked(t *testing.T) {
	k, _, clock := testKeeper(t)
	body := strings.Replace(kaBody, `"stream":true`,
		`"stream":true,"thinking":{"type":"enabled","budget_tokens":32000}`, 1)
	recordOne(t, k, kaPolicy(), body, clock.now())
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d thinking-enabled sessions, want 0", got)
	}
	// `adaptive` carries no budget and is unaffected — it is 81% of the addressable traffic.
	adaptive := strings.Replace(kaBody, `"stream":true`,
		`"stream":true,"thinking":{"type":"adaptive"}`, 1)
	recordOne(t, k, kaPolicy(), adaptive, clock.now())
	if got := k.Stats().Live; got != 1 {
		t.Fatalf("adaptive thinking tracked %d sessions, want 1", got)
	}
}

// A request the provider refused left no entry behind, so there is nothing to keep alive and
// no reason to hold its body or its credential.
func TestFailedRequestIsNotTracked(t *testing.T) {
	k, fd, clock := testKeeper(t)
	key := Key{Tenant: "t1", Session: "s"}
	for _, status := range []int{0, http.StatusBadRequest, http.StatusBadGateway} {
		rec(k, kaPolicy(), key, clock.now(), kaBody, status, Usage{}, false)
	}
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d sessions whose request the provider refused", got)
	}
	if got := fd.holds(key); got != 0 {
		t.Fatalf("the host was asked to hold a credential %d times for a refused request", got)
	}
}

// Off by default: an account that has not opted in is not tracked at all, so no body and no
// credential is held for it.
func TestDisabledPolicyHoldsNothing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordOne(t, k, Policy{}, kaBody, clock.now())
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("tracked %d sessions with the keep-alive off", got)
	}
	if got := fd.holds(sess1); got != 0 {
		t.Fatalf("the host holds %d credentials with the keep-alive off", got)
	}
}

// The gate: a session's FIRST request is never pinged, and a prefix below the floor is never
// pinged. Together these drop 8.7x the pings for 3.3% of the money, which is what makes the
// policy deployable rather than merely profitable.
func TestGateSkipsFirstRequestAndSmallPrefixes(t *testing.T) {
	t.Run("the session's first request is not pinged", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		recordTurn(t, k, kaPolicy(), kaBody, clock.now())
		if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
			t.Fatalf("pinged a single-request session (%d); those are 79%% of pings and 0.9%% of the value", n)
		}
	})
	t.Run("the second request is", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		recordOne(t, k, kaPolicy(), kaBody, clock.now())
		if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
			t.Fatalf("did not ping a session on its second turn (%d)", n)
		}
	})
	t.Run("a prefix below the floor is not pinged", func(t *testing.T) {
		k, _, clock := testKeeper(t)
		for i := 0; i < 2; i++ {
			// 19,999 billed tokens: one short of the 20,000 floor.
			rec(k, kaPolicy(), Key{Tenant: "t1", Session: "small"},
				clock.now().Add(time.Duration(i)*time.Second), kaBody, http.StatusOK,
				Usage{CacheRead: 19_999}, true)
		}
		if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
			t.Fatalf("pinged a session whose billed prefix is under the floor (%d)", n)
		}
	})
}

// The per-ping cost guard. Ping cost is bimodal — p50 $0.0004 against a p99 of $0.2275 and a
// max of $0.3780 — so the outlier to refuse is an individual ping, not a session's total.
func TestPerPingCostGuard(t *testing.T) {
	k, _, clock := testKeeperWith(t, func(o *Options) {
		o.Dispatcher.(*fakeDispatcher).setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3.8e-6,
			Output: 19e-6, CacheRead: 3.8e-7, CacheWrite: 4.75e-6}}})
	})
	pol := kaPolicy()
	pol.MaxUSDPerPing = 0.05
	// 400k billed tokens at opus's read rate is $0.152 a ping — over the $0.05 guard. Refused
	// at RECORD time, which is also the security-relevant answer: no body and no credential is
	// held for a session we have already decided not to ping.
	big := func(pol Policy, i int) {
		rec(k, pol, Key{Tenant: "t1", Session: "big"}, clock.now().Add(time.Duration(i)*time.Second),
			kaBody, http.StatusOK, Usage{CacheRead: 400_000}, true)
	}
	big(pol, 0)
	big(pol, 1)
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("held %d sessions whose projected ping exceeds the budget", got)
	}
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
		t.Fatalf("sent %d pings above the per-ping budget", n)
	}
	// The same traffic under a budget that allows it IS pinged, so the refusal above is the
	// guard and not some other gate.
	pol.MaxUSDPerPing = 0.5
	big(pol, 2)
	// 300s, not 281s: the clock has already moved on, so the new entry's own deadline is 280s
	// after ITS record time rather than after the start of the test.
	if n := k.Sweep(clock.advance(300 * time.Second)); n != 1 {
		t.Fatalf("did not ping once the budget allowed it (%d)", n)
	}
}

// A real request cancels the pending ping and reports what the span's pings did, and the
// entry — with its body and its credential — is dropped at that moment.
func TestArriveCancelsAndReports(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)

	pings, refreshed, tag := k.Arrive(sess1)
	if pings != 1 {
		t.Errorf("pings = %d, want 1", pings)
	}
	if refreshed != 48576 {
		t.Errorf("refreshed = %d, want the ping's own cache_read of 48576", refreshed)
	}
	if tag != "" {
		t.Errorf("tag = %q, want \"\" (no tag recorded in this test)", tag)
	}
	if got := k.Stats().Live; got != 0 {
		t.Errorf("still tracking %d sessions after the next request arrived", got)
	}
	if got := fd.holds(sess1); got != 0 {
		t.Errorf("the host still holds %d credentials after the next request arrived", got)
	}
	if p, r, s := k.Arrive(sess1); p != 0 || r != 0 || s != "" {
		t.Errorf("arrive reported %d/%d/%q for an untracked session, want 0/0/\"\"", p, r, s)
	}
}

// Arrive reports the SAME tag the entry was recorded with. The proxy builds its per-strategy
// ledger on this: the tag is the strategy id that resolved the ping's policy, and it is in scope
// at arrival and nowhere else once the entry is cleared. (The proxy's own test covers a real
// strategy end to end.)
func TestArriveReportsTheTagThatSentThePing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
		k.Record(Record{Key: sess1, StartedAt: at, Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576}, UsageOK: true,
			Policy: kaPolicy(), Tag: "s1", Header: kaHeader(), Hold: fd.hold(sess1)})
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)

	if _, _, tag := k.Arrive(sess1); tag != "s1" {
		t.Errorf("tag = %q, want %q — a real request's credit must trace back to the "+
			"strategy that sent the ping which earned it", tag, "s1")
	}
}

// The retention rules the keeper itself owns. The credential is the host's (Record.Hold /
// Dispatcher.Release); what is asserted here is that the keeper asks for it to be held only
// while it tracks the entry, and releases it on every path that drops one. The masking of the
// credential itself is tested with the proxy's dispatcher, which holds it.
func TestCredentialRetentionRules(t *testing.T) {
	held := func(t *testing.T) (*Keeper, *fakeDispatcher, *testClock) {
		t.Helper()
		k, fd, clock := testKeeper(t)
		// Twice, and over the prefix floor: the gate refuses a session's first request, and the
		// subject here is the credential rather than the gate.
		for i := 0; i < 2; i++ {
			rec(k, kaPolicy(), Key{Tenant: "t1", Session: "s"}, clock.now().Add(time.Duration(i)*time.Second),
				kaBody, http.StatusOK, Usage{CacheRead: 48576}, true)
		}
		return k, fd, clock
	}
	s := Key{Tenant: "t1", Session: "s"}

	t.Run("a tracked entry holds exactly one credential", func(t *testing.T) {
		_, fd, _ := held(t)
		if got := fd.holds(s); got != 1 {
			t.Fatalf("the host holds %d credentials for one tracked entry, want 1 — the replaced "+
				"first turn's must have been released", got)
		}
	})

	t.Run("release ZEROIZES rather than dropping the reference", func(t *testing.T) {
		k, fd, _ := held(t)
		k.mu.Lock()
		e := k.live[s.id()]
		k.mu.Unlock()
		if e == nil {
			t.Fatal("session not tracked")
		}
		// Keep our own handle on the exact buffer, so the assertion is about the BYTES and not
		// about whether the struct still points at them.
		body := e.body
		if len(body) == 0 {
			t.Fatal("nothing held to release")
		}
		k.mu.Lock()
		armed := e.timer != nil
		k.mu.Unlock()
		if !armed {
			t.Fatal("a tracked entry has no hard retention deadline")
		}
		k.retire(s.id())
		// The deadline is CANCELLED on release, not left to fire: a stale AfterFunc would later
		// retire — and Release the credential of — a NEWER entry on the same thread.
		k.mu.Lock()
		stale := e.timer != nil
		k.mu.Unlock()
		if stale {
			t.Error("the hard retention deadline was left running after release")
		}
		for i := range body {
			if body[i] != 0 {
				t.Fatalf("released buffer still holds data at byte %d; a dump taken now "+
					"would still yield it", i)
			}
		}
		if got := k.Stats().Live; got != 0 {
			t.Errorf("%d sessions still tracked after retirement", got)
		}
		if got := fd.holds(s); got != 0 {
			t.Errorf("the host still holds %d credentials after retirement", got)
		}
	})

	t.Run("RetireSession cancels the hard deadline", func(t *testing.T) {
		k, fd, _ := held(t)
		k.mu.Lock()
		e := k.live[s.id()]
		armed := e != nil && e.timer != nil
		k.mu.Unlock()
		if !armed {
			t.Fatal("session not tracked with a hard retention deadline")
		}
		k.RetireSession(s.Tenant, s.Session)
		k.mu.Lock()
		stale := e.timer != nil
		k.mu.Unlock()
		if stale {
			t.Error("the hard retention deadline was left running after RetireSession")
		}
		if got := fd.holds(s); got != 0 {
			t.Errorf("the host still holds %d credentials after RetireSession", got)
		}
	})

	t.Run("the hard deadline fires with no other activity", func(t *testing.T) {
		// A real scheduled deadline, not a sweep check: nothing else runs in this subtest, no
		// request arrives, no sweep is called, and the material must still be gone.
		k, fd, clock := testKeeper(t)
		pol := kaPolicy()
		pol.Idle = 20 * time.Millisecond // deadline is (K+1) x Idle = 60ms
		for i := 0; i < 2; i++ {
			rec(k, pol, s, clock.now().Add(time.Duration(i)*time.Second), kaBody, http.StatusOK,
				Usage{CacheRead: 48576}, true)
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
		if got := fd.holds(s); got != 0 {
			t.Fatalf("the hard deadline left %d credentials held", got)
		}
	})

	t.Run("withdrawn consent retires what is already held", func(t *testing.T) {
		k, fd, clock := held(t)
		if k.Stats().Live != 1 {
			t.Fatal("nothing held to revoke")
		}
		// The account turns the setting off. Its very next request must drop the hold — a stale
		// flag from when the hold began must never be able to extend it.
		rec(k, Policy{}, s, clock.now().Add(3*time.Second), kaBody, http.StatusOK,
			Usage{CacheRead: 48576}, true)
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("%d sessions still held after consent was withdrawn", got)
		}
		if got := fd.holds(s); got != 0 {
			t.Fatalf("%d credentials still held after consent was withdrawn", got)
		}
	})

	t.Run("the kill switch stops RETENTION, not just pinging", func(t *testing.T) {
		t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
		k, fd, clock := testKeeper(t)
		recordOne(t, k, kaPolicy(), kaBody, clock.now())
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("held %d sessions with the kill switch set; a switch that keeps the "+
				"material while refusing to use it is the worst of both", got)
		}
		if got := fd.holds(sess1); got != 0 {
			t.Fatalf("%d credentials held with the kill switch set", got)
		}
	})

	t.Run("Shutdown drops every held body and credential", func(t *testing.T) {
		k, fd, clock := testKeeper(t)
		k.Start()
		recordOne(t, k, kaPolicy(), kaBody, clock.now())
		k.Shutdown()
		if got := k.Stats().Live; got != 0 {
			t.Fatalf("%d sessions still held after Shutdown", got)
		}
		if got := fd.holds(sess1); got != 0 {
			t.Fatalf("%d credentials still held after Shutdown", got)
		}
	})
}

// unmasked returns a plaintext COPY, so a test can assert on the value without disturbing the
// masked buffer the keeper is holding.
func unmasked(b []byte) []byte {
	out := append([]byte(nil), b...)
	mask(out)
	return out
}

// The memory bound. Holding one body per live session is what makes a ping possible; holding
// an unbounded number is what makes a proxy get OOM-killed.
func TestSessionBoundEvicts(t *testing.T) {
	k, _, clock := testKeeper(t)
	for i := 0; i < MaxSessions+10; i++ {
		rec(k, kaPolicy(), Key{Tenant: "t1",
			Session: "sess-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Duration(i).String()},
			clock.now().Add(time.Duration(i)*time.Second), kaBody, http.StatusOK, Usage{CacheRead: 1}, true)
	}
	if got := k.Stats().Live; got > MaxSessions {
		t.Fatalf("tracking %d sessions, bound is %d", got, MaxSessions)
	}
}

// evictLocked deletes the entry whose ping is least imminent, and that is the only removal
// path reaching it — so it must release the credential right there, or an evicted entry's
// Hold would never be matched by a Release.
func TestEvictionReleasesTheEvictedEntriesCredentials(t *testing.T) {
	k, fd, clock := testKeeper(t)
	pol := kaPolicy()
	var keys []Key
	for i := 0; i < MaxSessions+10; i++ {
		key := Key{Tenant: "t1", Session: "sess-" + strconv.Itoa(i)}
		keys = append(keys, key)
		at := clock.now().Add(time.Duration(i) * time.Second)
		// Two records per key: turn 0 is never pingable and never held; turn 1 is, and is
		// Held — the same pattern recordOne/rec use for every other pingable-entry test.
		rec(k, pol, key, at.Add(-time.Second), kaBody, http.StatusOK, Usage{CacheRead: 48576}, true)
		rec(k, pol, key, at, kaBody, http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	if got := k.Stats().Live; got != MaxSessions {
		t.Fatalf("tracking %d sessions, bound is %d", got, MaxSessions)
	}
	var survivors, evicted int
	for _, key := range keys {
		if _, ok := k.Entry(key); ok {
			survivors++
			if got := fd.holds(key); got != 1 {
				t.Errorf("%v: surviving entry held %d time(s), want 1", key, got)
			}
			continue
		}
		evicted++
		if got := fd.holds(key); got != 0 {
			t.Errorf("%v: evicted entry still held %d time(s)", key, got)
		}
	}
	if survivors != MaxSessions {
		t.Fatalf("survivors = %d, want %d", survivors, MaxSessions)
	}
	if evicted != 10 {
		t.Fatalf("evicted = %d, want 10", evicted)
	}
}

// A body too large to hold is refused rather than kept: one multi-million-token request must
// not spend a fifth of the whole memory budget.
func TestOversizedBodyIsNotHeld(t *testing.T) {
	k, _, clock := testKeeper(t)
	big := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", MaxBodyBytes) + `"}]}`
	recordOne(t, k, kaPolicy(), big, clock.now())
	if got := k.Stats().Live; got != 0 {
		t.Fatalf("held %d oversized sessions", got)
	}
}

// The kill switch, which is an environment variable precisely so it works when the control
// plane is what is broken.
func TestKillSwitch(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	k, _, clock := testKeeper(t)
	k.Start()
	defer k.Shutdown()
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	// The sweep loop never starts, so nothing is ever due. Sweep itself is still callable —
	// the switch stops the timer, and the test asserts the timer is what is stopped.
	select {
	case <-k.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep goroutine is running with the kill switch set")
	}
}

// waitPings waits for the sweep's fire goroutines to finish n pings. The sweep dispatches
// asynchronously in production (a ping is a round trip and must not be made under the map
// lock); these tests run it inline, so this returns at once — it stays so a test still reads
// correctly against the default runner.
func waitPings(t *testing.T, k *Keeper, n int64) {
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

func waitSkipped(t *testing.T, k *Keeper, n int64) {
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

// Deletion, token revocation and disablement each end an account's authority, and the keeper is
// a credential store the registry's cascade does not know about. tenant.TestDeleteCascadesEveryCredential
// asserts the invariant on the registry's own stores; this is the same invariant for this one.
func TestForgetReleasesOneTenantsMaterial(t *testing.T) {
	k, fd, clock := testKeeper(t)
	victim, bystander := Key{Tenant: "victim", Session: "s"}, Key{Tenant: "bystander", Session: "s"}
	for _, key := range []Key{victim, bystander} {
		for i := 0; i < 2; i++ {
			rec(k, kaPolicy(), key, clock.now().Add(time.Duration(i)*time.Second), kaBody,
				http.StatusOK, Usage{CacheRead: 48576}, true)
		}
	}
	if got := k.Stats().Live; got != 2 {
		t.Fatalf("expected two tenants held, got %d", got)
	}
	k.mu.Lock()
	body := k.live[victim.id()].body
	k.mu.Unlock()

	k.Forget("victim")

	if got := k.Stats().Live; got != 1 {
		t.Errorf("after forgetting one tenant, %d sessions held; want the bystander's only", got)
	}
	if _, ok := k.Entry(victim); ok {
		t.Error("the forgotten tenant is still held")
	}
	if _, ok := k.Entry(bystander); !ok {
		t.Error("forgetting one tenant released another tenant's material")
	}
	// The credential is released and the bytes are gone, not just the map entry.
	if got := fd.holds(victim); got != 0 {
		t.Errorf("the forgotten tenant's credential is still held (%d)", got)
	}
	if got := fd.holds(bystander); got != 1 {
		t.Errorf("forgetting one tenant released another's credential (%d held)", got)
	}
	for i := range body {
		if body[i] != 0 {
			t.Fatalf("the forgotten tenant's body still holds data at byte %d", i)
		}
	}
	// The turn counter goes too, or a re-registered id inherits its predecessor's history.
	k.mu.Lock()
	_, turns := k.turns[victim.id()]
	k.mu.Unlock()
	if turns {
		t.Error("the forgotten tenant's turn counter survived")
	}
}

// The retained body is masked for the same reason the credential is, and the asymmetry is what
// decides it: a leaked key is rotatable, a leaked transcript is not.
func TestRetainedBodyIsMaskedAtRest(t *testing.T) {
	const marker = "MY-PRIVATE-SOURCE-CODE-MARKER"
	k, fd, clock := testKeeper(t)
	body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+marker+`"`, 1)
	key := Key{Tenant: "t1", Session: "s"}
	for i := 0; i < 2; i++ {
		rec(k, kaPolicy(), key, clock.now().Add(time.Duration(i)*time.Second), body,
			http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	ev, ok := k.Entry(key)
	if !ok {
		t.Fatal("session not tracked")
	}
	held := ev.MaskedBody
	if bytes.Contains(held, []byte(marker)) {
		t.Error("the retained conversation is sitting in memory in plaintext")
	}
	if !bytes.Contains(unmasked(held), []byte(marker)) {
		t.Fatal("unmasking did not recover the body, so the ping would send the wrong bytes")
	}
	// And the ping still sends the REAL prefix — masking must not corrupt what goes on the wire,
	// or every ping writes a new entry at 12.5x instead of refreshing one at 0.1x.
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	fd.mu.Lock()
	sent := append([]byte(nil), fd.sent[0].Body...)
	fd.mu.Unlock()
	// BYTE-IDENTICAL, not merely marker-bearing: a single corrupted byte anywhere else in the
	// prefix still contains the marker, and that byte is a 1.25x write instead of a 0.1x
	// refresh. This is the assertion that actually guards the mask round trip.
	want, ok := pingBody([]byte(body), "/v1/messages")
	if !ok {
		t.Fatal("pingBody refused the fixture")
	}
	if !bytes.Equal(sent, want) {
		t.Errorf("the ping's bytes differ from the recorded prefix (%d sent vs %d expected)",
			len(sent), len(want))
	}
}

// A session armed on an account whose keep-alive is OFF still has a per-ping cost ceiling.
//
// This is the case the guard was written for and the one case it did not cover. The config
// builder hands the request path a ZERO Policy when nothing in the account's `cache:` block is
// switched on, `pingable()` read `MaxUSDPerPing <= 0` as "no cap", and the arm response
// reported `max_usd_per_ping: 0` — so the operator was told the ceiling was $0.00 while
// infinity was enforced, on the one path where per-ping cost is unbounded by anything else.
// (The override that switches such a policy on is the proxy's, and tested there.)
func TestAZeroCeilingPolicyStillHasAPingCeiling(t *testing.T) {
	// What a session override makes of a zero account policy: on, with no ceiling configured.
	pol := Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2}
	// It BITES: an entry whose projected ping cost is over the default is refused.
	over := &kaEntry{turn: 1, prefix: 1_000_000, pol: pol, pingUSD: DefaultMaxUSDPerPing + 0.01}
	if over.pingable() {
		t.Errorf("a ping projected at $%.2f passed the guard on an account with no configured "+
			"ceiling; the p99 ping is $0.2275 and the max $0.3780, which is what the guard is "+
			"for", over.pingUSD)
	}
	under := &kaEntry{turn: 1, prefix: 1_000_000, pol: pol, pingUSD: DefaultMaxUSDPerPing - 0.01}
	if !under.pingable() {
		t.Error("a ping inside the default ceiling was refused; the guard has become a block")
	}
	// A caller who really wants no ceiling has to type a negative number.
	none := &kaEntry{turn: 1, prefix: 1_000_000, pingUSD: 99,
		pol: Policy{KeepAlive: true, Idle: time.Second, MaxPings: 1, MaxUSDPerPing: -1}}
	if !none.pingable() {
		t.Error("an explicitly negative ceiling did not mean unlimited; that is the only way to " +
			"ask for it and it has to keep working")
	}
}

// With no PredictorID at all (every account today, and every strategy that predates the
// field), pingable() must behave EXACTLY as it did before the predictor gate existed —
// this is the regression the whole opt-in design rests on.
func TestPingableWithNoPredictorNamedIsUnchanged(t *testing.T) {
	base := Policy{KeepAlive: true, Idle: time.Second, MaxPings: 1}
	for _, reason := range []string{"tool_use", "end_turn", "stop", ""} {
		e := &kaEntry{turn: 1, prefix: 1_000_000, pol: base, stopReason: reason}
		if !e.pingable() {
			t.Errorf("stop_reason=%q with no PredictorID was refused; the gate must not run "+
				"at all when PredictorID is empty", reason)
		}
	}
}

// With PredictorID naming the shipped stop_reason gate, pingable() must ping ONLY on the
// actually-done cluster — the whole point of wiring docs/results/kv-ttl-predictor-arms.md's
// finding into production.
func TestPingableGatesOnTheNamedPredictor(t *testing.T) {
	gated := Policy{KeepAlive: true, Idle: time.Second, MaxPings: 1,
		PredictorID: "stop-reason-gated", PredictorThreshold: 0.5}
	cases := []struct {
		reason string
		want   bool
	}{
		{"tool_use", false}, {"stop_sequence", false}, {"stop", false}, {"", false},
		{"end_turn", true}, {"max_tokens", true},
	}
	for _, c := range cases {
		e := &kaEntry{turn: 1, prefix: 1_000_000, pol: gated, stopReason: c.reason}
		if got := e.pingable(); got != c.want {
			t.Errorf("stop_reason=%q: pingable() = %v, want %v", c.reason, got, c.want)
		}
	}
}

// An unknown predictor id must refuse to ping rather than silently letting the entry
// through unconditionally — the fail-toward-not-spending direction, since the money is
// what a broken or drifted predictor reference could otherwise waste.
func TestPingableRefusesAnUnknownPredictorID(t *testing.T) {
	e := &kaEntry{turn: 1, prefix: 1_000_000, stopReason: "end_turn",
		pol: Policy{KeepAlive: true, Idle: time.Second, MaxPings: 1,
			PredictorID: "not-a-real-predictor"}}
	if e.pingable() {
		t.Error("an unregistered predictor id let a ping through; it must refuse instead")
	}
}
