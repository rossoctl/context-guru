package keepalive

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The manual-only hold: a request that asks for it (Policy.ManualHold, the host's reading of the
// x-context-guru-ping header) is kept for at most ManualHoldMax so the Ping button can work on an
// account whose automatic keep-alive is off. It sends no ping of its own.

var offPolicy = Policy{} // an account with no automatic keep-alive

func recordHold(k *Keeper, key Key, at time.Time, body string, pol Policy, hold bool) {
	pol.ManualHold = hold
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: key, StartedAt: at.Add(d), Body: []byte(body), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
	}
}

func stateOf(k *Keeper, key Key) PingState {
	return k.PingStates(key.Tenant, false, key.Session).For(key.Thread)
}

func TestManualHoldKeepsAThreadOnAnAccountWithKeepAliveOff(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, true)
	if _, ok := k.Entry(sess1); !ok {
		t.Fatal("a request asking for the hold was not held")
	}
	if st := stateOf(k, sess1); !st.Pingable || st.Reason != "" {
		t.Errorf("state = %+v, want pingable", st)
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatalf("PingNow on a manual-only hold: %v", err)
	}
	if fd.n() != 1 {
		t.Errorf("sent %d, want the one manual ping", fd.n())
	}
}

func TestWithoutTheHeaderNothingIsHeldAndTheReasonSaysSo(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, false)
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("a request that did not ask was held")
	}
	if fd.holds(sess1) != 0 {
		t.Error("a credential was held for a request that did not ask")
	}
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonNotOptedIn {
		t.Errorf("state = %+v, want not pingable, %q", st, ReasonNotOptedIn)
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("PingNow = %v, want ErrNotHeld", err)
	}
}

// The keeper never pings on its own for a manual-only hold: no sweep tick, at any time, sends one.
func TestManualHoldNeverSendsAnAutomaticPing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	for _, d := range []time.Duration{0, 100 * time.Second, 270 * time.Second, 280 * time.Second, 299 * time.Second, 301 * time.Second, time.Hour} {
		if n := k.Sweep(start.Add(d)); n != 0 {
			t.Fatalf("sweep at +%s fired %d pings", d, n)
		}
	}
	if fd.n() != 0 || k.Stats().Pings != 0 {
		t.Errorf("upstream saw %d requests, pings counted %d; want none", fd.n(), k.Stats().Pings)
	}
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	if k.PendingPings() != 0 || len(k.Plans("t1", false, sess1.Session)) != 0 {
		t.Error("a manual-only hold is reported as a planned ping (it would keep the idle-exit watchdog awake)")
	}
}

// Released at 300 s from the request start, and nothing of the request is left behind.
func TestManualHoldIsReleasedAndZeroisedAtThreeHundredSeconds(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	k.mu.Lock()
	e := k.live[sess1.id()]
	body := e.body // the keeper-owned masked copy
	k.mu.Unlock()
	if len(body) == 0 {
		t.Fatal("no body held")
	}
	k.Sweep(start.Add(299 * time.Second))
	if _, ok := k.Entry(sess1); !ok {
		t.Fatal("released before 300 s")
	}
	k.Sweep(start.Add(300 * time.Second))
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("still held at 300 s")
	}
	k.mu.Lock()
	live, bytes := len(k.live), k.bytes
	k.mu.Unlock()
	if live != 0 || bytes != 0 {
		t.Errorf("live %d, bytes %d after expiry; want 0/0", live, bytes)
	}
	for i, b := range body {
		if b != 0 {
			t.Fatalf("byte %d of the held body survived the release", i)
		}
	}
	if fd.holds(sess1) != 0 || len(fd.released) != 2 { // two records: the first was replaced by the second
		t.Errorf("credential holds %d, releases %d; want 0 held and every hold released", fd.holds(sess1), len(fd.released))
	}
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonExpired {
		t.Errorf("state = %+v, want not pingable, %q", st, ReasonExpired)
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("PingNow after expiry = %v, want ErrNotHeld", err)
	}
}

