package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
)

// Per-session keep-alive overrides, and the seven hardening controls they must not weaken.
//
// The subject of nearly every test here is a REFUSAL. An override is an authorization to spend
// somebody's money on their own credential, so the interesting behaviour is not that it works,
// it is that it cannot be used to reach around the guard, the deadline, the kill switch or the
// audit sink.

// armOn installs a validated override on a keeper, failing the test if the bounds refuse it.
func armOn(t *testing.T, k *keeper, session string, idle time.Duration, pings int, until time.Time) sessionOverride {
	t.Helper()
	o, err := validOverride(session, idle, pings, 0, until, k.now(), "t1")
	if err != nil {
		t.Fatalf("validOverride: %v", err)
	}
	if err := k.arm("t1", session, o); err != nil {
		t.Fatalf("arm: %v", err)
	}
	return o
}

// The per-ping cost guard is the one control an override may not widen, because ping cost is
// bimodal (p50 $0.0004, p99 $0.2275, max $0.3780) — so the outlier worth refusing is one ping,
// and a caller who could raise the ceiling for "their" session could authorize the outlier.
func TestOverrideCannotRaiseThePerPingBudget(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	account := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.01, MinPrefixTokens: 20000}
	armOn(t, k, "sess-1", 120*time.Second, 4, clock.now().Add(time.Hour))
	got := k.overrideFor("t1", "sess-1", account)
	if got.MaxUSDPerPing != 0.01 {
		t.Errorf("the override moved the per-ping budget to %v; it is the account's own and not "+
			"negotiable per session", got.MaxUSDPerPing)
	}
	// The three fields it MAY move did move, or the override does nothing.
	if got.Idle != 120*time.Second || got.MaxPings != 4 {
		t.Errorf("the override did not take effect: %+v", got)
	}
	// And it may switch the mechanism ON for one session while the account default is off —
	// that is the point of it, and the arming request is the consent act.
	off := CachePolicy{Idle: 280 * time.Second, MaxPings: 2}
	if !k.overrideFor("t1", "sess-1", off).on() {
		t.Error("an override could not enable the keep-alive for one session on an account whose " +
			"default is off, which is the whole use for it")
	}
}

