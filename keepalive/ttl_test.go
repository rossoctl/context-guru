package keepalive

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
)

const (
	ttlBody1h = `{"model":"aws/claude-sonnet-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	ttlBodyMixed = `{"model":"aws/claude-sonnet-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
)

var (
	bill1h = Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}
	bill5m = Usage{CacheRead: 48576, CacheWrite: 1200}
	read   = Usage{CacheRead: 49776}
)

// recTier records one finished request with an explicit usage, twice (the gate skips a thread's first
// request), on the given key.
func recTier(k *Keeper, pol Policy, key Key, at time.Time, body string, u Usage) {
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: key, StartedAt: at.Add(d), Body: []byte(body), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: u, UsageOK: true, Policy: pol,
			Header: kaHeader(), Hold: fakeOf(k).hold(key)})
	}
}

func TestOneHourBilledSessionPingsOnceNear59Minutes(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recTier(k, kaPolicy(), sess1, start, ttlBody1h, bill1h)
	for _, elapsed := range []time.Duration{270 * time.Second, 280 * time.Second, 30 * time.Minute, 3539 * time.Second} {
		if n := k.Sweep(start.Add(elapsed)); n != 0 {
			t.Fatalf("1h session pinged after %s; the ping time is 59 min", elapsed)
		}
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (fired %d)", n)
	}
	if n := k.Sweep(start.Add(59*time.Minute + 3540*time.Second)); n != 0 {
		t.Fatalf("second ping fired; the 1h schedule is 1 ping (%d)", n)
	}
}

func TestOneHourScheduleIsConfigurable(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	pol := kaPolicy()
	pol.Idle1h, pol.MaxPings1h = 50*time.Minute, 2
	recTier(k, pol, sess1, start, ttlBody1h, bill1h)
	if n := k.Sweep(start.Add(50 * time.Minute)); n != 1 {
		t.Fatalf("first ping at 50 min: %d", n)
	}
	if n := k.Sweep(start.Add(100 * time.Minute)); n != 1 {
		t.Fatalf("second ping at 100 min: %d", n)
	}
}

func TestFiveMinuteSessionKeepsTheFiveMinuteSchedule(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		u    Usage
	}{
		"no ttl asked":                 {kaBody, bill5m},
		"1h asked, gateway downgraded": {ttlBody1h, bill5m},
		"mixed ask, head 1h only":      {ttlBodyMixed, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 600}},
		"1h asked, nothing billed":     {ttlBody1h, Usage{CacheRead: 48576}},
	} {
		t.Run(name, func(t *testing.T) {
			k, _, clock := testKeeper(t)
			start := clock.now()
			recTier(k, kaPolicy(), sess1, start, tc.body, tc.u)
			if n := k.Sweep(start.Add(279 * time.Second)); n != 0 {
				t.Fatalf("pinged early (%d)", n)
			}
			if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
				t.Fatalf("no ping at 280 s (%d)", n)
			}
			if n := k.Sweep(start.Add(560 * time.Second)); n != 1 {
				t.Fatalf("no second ping at 560 s (%d)", n)
			}
		})
	}
}

func TestPureReadTurnInheritsTheBilledTier(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recTier(k, kaPolicy(), sess1, start.Add(-time.Minute), ttlBody1h, bill1h)
	recTier(k, kaPolicy(), sess1, start, ttlBody1h, read)
	if n := k.Sweep(start.Add(300 * time.Second)); n != 0 {
		t.Fatalf("read-only turn on a 1h thread pinged at 5 min (%d)", n)
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (%d)", n)
	}
}

func TestTwoTiersOnOneKeeper(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	sub, api := Key{Tenant: "t1", Session: "sub"}, Key{Tenant: "t1", Session: "api"}
	recTier(k, kaPolicy(), sub, start, ttlBody1h, bill1h)
	recTier(k, kaPolicy(), api, start, kaBody, bill5m)
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("at 280 s only the 5m session is due, fired %d", n)
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 2 {
		t.Fatalf("at 59 min the 1h session and the 5m session's second ping are due, fired %d", n)
	}
}

func TestAllBreakpointsAskExtendedTTL(t *testing.T) {
	if !apply.AllBreakpointsAskExtendedTTL([]byte(ttlBody1h)) {
		t.Fatal("all-1h body not detected")
	}
	for name, b := range map[string]string{"mixed": ttlBodyMixed, "none": `{"messages":[]}`, "5m": kaBody} {
		if apply.AllBreakpointsAskExtendedTTL([]byte(b)) {
			t.Fatalf("%s body reported as all-1h", name)
		}
	}
}