// Between the deadline and the next sweep tick the entry is still in the map; it must not be pingable.
func TestManualHoldIsNotPingableAfterItsDeadlineEvenBeforeTheSweep(t *testing.T) {
	k, _, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, true)
	clock.advance(301 * time.Second)
	if k.PingStates("t1", false, sess1.Session)[""].Pingable {
		t.Error("pingable past the deadline")
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("PingNow = %v, want ErrNotHeld", err)
	}
}

// A new REAL request restarts the 300 s. (A press does not: TestAPressNeverExtendsTheHold.)
func TestManualHoldIsRestartedByARequest(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	recordHold(k, sess1, start.Add(200*time.Second), kaBody, offPolicy, true)
	k.Sweep(start.Add(450 * time.Second))
	if _, ok := k.Entry(sess1); !ok {
		t.Fatal("a request at +200 s did not restart the hold")
	}
	k.Sweep(start.Add(500 * time.Second))
	if _, ok := k.Entry(sess1); ok {
		t.Error("held past 300 s from the last request")
	}
}

// The existing caps bind a manual-only hold like any other.
func TestManualHoldRespectsTheBodyEntryAndByteCaps(t *testing.T) {
	pad := func(n int) string {
		return strings.Replace(kaBody, `"text":"hi"`, `"text":"`+strings.Repeat("x", n)+`"`, 1)
	}

	k, _, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), pad(MaxBodyBytes), offPolicy, true) // over 8 MiB with the JSON around it
	if _, ok := k.Entry(sess1); ok {
		t.Error("a body over the per-body cap was held")
	}
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonTooLarge {
		t.Errorf("state = %+v, want %q", st, ReasonTooLarge)
	}

	k, _, clock = testKeeper(t) // entries
	for i := 0; i < MaxSessions+20; i++ {
		recordHold(k, Key{Tenant: "t1", Session: "s" + string(rune('A'+i%26)) + strings.Repeat("z", i/26)}, clock.now(), kaBody, offPolicy, true)
	}
	if n := k.Stats().Live; n > MaxSessions {
		t.Errorf("%d entries held, cap %d", n, MaxSessions)
	}

	k, _, clock = testKeeper(t) // bytes
	big := pad(MaxBodyBytes - 4096)
	for i := 0; i < 20; i++ {
		recordHold(k, Key{Tenant: "t1", Session: "big" + string(rune('a'+i))}, clock.now(), big, offPolicy, true)
	}
	k.mu.Lock()
	bytes := k.bytes
	k.mu.Unlock()
	if bytes > MaxBytes {
		t.Errorf("%d bytes held, cap %d", bytes, MaxBytes)
	}
}

// The per-ping ceiling decides before anything is kept.
func TestManualHoldRefusesAThreadWhosePingWouldBeOverTheCeiling(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-3, Output: 1e-3, CacheRead: 1e-3, CacheWrite: 1e-3}}})
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, true) // 49k tokens x $0.001 = $49 per ping
	if _, ok := k.Entry(sess1); ok {
		t.Error("held a thread whose ping is over the ceiling")
	}
	if st := stateOf(k, sess1); st.Reason != ReasonOverBudget {
		t.Errorf("reason = %q, want %q", st.Reason, ReasonOverBudget)
	}
}

func TestManualHoldRefusesWhatCannotBePingedAtAll(t *testing.T) {
	k, _, clock := testKeeper(t)
	thinking := strings.Replace(kaBody, `"stream":true,`, `"stream":true,"thinking":{"type":"enabled","budget_tokens":1000},`, 1)
	recordHold(k, sess1, clock.now(), thinking, offPolicy, true)
	if _, ok := k.Entry(sess1); ok {
		t.Error("held a thinking-enabled thread")
	}
	if st := stateOf(k, sess1); st.Reason != ReasonUnsupported {
		t.Errorf("reason = %q, want %q", st.Reason, ReasonUnsupported)
	}
}