// The credential-hold ceiling that is ACTUALLY ENFORCED: 58 minutes, and it comes from the idle
// and ping caps rather than from maxOverrideHold.
//
// (K+1) x X is the hard retention deadline, so an override changes how long this service may hold
// somebody's key. The `maxOverrideHold = time.Hour` constant reads as an independent third rule
// and is not one: 12 x 290 s = 58 minutes is the most the other two caps allow, so the hour is
// unreachable and no override can be refused by it. This test used to claim to reach it — "12
// would be 62.7 minutes, so reach the ceiling through X" — and then could not, because X is capped
// too, ending up asserting hold() arithmetic on ACCEPTED overrides. It asserts the real ceiling
// now, in both directions.
func TestOverrideRespectsTheCredentialHoldCeiling(t *testing.T) {
	now := time.Now()
	// The maximum hold any override can buy: both caps at their limit.
	o, err := validOverride("s", maxOverrideIdle, maxOverridePings, 0, now.Add(time.Hour), now, "t1")
	if err != nil {
		t.Fatalf("the largest override the caps allow was refused: %v", err)
	}
	if got, want := o.hold(), 58*time.Minute; got != want {
		t.Errorf("the maximum credential hold is %v, want %v ((%d+1) x %v). If this moved, the "+
			"Settings copy and maxOverrideHold both need revisiting — the hour is unreachable "+
			"only because of these two caps", got, want, maxOverridePings, maxOverrideIdle)
	}
	if o.hold() > maxOverrideHold {
		t.Errorf("the caps now allow a hold of %v, past the operator ceiling of %v: the ceiling "+
			"has stopped being unreachable and needs its own refusal test", o.hold(), maxOverrideHold)
	}
	// One past each cap is refused, so the 58 minutes is a real bound and not a coincidence.
	if _, err := validOverride("s", maxOverrideIdle, maxOverridePings+1, 0, now.Add(time.Hour), now, "t1"); err == nil {
		t.Errorf("K = %d was accepted; the band is %d..%d", maxOverridePings+1,
			minOverridePings, maxOverridePings)
	}
	if _, err := validOverride("s", maxOverrideIdle+time.Second, maxOverridePings, 0, now.Add(time.Hour), now, "t1"); err == nil {
		t.Errorf("X = %v was accepted; past %v the first ping lands after the lifetime has lapsed",
			maxOverrideIdle+time.Second, maxOverrideIdle)
	}
	// And the message has to name the number the person is authorizing.
	if _, err := validOverride("s", 280*time.Second, 11, 0, now.Add(time.Hour), now, "t1"); err != nil {
		t.Errorf("(11+1) x 280 = 56 minutes of hold was refused: %v", err)
	}
	// The refusal for a genuine over-hour hold states the computed hold in minutes.
	for _, tc := range []struct {
		idle  time.Duration
		pings int
	}{{290 * time.Second, 11}} {
		o, err := validOverride("s", tc.idle, tc.pings, 0, now.Add(time.Hour), now, "t1")
		if err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
		if got, want := o.hold(), time.Duration(tc.pings+1)*tc.idle; got != want {
			t.Errorf("hold = %v, want (K+1) x X = %v", got, want)
		}
	}
	// The interval band, and the reason for its upper end: past 290 s the first ping lands
	// after a 300 s lifetime has lapsed and pays a WRITE.
	for _, bad := range []time.Duration{0, 59 * time.Second, 291 * time.Second, time.Hour} {
		if _, err := validOverride("s", bad, 2, 0, now.Add(time.Hour), now, "t1"); err == nil {
			t.Errorf("idle %v was accepted", bad)
		}
	}
	// An ACCEPTED override's entry gets the deadline its own policy implies, not the account's.
	k, _, clock := testKeeper(t, Limits{})
	armOn(t, k, "sess-1", 60*time.Second, 3, clock.now().Add(time.Hour))
	recordOne(t, k, CachePolicy{}, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.mu.Lock()
	e := k.live[kaKey("t1", "sess-1")]
	k.mu.Unlock()
	if e == nil {
		t.Fatal("an armed session was not tracked, so its deadline cannot be checked")
	}
	if e.pol.Idle != 60*time.Second || e.pol.MaxPings != 3 {
		t.Errorf("the entry carries %v/%d, not the override's 60s/3", e.pol.Idle, e.pol.MaxPings)
	}
	if e.timer == nil {
		t.Error("an armed session was retained with no hard retention deadline")
	}
}

// An expiry is an expiry: past `until` the account's own policy resolves again, pinging stops,
// and the map does not keep the dead entry.
func TestOverrideExpiresAndStopsPinging(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	off := CachePolicy{Idle: 280 * time.Second, MaxPings: 2} // KeepAlive false: account is OFF
	armOn(t, k, "sess-1", 60*time.Second, 3, clock.now().Add(10*time.Minute))
	if !k.overrideFor("t1", "sess-1", off).on() {
		t.Fatal("the override is not in force, so its expiry proves nothing")
	}
	clock.advance(11 * time.Minute)
	if k.overrideFor("t1", "sess-1", off).on() {
		t.Error("an expired override still enabled the keep-alive")
	}
	// And it is not retained. overrideFor treats it as absent regardless; the sweep deletes it,
	// so the map cannot grow on the strength of authorizations nobody withdrew.
	k.sweep(clock.now())
	k.mu.Lock()
	_, still := k.overrides[kaKey("t1", "sess-1")]
	k.mu.Unlock()
	if still {
		t.Error("an expired override was retained in the map")
	}
}

// Arming a session that never sends another request must cost NOTHING: no entry, no held body,
// no held credential, no ping. An override is POLICY, not state — which is what makes a stale
// override on a dead session harmless, and it is why per-session control is shippable at all.
func TestOverrideOnADeadSessionCostsNothing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	armOn(t, k, "ghost", 60*time.Second, 4, clock.now().Add(6*time.Hour))
	// No record() call at all: the session never sends anything again.
	for i := 0; i < 10; i++ {
		k.sweep(clock.advance(70 * time.Second))
	}
	if got := k.Stats().Live; got != 0 {
		t.Errorf("%d entries live for a session that sent nothing", got)
	}
	if got := fs.n(); got != 0 {
		t.Errorf("%d pings sent to a session that sent nothing", got)
	}
	if k.bytes != 0 {
		t.Errorf("%d bytes held for a session that sent nothing", k.bytes)
	}
}