// A 1-hour thread followed by a read-only turn whose request is a MIXED ask. The read-only turn bills
// nothing, so only the request can tell: the tail now expires in five minutes.
func TestMixedAskOnAReadOnlyTurnGoesBackToFiveMinutes(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recTier(k, kaPolicy(), sess1, start.Add(-time.Minute), ttlBody1h, bill1h)
	recTier(k, kaPolicy(), sess1, start, ttlBodyMixed, read)
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("a mixed-ask read-only turn was not back on the 5-minute schedule (fired %d)", n)
	}
}

// The main thread is on the 1-hour tier and a subagent thread of the same session is on 5 minutes.
func TestMainThreadOneHourAndSubagentFiveMinutesInOneSession(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	sub := Key{Tenant: "t1", Session: "sess-1", Thread: "a:sub1"}
	recTier(k, kaPolicy(), sess1, start, ttlBody1h, bill1h)
	recTier(k, kaPolicy(), sub, start, kaBody, bill5m)
	if n := k.Sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("at 280 s only the subagent thread is due (fired %d)", n)
	}
	if e, _ := k.Entry(sess1); !e.TTL1h {
		t.Error("the main thread is not recorded as 1h")
	}
	if e, _ := k.Entry(sub); e.TTL1h {
		t.Error("the subagent thread is recorded as 1h")
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 2 {
		t.Fatalf("at 59 min the main thread's one ping and the subagent's second are due (fired %d)", n)
	}
}

// `none` must stay none: a manual-only hold on a 1h thread (the account's keep-alive is OFF, the
// request only asked for the Ping button) gets no schedule, no ping and no pending ping.
//
// The 1-hour swap cannot turn a parked policy on by itself: ForOneHour never sets KeepAlive, which
// On() requires. The test pins that, because a ForOneHour that ever did would silently start
// spending on every account that has keep-alive off.
func TestNoneStaysNoneOnAOneHourThread(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	parked := kaPolicy()
	parked.KeepAlive = false
	parked.ManualHold = true
	recTier(k, parked, sess1, start, ttlBody1h, bill1h)
	if e, ok := k.Entry(sess1); !ok || e.Policy.On() {
		t.Fatalf("a keep-alive-off 1h thread must be held manual-only with no schedule (held=%v)", ok)
	}
	for _, d := range []time.Duration{280 * time.Second, 59 * time.Minute, 2 * time.Hour} {
		if n := k.Sweep(start.Add(d)); n != 0 {
			t.Fatalf("pinged at %s with keep-alive off (%d)", d, n)
		}
	}
	if fd.n() != 0 {
		t.Fatalf("%d pings sent with keep-alive off", fd.n())
	}
	if k.PendingPings() != 0 {
		t.Error("a manual-only hold on a 1h thread counts as a pending ping")
	}
	if parked.ForOneHour().On() {
		t.Error("ForOneHour turned a keep-alive-off policy on")
	}
}

// The credential hold, which is the number shown to the user.
func TestCredentialHoldPerTier(t *testing.T) {
	two := kaPolicy()
	two.MaxPings1h = 2
	huge := kaPolicy()
	huge.MaxPings1h = 100
	for _, tc := range []struct {
		name string
		pol  Policy
		want time.Duration
	}{
		{"5m default", kaPolicy(), 3 * 280 * time.Second},
		{"1h default", kaPolicy().ForOneHour(), 118 * time.Minute},
		{"1h at 2 pings", two.ForOneHour(), 177 * time.Minute},
		{"1h above the cap is clamped", huge.ForOneHour(), MaxHold1h},
	} {
		if got := (&kaEntry{pol: tc.pol}).hold(); got != tc.want {
			t.Errorf("%s: hold %s, want %s", tc.name, got, tc.want)
		}
	}
	if MaxHold1h != 236*time.Minute {
		t.Errorf("MaxHold1h = %s, want 236m", MaxHold1h)
	}
}