// An account that HAS automatic keep-alive and sends the header keeps its normal hold. Only a thread
// the automatic gates refuse (here: under its minimum prefix) falls back to the manual-only hold.
func TestManualHoldOnAnAccountWithKeepAliveOn(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, kaPolicy(), true)
	if n := k.Sweep(start.Add(281 * time.Second)); n != 1 {
		t.Fatalf("the normal hold stopped pinging because the header was sent (%d)", n)
	}

	small := kaPolicy()
	small.MinPrefixTokens = 1 << 20 // the thread is below the automatic floor
	other := Key{Tenant: "t1", Session: "small"}
	recordHold(k, other, start, kaBody, small, true)
	if st := stateOf(k, other); !st.Pingable {
		t.Fatalf("a thread under the automatic floor was not held for the button: %+v", st)
	}
	// sess1 above is a normal hold and may ping again; what must never be pinged is the fallback.
	for _, d := range []time.Duration{281 * time.Second, 600 * time.Second, 900 * time.Second} {
		k.Sweep(start.Add(d))
	}
	for _, sent := range fd.sent {
		if sent.Key == other {
			t.Fatalf("the manual-only fallback was pinged automatically: %+v", sent.Key)
		}
	}
	k2, _, clock2 := testKeeper(t)
	recordHold(k2, other, clock2.now(), kaBody, small, false)
	if _, ok := k2.Entry(other); ok {
		t.Error("without the header the under-floor thread was held")
	}
}

// ---- the reasons on an account with automatic keep-alive ON (issue #662) ----

// Pings that ran out are not a failure. While the entry is still held and the cache can still be
// warm, a press is allowed (same ceiling and 30 s gap). After the keeper lets go, the reason says so.
func TestAPressIsAllowedOnAnEntryWhosePlannedPingsRanOut(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start)
	k.Sweep(start.Add(280 * time.Second))
	k.Sweep(start.Add(560 * time.Second)) // K = 2: both used
	clock.advance(570 * time.Second)
	if e, _ := k.Entry(sess1); e.Pings != 2 || e.Stopped {
		t.Fatalf("entry = %+v, want 2 pings used and not stopped", e)
	}
	if st := stateOf(k, sess1); !st.Pingable || st.Reason != "" {
		t.Fatalf("state = %+v, want pingable while it is still held and warm", st)
	}
	before := fd.n()
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatalf("press after the pings ran out: %v", err)
	}
	if fd.n() != before+1 {
		t.Errorf("sent %d, want exactly the press", fd.n()-before)
	}
	// Past its release the keeper holds nothing, and says why.
	k2, _, clock2 := testKeeper(t)
	s2 := clock2.now()
	recordOne(t, k2, kaPolicy(), kaBody, s2)
	k2.Sweep(s2.Add(280 * time.Second))
	k2.Sweep(s2.Add(560 * time.Second))
	k2.Sweep(s2.Add(900 * time.Second))
	if _, ok := k2.Entry(sess1); ok {
		t.Fatal("the spent entry was not released")
	}
	if st := stateOf(k2, sess1); st.Pingable || st.Reason != ReasonPingsUsed {
		t.Errorf("state = %+v, want not pingable, %q", st, ReasonPingsUsed)
	}
}

// A miss or a refused ping stops the entry, and a press stays refused.
func TestAnEntryStoppedByAFailureIsNotPingableAndSaysStopped(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil })
	k.Sweep(start.Add(280 * time.Second)) // the scheduled ping gets a 400 and stops the entry
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonStopped {
		t.Fatalf("state = %+v, want not pingable, %q", st, ReasonStopped)
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("press on a stopped entry = %v, want ErrNotHeld", err)
	}
	// And after the keeper releases it, still "stopped", not "not opted in".
	k.Sweep(start.Add(600 * time.Second))
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("the stopped entry was not released")
	}
	if st := stateOf(k, sess1); st.Reason != ReasonStopped {
		t.Errorf("after release: %+v, want %q", st, ReasonStopped)
	}
}