// Disarming releases the held material NOW rather than at the hard deadline, and what it
// releases is zeroized rather than merely dropped — this extends the no-output-surface bar to
// the disarm path, at debug level, because that is the realistic leak path.
func TestDisarmRetiresTheHeldMaterialNow(t *testing.T) {
	const cred = "sk-caller-DISARM-DO-NOT-LEAK"
	const secret = "MY-PRIVATE-SOURCE-CODE-DISARM"
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	sink, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: sink})
	defer h.Close()
	k := h.keeper
	clock := &testClock{at: time.Now()}
	k.now = clock.now
	fs := &fakeSender{}
	k.send = fs.send

	body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+secret+`"`, 1)
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+cred)
	armOn(t, k, "armed", 60*time.Second, 3, clock.now().Add(time.Hour))
	for i := 0; i < 2; i++ {
		k.record(tn, "armed", clock.now().Add(time.Duration(i)*time.Second), []byte(body),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic,
			"/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}
	k.mu.Lock()
	e := k.live[kaKey("t1", "armed")]
	k.mu.Unlock()
	if e == nil {
		t.Fatal("nothing retained, so nothing is being released")
	}
	held := e.body // the keeper's own copy; clear() overwrites the array in place
	if len(held) == 0 {
		t.Fatal("the entry holds no body")
	}
	if !k.disarm("t1", "armed") {
		t.Fatal("disarm reported nothing was armed")
	}
	if got := k.Stats().Live; got != 0 {
		t.Errorf("%d entries still live after a disarm; the release must not wait for the "+
			"hard deadline", got)
	}
	// The BYTES are gone, not merely unreferenced: a dropped slice sits in the heap until a
	// collection that may never come, and a dump taken in between still yields the transcript.
	if strings.Contains(string(held), secret) {
		t.Error("the held body was dropped rather than zeroized")
	}
	for _, b := range held {
		if b != 0 {
			t.Error("the held body was not fully zeroized")
			break
		}
	}
	if e.timer != nil {
		t.Error("the hard retention deadline was left running after a disarm")
	}
	// And nothing leaked on the way out, at debug level.
	surfaces := map[string]string{
		"log sink (debug)": logs.String(),
		"entry %+v":        fmt.Sprintf("%+v", *e),
		"KeepAliveStats":   fmt.Sprintf("%+v", k.Stats()),
	}
	for name, out := range surfaces {
		for _, marker := range []string{cred, secret} {
			if strings.Contains(out, marker) {
				t.Errorf("%s leaked %q:\n%s", name, marker, out)
			}
		}
	}
	// A second disarm is a no-op rather than a panic: every refusal path calls it unconditionally.
	if k.disarm("t1", "armed") {
		t.Error("a second disarm reported something was armed")
	}
}

// The two GLOBAL refusals win over an override, because they are the reasons retention is
// defensible at all: the operator's kill switch, and a deployment with no audit sink.
func TestArmingIsRefusedWithoutAnAuditSinkOrWithTheKillSwitchOn(t *testing.T) {
	now := time.Now()
	o, err := validOverride("s", 280*time.Second, 2, 0, now.Add(time.Hour), now, "t1")
	if err != nil {
		t.Fatal(err)
	}
	// No audit sink.
	k := newKeeper(&Handler{opts: Options{}, limiter: NewLimiter(Limits{})})
	k.now = func() time.Time { return now }
	if err := k.arm("t1", "s", o); err == nil {
		t.Error("a deployment with no audit sink accepted an arm; the whole credential " +
			"arrangement rests on being reviewable")
	}
	// Kill switch. It stops RETENTION, not merely pinging, so it must also stop the thing that
	// authorizes retention.
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	k2, _, clock := testKeeper(t, Limits{})
	if err := k2.arm("t1", "s", o); err == nil {
		t.Error("the operator kill switch did not refuse an arm")
	}
	// And an armed session already in the map does not survive the switch: record retires it.
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "")
	armOn(t, k2, "s", 60*time.Second, 2, clock.now().Add(time.Hour))
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	recordOne(t, k2, CachePolicy{}, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if got := k2.Stats().Live; got != 0 {
		t.Errorf("%d entries retained for an armed session with the kill switch on", got)
	}
}

// A session id whose `tenant:uuid` prefix names SOMEBODY ELSE is REFUSED, and if one ever got
// past that it would still address nothing: the keeper keys on the AUTHENTICATED principal, never
// on a value out of the body.
//
// Two layers, and both are the test. The refusal is the fix for a real path — a manager's
// service-wide session list is entirely other tenants' rows, so arming one was a click away, and
// it returned a cheerful 200 having kept nothing warm. The isolation underneath it is defence in
// depth and stays asserted directly against the keeper, because it is what makes the refusal a
// usability fix rather than the only thing standing between two accounts.
func TestOverrideForAnotherTenantsSessionIdIsRefusedAndWouldAddressNothing(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	victim := "t-victim:9f3a-1c2b"
	now := clock.now()
	if _, err := validOverride(victim, 60*time.Second, 4, 0, now.Add(time.Hour), now, "t1"); err == nil {
		t.Error("arming a session id that names another account was accepted; it can only ever " +
			"be a no-op, and the arm response would report a 0-token prefix and no price")
	}
	// The caller's OWN prefix is of course fine, as is a bare id with no tenant prefix at all.
	for _, ok := range []string{"t1:9f3a-1c2b", "9f3a-1c2b"} {
		if _, err := validOverride(ok, 60*time.Second, 4, 0, now.Add(time.Hour), now, "t1"); err != nil {
			t.Errorf("session id %q was refused for the principal that owns it: %v", ok, err)
		}
	}
	// Defence in depth: installed directly, past the check, it is still inert for the victim.
	o, err := validOverride("t1:9f3a-1c2b", 60*time.Second, 4, 0, now.Add(time.Hour), now, "t1")
	if err != nil {
		t.Fatal(err)
	}
	o.pol.Idle = 60 * time.Second
	if err := k.arm("t1", victim, o); err != nil {
		t.Fatal(err)
	}
	// The victim's own traffic is unaffected: resolution is under THEIR tenant id.
	off := CachePolicy{Idle: 280 * time.Second, MaxPings: 2}
	if k.overrideFor("t-victim", victim, off).on() {
		t.Error("an arm under one principal reached another tenant's session")
	}
	// It landed under the attacker's own key, where it is inert.
	if !k.overrideFor("t1", victim, off).on() {
		t.Error("the arm did not land under the authenticated principal at all")
	}
	// The armed list is scoped too: the victim sees nothing.
	if got := k.armedFor("t-victim"); len(got) != 0 {
		t.Errorf("the victim's armed list shows %d entries from another principal's arm", len(got))
	}
	if got := k.armedFor("t1"); len(got) != 1 {
		t.Errorf("the arming principal's own list shows %d entries, want 1", len(got))
	}
	// And a session id that is not one is refused before it becomes a map key.
	for _, bad := range []string{"", strings.Repeat("x", maxSessionIDBytes+1), "a\x00b"} {
		if _, err := validOverride(bad, 280*time.Second, 2, 0, now.Add(time.Hour), now, "t1"); err == nil {
			t.Errorf("session id %q was accepted", bad)
		}
	}
}

// The expiry is MANDATORY and it is capped at 12 hours, because session lifetime p99 is 12.9 h:
// past that the authorization is being spent on a session that statistically no longer exists.
func TestOverrideExpiryIsMandatoryAndCapped(t *testing.T) {
	now := time.Now()
	for name, until := range map[string]time.Time{
		"absent":      {},
		"in the past": now.Add(-time.Minute),
		"now":         now,
		"beyond 12h":  now.Add(13 * time.Hour),
	} {
		if _, err := validOverride("s", 280*time.Second, 2, 0, until, now, "t1"); err == nil {
			t.Errorf("an expiry that is %s was accepted", name)
		}
	}
	if _, err := validOverride("s", 280*time.Second, 2, 0, now.Add(12*time.Hour), now, "t1"); err != nil {
		t.Errorf("exactly 12 hours was refused: %v", err)
	}
}

// The map bounds, both of them, and neither is a silent clamp.
func TestOverrideCountBoundsAreRefusalsNotClamps(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	until := clock.now().Add(time.Hour)
	for i := 0; i < maxOverridesPerTenant; i++ {
		armOn(t, k, fmt.Sprintf("s-%d", i), 280*time.Second, 2, until)
	}
	o, err := validOverride("one-too-many", 280*time.Second, 2, 0, until, clock.now(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.arm("t1", "one-too-many", o); err == nil {
		t.Errorf("a %dth override for one account was accepted", maxOverridesPerTenant+1)
	}
	// Replacing an EXISTING one is still allowed at the bound — otherwise a full list could not
	// be edited.
	if err := k.arm("t1", "s-0", o); err != nil {
		t.Errorf("re-arming an existing session at the bound was refused: %v", err)
	}
	// A different account is unaffected by this one's bound.
	if err := k.arm("t2", "theirs", o); err != nil {
		t.Errorf("one account's bound refused another account: %v", err)
	}
}

// A Settings save calls keeper.forget, and that must kill armed sessions too: unticking the
// account-wide box is a consent withdrawal, and leaving per-session authorizations running would
// be the setting not working.
func TestForgettingATenantDisarmsItsSessions(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	armOn(t, k, "mine", 280*time.Second, 2, clock.now().Add(time.Hour))
	if err := k.arm("t2", "theirs", func() sessionOverride {
		o, err := validOverride("theirs", 280*time.Second, 2, 0, clock.now().Add(time.Hour),
			clock.now(), "t2")
		if err != nil {
			t.Fatal(err)
		}
		return o
	}()); err != nil {
		t.Fatal(err)
	}
	k.forget("t1")
	if got := len(k.armedFor("t1")); got != 0 {
		t.Errorf("%d overrides survived a consent withdrawal", got)
	}
	if got := len(k.armedFor("t2")); got != 1 {
		t.Errorf("forgetting one tenant disarmed another's %d sessions", 1-got)
	}
}

// The armed list is the LIVE map and it says on the wire that it is not durable. An override that
// silently survived a restart would be worse than one that does not; a client that renders it as
// durable is misrepresenting an authorization to spend.
func TestArmedSessionsAreNotDurableAndSayItOnTheWire(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	w, _ := f.do(t, http.MethodGet, "/api/me/keepalive/sessions", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/me/keepalive/sessions = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"durable":false`) {
		t.Errorf("the armed list does not state that it is lost on restart: %s", w.Body)
	}
	// Stop() drops them with everything else: the process they authorized is ending.
	k, _, clock := testKeeper(t, Limits{})
	armOn(t, k, "s", 280*time.Second, 2, clock.now().Add(time.Hour))
	k.Stop()
	if got := len(k.armedFor("t1")); got != 0 {
		t.Errorf("%d overrides survived Stop()", got)
	}
}

// A session armed on an account whose keep-alive is OFF still has a per-ping cost ceiling.
//
// This is the case the guard was written for and the one case it did not cover. cachePolicy()
// hands the request path a ZERO CachePolicy when nothing in the account's `cache:` block is
// switched on, `pingable()` read `MaxUSDPerPing <= 0` as "no cap", and the arm response
// reported `max_usd_per_ping: 0` — so the operator was told the ceiling was $0.00 while
// infinity was enforced, on the one path where per-ping cost is unbounded by anything else.
func TestAnOverrideOnAKeepAliveOffAccountStillHasAPingCeiling(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	armOn(t, k, "sess-1", 280*time.Second, 2, clock.now().Add(time.Hour))
	// The account default: keep-alive off, nothing configured — a zero policy.
	pol := k.overrideFor("t1", "sess-1", CachePolicy{})
	if !pol.on() {
		t.Fatal("the override did not enable the mechanism; the rest of this test is vacuous")
	}
	if pol.Ceiling() != DefaultMaxUSDPerPing {
		t.Errorf("Ceiling() = %v on an unconfigured policy, want the default %v — 0 means "+
			"nobody configured a ceiling, and in a spend guard that may not mean infinity",
			pol.Ceiling(), DefaultMaxUSDPerPing)
	}
	// And it BITES: an entry whose projected ping cost is over the default is refused.
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
		pol: CachePolicy{KeepAlive: true, Idle: time.Second, MaxPings: 1, MaxUSDPerPing: -1}}
	if !none.pingable() {
		t.Error("an explicitly negative ceiling did not mean unlimited; that is the only way to " +
			"ask for it and it has to keep working")
	}
}

// worst_case_pings is a CEILING, so it counts one ping per idle interval and not K per span.
//
// A span ends the instant a real request arrives. The worst case is therefore not a session
// that goes quiet — it is one whose requests land just after each ping, restarting the clock
// every time. `until/((K+1)X) x K` under-states that by (K+1)/K: 8 against 12 at the shipped
// defaults, 2x at K=1. It is labelled CEILING in the arm dialog somebody reads before
// authorizing a spend.
func TestWorstCasePingsIsAnActualCeiling(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	o, err := validOverride("s", 280*time.Second, 2, 0, now.Add(time.Hour), now, "t1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, pings := h.worstCase("t1", "s", o)
	// 3600/280 = 12.85 -> 12. The span form gives 3600/840 = 4 spans x 2 = 8.
	if pings != 12 {
		t.Errorf("worst_case_pings = %d over an hour at X=280s, want 12 (3600/280). 8 is the "+
			"(K+1)X-span form, which under-states the ceiling by (K+1)/K", pings)
	}
	// K=1 is where the span form is wrong by 2x, so it is worth pinning too.
	o1, err := validOverride("s", 280*time.Second, 1, 0, now.Add(time.Hour), now, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, p1 := h.worstCase("t1", "s", o1); p1 != 12 {
		t.Errorf("worst_case_pings = %d at K=1, want 12: the ceiling is set by the idle interval, "+
			"not by K", p1)
	}
}

// The default this package enforces is the one the configuration loader documents.
//
// proxy may not import config, so the constant is duplicated, and a duplicated money default
// that drifts is worse than no default at all: the account document would say one thing and the
// request path enforce another.
func TestTheProxysPingCeilingDefaultMatchesTheConfigLoaders(t *testing.T) {
	if DefaultMaxUSDPerPing != config.DefaultKeepAliveMaxUSDPerPing {
		t.Errorf("proxy.DefaultMaxUSDPerPing = %v, config.DefaultKeepAliveMaxUSDPerPing = %v; "+
			"an unconfigured account would be told one ceiling and have another enforced",
			DefaultMaxUSDPerPing, config.DefaultKeepAliveMaxUSDPerPing)
	}
}