// The tier a request carried survives the bound that resets the per-thread maps, and a request the
// gates refuse never enters the map at all.
func TestTierMemoryIsBoundedAndKeepsTheCurrentThread(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	thinking := strings.Replace(ttlBody1h, `{"model"`, `{"thinking":{"type":"enabled","budget_tokens":1024},"model"`, 1)
	recTier(k, kaPolicy(), Key{Tenant: "t1", Session: "think"}, start, thinking, bill1h)
	k.mu.Lock()
	grew := len(k.tier1h)
	k.mu.Unlock()
	if grew != 0 {
		t.Fatalf("a refused request wrote %d tier entries", grew)
	}
	k.mu.Lock()
	for i := 0; i < MaxTurnKeys-1; i++ {
		id := Key{Tenant: "t1", Session: "filler" + strconv.Itoa(i)}.id()
		k.turns[id] = 1
		k.tier1h[id] = true
	}
	k.mu.Unlock()
	one := func(session string) {
		k.Record(Record{Key: Key{Tenant: "t1", Session: session}, StartedAt: start, Body: []byte(ttlBody1h),
			Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK, Usage: bill1h,
			UsageOK: true, Policy: kaPolicy(), Header: kaHeader(), Hold: fakeOf(k).hold(Key{Tenant: "t1", Session: session})})
	}
	one("fills-the-bound")
	one("trips-the-reset")
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.tier1h[Key{Tenant: "t1", Session: "trips-the-reset"}.id()] {
		t.Error("the reset dropped the tier of the thread that triggered it")
	}
	if len(k.tier1h) != 1 || len(k.turns) != 1 {
		t.Errorf("reset left turns=%d tier1h=%d, want 1 and 1", len(k.turns), len(k.tier1h))
	}
}

// --- interactions with main's manual-ping hold (#666) and manual ping (#655) -------------------

// A manual-only hold is 300 s for a 5-minute thread and 59 minutes for a 1-hour one: capped to 300 s
// the button could never work on a 1-hour thread near the hour, which is when a press matters.
func TestManualHoldFollowsTheThreadsTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		u    Usage
		want time.Duration
	}{
		{"5-minute thread", kaBody, bill5m, ManualHoldMax},
		{"1-hour thread", ttlBody1h, bill1h, ManualHoldMax1h},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t)
			start := clock.now()
			recTier(k, offPolicyHold(), sess1, start, tc.body, tc.u)
			k.mu.Lock()
			e := k.live[sess1.id()]
			got := e.holdFor
			k.mu.Unlock()
			if e == nil || got != tc.want {
				t.Fatalf("manual hold = %v, want %v", got, tc.want)
			}
			k.Sweep(start.Add(tc.want - time.Second))
			if st := stateOf(k, sess1); !st.Pingable {
				t.Fatalf("not pingable just inside its hold: %+v", st)
			}
			k.Sweep(start.Add(tc.want + time.Second))
			if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonExpired {
				t.Fatalf("still pingable past its hold: %+v", st)
			}
		})
	}
}

func offPolicyHold() Policy { return Policy{ManualHold: true} }

// A manual press on a 1-hour thread works at 30 minutes (well past the 300 s a 5-minute hold would
// allow) and, being a press, does not extend the hold.
func TestManualPingOnAOneHourThreadWorksPastFiveMinutes(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recTier(k, offPolicyHold(), sess1, start, ttlBody1h, bill1h)
	clock.advance(30 * time.Minute)
	res, err := k.PingNow(ManualRequest{Key: sess1, RewriteUSD: 0.75})
	if err != nil {
		t.Fatalf("a press at 30 min on a 1h thread was refused: %v", err)
	}
	if res.AvoidedUSD != 0.75 || fd.n() != 1 {
		t.Fatalf("the press avoided $%.2f over %d pings, want $0.75 over 1", res.AvoidedUSD, fd.n())
	}
	k.Sweep(start.Add(ManualHoldMax1h + time.Second))
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err == nil {
		t.Fatal("a press extended the hold past 59 min from the last real request")
	}
}

// An automatic 1-hour thread plus a press: the press READS the cache, so it restarts the automatic
// clock from the press, not from the last real request.
func TestManualPingRestartsTheOneHourAutomaticClock(t *testing.T) {
	k, _, clock := testKeeper(t)
	start := clock.now()
	recTier(k, kaPolicy(), sess1, start, ttlBody1h, bill1h)
	clock.advance(30 * time.Minute)
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err != nil {
		t.Fatal(err)
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 0 {
		t.Fatalf("the scheduled ping fired at 59 min although a press refreshed the cache at 30 (%d)", n)
	}
	if n := k.Sweep(start.Add(30*time.Minute + 59*time.Minute)); n != 1 {
		t.Fatalf("no scheduled ping 59 min after the press (%d)", n)
	}
}