// Nothing known about a thread (a restart, or no request yet) is "no entry", never "not opted in".
func TestAThreadNothingIsKnownAboutSaysNoEntry(t *testing.T) {
	k, _, _ := testKeeper(t)
	if st := k.PingStates("t1", false, "never-seen").For(""); st.Pingable || st.Reason != ReasonNoEntry {
		t.Errorf("state = %+v, want %q", st, ReasonNoEntry)
	}
	if st := (PingStateMap(nil)).For("a:x"); st.Reason != ReasonNoEntry {
		t.Errorf("nil map: %+v, want %q", st, ReasonNoEntry)
	}
}

// Keep-alive ON, no header: the automatic gates refuse a thread. That is not "not opted in".
func TestGatedThreadsOnAnAccountWithKeepAliveOnSayWhy(t *testing.T) {
	k, fd, clock := testKeeper(t)
	small := kaPolicy()
	small.MinPrefixTokens = 1 << 20
	recordHold(k, sess1, clock.now(), kaBody, small, false)
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonBelowLimits {
		t.Errorf("under the floor, no header: %+v, want %q", st, ReasonBelowLimits)
	}

	k, fd, clock = testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-3, Output: 1e-3, CacheRead: 1e-3, CacheWrite: 1e-3}}})
	recordHold(k, sess1, clock.now(), kaBody, kaPolicy(), false)
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonOverBudget {
		t.Errorf("over the ceiling, no header: %+v, want %q", st, ReasonOverBudget)
	}
	recordHold(k, Key{Tenant: "t1", Session: "hdr"}, clock.now(), kaBody, kaPolicy(), true)
	if st := stateOf(k, Key{Tenant: "t1", Session: "hdr"}); st.Pingable || st.Reason != ReasonOverBudget {
		t.Errorf("over the ceiling, with the header: %+v, want %q", st, ReasonOverBudget)
	}

	// With the header, a thread under the floor IS held and pingable.
	k, _, clock = testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, small, true)
	if st := stateOf(k, sess1); !st.Pingable {
		t.Errorf("under the floor, with the header: %+v, want pingable", st)
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Errorf("press on a thread under the floor: %v", err)
	}
}

// The truthful "not opted in": no automatic policy and no header.
func TestNotOptedInIsOnlyForARequestThatCarriedNothing(t *testing.T) {
	k, _, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, false)
	if st := stateOf(k, sess1); st.Reason != ReasonNotOptedIn {
		t.Errorf("state = %+v, want %q", st, ReasonNotOptedIn)
	}
}

// ---- review-666: one tenant must not be able to starve the others ----

func tenantKey(tenant, session string) Key { return Key{Tenant: tenant, Session: session} }

// The caps are shared, and evictLocked drops the NEWEST entry. Manual-only holds are evicted before any
// automatic one, and one tenant can hold only so many of them.
func TestOneTenantsManualHoldsCannotStarveAnotherTenantsAutomaticHold(t *testing.T) {
	k, _, clock := testKeeper(t)
	at := clock.now()
	for i := 0; i < MaxSessions+50; i++ { // tenant A floods with the header, many session ids
		recordHold(k, tenantKey("A", "flood-"+strconv.Itoa(i)), at, kaBody, offPolicy, true)
	}
	k.mu.Lock()
	var aLive int
	for _, e := range k.live {
		if e.tenant == "A" {
			aLive++
		}
	}
	k.mu.Unlock()
	if aLive > ManualHoldMaxPerTenant {
		t.Errorf("tenant A holds %d manual-only entries, cap %d", aLive, ManualHoldMaxPerTenant)
	}
	// Tenant B's automatic keep-alive still works.
	b := tenantKey("B", "auto")
	recordHold(k, b, at, kaBody, kaPolicy(), false)
	if _, ok := k.Entry(b); !ok {
		t.Fatal("tenant B's automatic hold was refused after tenant A flooded")
	}
	if n := k.Sweep(at.Add(281 * time.Second)); n < 1 {
		t.Errorf("tenant B's automatic ping did not fire (%d)", n)
	}
}

// Even at the global cap, a NEW automatic hold displaces a manual-only one, not the other way round.
func TestManualOnlyEntriesAreEvictedBeforeAutomaticOnes(t *testing.T) {
	k, _, clock := testKeeper(t)
	at := clock.now()
	// Fill the global entry cap from many tenants, each within its own manual cap.
	for i := 0; i < MaxSessions; i++ {
		recordHold(k, tenantKey("m"+strconv.Itoa(i/ManualHoldMaxPerTenant), "s"+strconv.Itoa(i)), at, kaBody, offPolicy, true)
	}
	if k.Stats().Live != MaxSessions {
		t.Fatalf("setup: %d held, want %d", k.Stats().Live, MaxSessions)
	}
	auto := tenantKey("auto", "x")
	recordHold(k, auto, at.Add(time.Second), kaBody, kaPolicy(), false)
	if _, ok := k.Entry(auto); !ok {
		t.Fatal("an automatic hold was evicted on arrival while manual-only holds sat in the map")
	}
	if k.Stats().Live > MaxSessions {
		t.Errorf("%d held, cap %d", k.Stats().Live, MaxSessions)
	}
}

// The per-tenant byte cap, with the oldest manual hold going first.
func TestAManualFloodByBytesEvictsThatTenantsOldestHoldFirst(t *testing.T) {
	k, _, clock := testKeeper(t)
	at := clock.now()
	big := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+strings.Repeat("x", 6<<20)+`"`, 1)
	for i := 0; i < 8; i++ {
		recordHold(k, tenantKey("A", "big"+strconv.Itoa(i)), at.Add(time.Duration(i)*time.Second), big, offPolicy, true)
	}
	k.mu.Lock()
	var bytes int64
	for _, e := range k.live {
		if e.tenant == "A" && e.manualOnly {
			bytes += int64(len(e.body))
		}
	}
	k.mu.Unlock()
	if bytes > ManualHoldMaxBytesPerTenant {
		t.Errorf("tenant A holds %d manual-only bytes, cap %d", bytes, ManualHoldMaxBytesPerTenant)
	}
	if _, ok := k.Entry(tenantKey("A", "big0")); ok {
		t.Error("the OLDEST hold survived; the newest should")
	}
	if _, ok := k.Entry(tenantKey("A", "big7")); !ok {
		t.Error("the newest hold was evicted")
	}
}

// ---- the 300 s is measured from the last REAL request; a press never extends it ----

func TestAPressNeverExtendsTheHold(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	clock.advance(250 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatalf("press at 250 s: %v", err)
	}
	k.Sweep(start.Add(299 * time.Second))
	if _, ok := k.Entry(sess1); !ok {
		t.Fatal("released before 300 s")
	}
	k.Sweep(start.Add(300 * time.Second))
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("a press at 250 s extended the hold past 300 s from the last real request")
	}
	// Presses every 30 s cannot keep a credential held either.
	k2, _, clock2 := testKeeper(t)
	s2 := clock2.now()
	recordHold(k2, sess1, s2, kaBody, offPolicy, true)
	for i := 0; i < 12; i++ {
		clock2.advance(30 * time.Second)
		_, _ = k2.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second})
	}
	k2.Sweep(clock2.now())
	if _, ok := k2.Entry(sess1); ok {
		t.Error("presses every 30 s kept the credential held past 300 s")
	}
}

// A new REAL request does restart it.
func TestARealRequestRestartsTheHold(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recordHold(k, sess1, start, kaBody, offPolicy, true)
	recordHold(k, sess1, start.Add(200*time.Second), kaBody, offPolicy, true)
	k.Sweep(start.Add(450 * time.Second))
	if _, ok := k.Entry(sess1); !ok {
		t.Fatal("a real request at +200 s did not restart the hold")
	}
}

// The entry's own timer is what releases a quiet process, with no sweep and no request.
func TestTheManualHoldTimerReleasesAndZeroisesWithoutASweep(t *testing.T) {
	k, fd, clock := testKeeper(t)
	recordHold(k, sess1, clock.now(), kaBody, offPolicy, true)
	k.mu.Lock()
	e := k.live[sess1.id()]
	body := e.body
	k.mu.Unlock()
	clock.advance(ManualHoldMax + time.Second)
	k.expireManual(sess1.id()) // what the timer calls
	if _, ok := k.Entry(sess1); ok {
		t.Fatal("still held after the timer fired")
	}
	for i, b := range body {
		if b != 0 {
			t.Fatalf("byte %d survived the timer's release", i)
		}
	}
	if fd.holds(sess1) != 0 {
		t.Error("the credential was not released")
	}
	// And the real timer fires on its own (a short one, injected).
	k3, _, c3 := testKeeper(t)
	k3.manualHoldMax = 40 * time.Millisecond
	recordHold(k3, sess1, c3.now(), kaBody, offPolicy, true)
	c3.advance(time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		k3.mu.Lock()
		n := len(k3.live)
		k3.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the entry's own timer never released it")
}

// ---- nothing of a tenant survives Forget, and RetireSession clears its reasons ----

func TestForgetAndRetireSessionClearTheReasons(t *testing.T) {
	k, _, clock := testKeeper(t)
	recordHold(k, tenantKey("T", "s"), clock.now(), kaBody, offPolicy, false) // leaves "not opted in"
	recordHold(k, tenantKey("U", "s"), clock.now(), kaBody, offPolicy, false)
	k.mu.Lock()
	if len(k.reasons) != 2 {
		k.mu.Unlock()
		t.Fatalf("setup: %d reasons, want 2", len(k.reasons))
	}
	k.mu.Unlock()
	k.Forget("T")
	k.mu.Lock()
	n := len(k.reasons)
	k.mu.Unlock()
	if n != 1 {
		t.Errorf("after Forget(T) %d reasons remain, want only U's", n)
	}
	k.RetireSession("U", "s")
	k.mu.Lock()
	n = len(k.reasons)
	k.mu.Unlock()
	if n != 0 {
		t.Errorf("after RetireSession(U) %d reasons remain", n)
	}
}

// ---- every refusal path says why; an opted-in user is never told "not opted in" ----

func TestEveryRefusalOfAnOptedInRequestHasItsOwnReason(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(k *Keeper, fd *fakeDispatcher, clock *testClock)
		record func(k *Keeper, clock *testClock)
		want   string
	}{
		{"dead credential", func(k *Keeper, _ *fakeDispatcher, c *testClock) {
			k.mu.Lock()
			k.markCredDeadLocked("t1", "cred-x", "aws/claude-sonnet-5", 401, c.now())
			k.mu.Unlock()
		}, func(k *Keeper, c *testClock) {
			for _, d := range []time.Duration{-time.Second, 0} {
				k.Record(Record{Key: sess1, StartedAt: c.now().Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic, Route: "/v1/messages",
					Status: 200, Usage: Usage{CacheRead: 48576, CacheWrite: 1200}, UsageOK: true, Cred: "cred-x",
					Policy: Policy{ManualHold: true}, Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
			}
		}, ReasonUnavailable},
		{"no cache usage", nil, func(k *Keeper, c *testClock) {
			for _, d := range []time.Duration{-time.Second, 0} {
				k.Record(Record{Key: sess1, StartedAt: c.now().Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic, Route: "/v1/messages",
					Status: 200, Usage: Usage{FreshInput: 100}, UsageOK: true, Policy: Policy{ManualHold: true}, Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
			}
		}, ReasonNoEntry},
		{"client ended", func(k *Keeper, _ *fakeDispatcher, c *testClock) {
			k.MarkEnded("t1", sess1.Session, c.now().Add(time.Hour))
		}, func(k *Keeper, c *testClock) { recordHold(k, sess1, c.now(), kaBody, offPolicy, true) }, ReasonNoEntry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, fd, clock := testKeeper(t)
			// A stale reason from an earlier request must not survive either.
			k.mu.Lock()
			k.setReasonLocked(sess1, ReasonNotOptedIn)
			k.mu.Unlock()
			if tc.setup != nil {
				tc.setup(k, fd, clock)
			}
			tc.record(k, clock)
			if _, ok := k.Entry(sess1); ok {
				t.Fatal("the request was held")
			}
			if st := stateOf(k, sess1); st.Pingable || st.Reason != tc.want {
				t.Errorf("state = %+v, want not pingable, %q", st, tc.want)
			}
		})
	}
}
