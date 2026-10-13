package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
)

const (
	ttlBody1h = `{"model":"claude-sonnet-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	ttlBodyMixed = `{"model":"claude-sonnet-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
)

// recordUsage registers one finished request with an explicit usage, after a seeded first turn.
func recordUsage(t *testing.T, k *keeper, pol CachePolicy, body string, at time.Time, u Usage) {
	t.Helper()
	tn := &Tenancy{ID: "t1", Cache: pol}
	rec := func(at time.Time, u Usage) {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		k.record(tn, "sess-1", "", at, []byte(body), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, u, true)
	}
	rec(at.Add(-time.Second), u)
	rec(at, u)
}

func TestOneHourBilledSessionPingsOnceNear59Minutes(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	recordUsage(t, k, kaPolicy(), ttlBody1h, start, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200})

	for _, elapsed := range []time.Duration{270 * time.Second, 280 * time.Second, 30 * time.Minute, 3539 * time.Second} {
		if n := k.sweep(start.Add(elapsed)); n != 0 {
			t.Fatalf("1h session pinged after %s; the ping time is 59 min", elapsed)
		}
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (fired %d)", n)
	}
	if n := k.sweep(start.Add(59*time.Minute + 3540*time.Second)); n != 0 {
		t.Fatalf("second ping fired; the 1h schedule is 1 ping (%d)", n)
	}
}

func TestOneHourScheduleIsConfigurable(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	pol := kaPolicy()
	pol.Idle1h, pol.MaxPings1h = 50*time.Minute, 2
	recordUsage(t, k, pol, ttlBody1h, start, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200})
	if n := k.sweep(start.Add(50 * time.Minute)); n != 1 {
		t.Fatalf("first ping at 50 min: %d", n)
	}
	if n := k.sweep(start.Add(100 * time.Minute)); n != 1 {
		t.Fatalf("second ping at 100 min: %d", n)
	}
}

func TestFiveMinuteSessionKeepsTheFiveMinuteSchedule(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		u    Usage
	}{
		"no ttl asked":                 {kaBody, Usage{CacheRead: 48576, CacheWrite: 1200}},
		"1h asked, gateway downgraded": {ttlBody1h, Usage{CacheRead: 48576, CacheWrite: 1200}},
		"mixed ask, head 1h only":      {ttlBodyMixed, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 600}},
		"1h asked, nothing billed":     {ttlBody1h, Usage{CacheRead: 48576}},
	} {
		t.Run(name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			start := clock.now()
			recordUsage(t, k, kaPolicy(), tc.body, start, tc.u)
			if n := k.sweep(start.Add(279 * time.Second)); n != 0 {
				t.Fatalf("pinged early (%d)", n)
			}
			if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
				t.Fatalf("no ping at 280 s (%d)", n)
			}
			if n := k.sweep(start.Add(560 * time.Second)); n != 1 {
				t.Fatalf("no second ping at 560 s (%d)", n)
			}
		})
	}
}

func TestPureReadTurnInheritsTheBilledTier(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	pol := kaPolicy()
	recordUsage(t, k, pol, ttlBody1h, start.Add(-time.Minute), Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200})
	recordUsage(t, k, pol, ttlBody1h, start, Usage{CacheRead: 49776})
	if n := k.sweep(start.Add(300 * time.Second)); n != 0 {
		t.Fatalf("read-only turn on a 1h thread pinged at 5 min (%d)", n)
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (%d)", n)
	}
}

func TestTwoTiersOnOneProxy(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	rec := func(session, body string, u Usage) {
		for i := 0; i < 2; i++ {
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
			r.Header.Set("Authorization", "Bearer sk-caller-secret")
			k.record(tn, session, "", start, []byte(body), upstream{base: "http://up", path: "/v1/messages"},
				r, bschemas.Anthropic, "/v1/messages", http.StatusOK, u, true)
		}
	}
	rec("sub", ttlBody1h, Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200})
	rec("api", kaBody, Usage{CacheRead: 48576, CacheWrite: 1200})
	if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("at 280 s only the 5m session is due, fired %d", n)
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 2 {
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

// recordThread is recordUsage for an explicit session and thread, with its own seeded first turn.
func recordThread(t *testing.T, k *keeper, tn *Tenancy, session, thread, body string, at time.Time, u Usage) {
	t.Helper()
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		k.record(tn, session, thread, at, []byte(body), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, u, true)
	}
}

var (
	bill1h = Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}
	bill5m = Usage{CacheRead: 48576, CacheWrite: 1200}
	read   = Usage{CacheRead: 49776}
)

// A 1-hour thread followed by a read-only turn whose request is a MIXED ask. The read-only turn
// bills nothing, so only the request can tell: the tail now expires in five minutes.
// Mutation: delete the AllBreakpointsAskExtendedTTL check in resolveTier and this fails.
func TestMixedAskOnAReadOnlyTurnGoesBackToFiveMinutes(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	recordThread(t, k, tn, "sess-1", "", ttlBody1h, start.Add(-time.Minute), bill1h)
	recordThread(t, k, tn, "sess-1", "", ttlBodyMixed, start, read)
	if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("a mixed-ask read-only turn was not back on the 5-minute schedule (fired %d)", n)
	}
}

// The main thread is on the 1-hour tier and a subagent thread of the same session is on 5 minutes.
// Each follows its own schedule.
func TestMainThreadOneHourAndSubagentFiveMinutesInOneSession(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	recordThread(t, k, tn, "sess-1", "", ttlBody1h, start, bill1h)
	recordThread(t, k, tn, "sess-1", "a:sub1", kaBody, start, bill5m)
	if n := k.sweep(start.Add(280 * time.Second)); n != 1 {
		t.Fatalf("at 280 s only the subagent thread is due (fired %d)", n)
	}
	k.mu.Lock()
	main, sub := k.live[kaThreadKey("t1", "sess-1", "")], k.live[kaThreadKey("t1", "sess-1", "a:sub1")]
	k.mu.Unlock()
	if main == nil || sub == nil || !main.ttl1h || sub.ttl1h {
		t.Fatalf("tiers wrong: main=%+v sub=%+v", main, sub)
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 2 {
		t.Fatalf("at 59 min the main thread's one ping and the subagent's second are due (fired %d)", n)
	}
}

// `none` must stay none: no entry, no ping, however the thread was billed.
func TestNoneStaysNoneOnAOneHourThread(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	recordUsage(t, k, CachePolicy{}, ttlBody1h, start, bill1h)
	k.mu.Lock()
	held := len(k.live)
	k.mu.Unlock()
	if held != 0 {
		t.Fatalf("a keep-alive-off account holds %d entries for a 1h thread", held)
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 0 {
		t.Fatalf("pinged with keep-alive off (%d)", n)
	}
}

// An armed override enables a session on an account whose default is off. Its 60-290 s band cannot
// express an hour, so a 1-hour thread keeps the 1-hour schedule: one ping at 59 min, none at 270 s.
func TestOverrideOnAOneHourThreadUsesTheOneHourSchedule(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	armOn(t, k, "sess-1", 120*time.Second, 4, start.Add(4*time.Hour))
	tn := &Tenancy{ID: "t1", Cache: CachePolicy{}}
	recordThread(t, k, tn, "sess-1", "", ttlBody1h, start, bill1h)
	if n := k.sweep(start.Add(270 * time.Second)); n != 0 {
		t.Fatalf("a 1h thread was pinged on the override's 5-minute pair (%d)", n)
	}
	if n := k.sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (%d)", n)
	}
	if n := k.sweep(start.Add(2 * time.Hour)); n != 0 {
		t.Fatalf("a second ping: the override's 4 pings leaked into the 1h thread (%d)", n)
	}
}

// The credential hold, which is the number shown to the user.
func TestCredentialHoldPerTier(t *testing.T) {
	cases := []struct {
		name string
		pol  CachePolicy
		want time.Duration
	}{
		{"5m default", kaPolicy(), 3 * 280 * time.Second},
		{"1h default", kaPolicy().forOneHour(), 118 * time.Minute},
		{"1h at 2 pings", func() CachePolicy { p := kaPolicy(); p.MaxPings1h = 2; return p.forOneHour() }(), 177 * time.Minute},
		{"1h above the cap is clamped", func() CachePolicy { p := kaPolicy(); p.MaxPings1h = 100; return p.forOneHour() }(), maxKeepAliveHold1h},
	}
	for _, tc := range cases {
		e := &kaEntry{pol: tc.pol}
		if got := e.hold(); got != tc.want {
			t.Errorf("%s: hold %s, want %s", tc.name, got, tc.want)
		}
	}
	if maxKeepAliveHold1h != 236*time.Minute {
		t.Errorf("maxKeepAliveHold1h = %s, want 236m", maxKeepAliveHold1h)
	}
	// And a recorded 1h entry really carries that hold.
	k, _, clock := testKeeper(t, Limits{})
	recordUsage(t, k, kaPolicy(), ttlBody1h, clock.now(), bill1h)
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, e := range k.live {
		if e.hold() != 118*time.Minute {
			t.Errorf("recorded 1h entry holds %s, want 118m", e.hold())
		}
	}
}

// The tier a request carried survives the bound that resets the per-thread maps, and a request the
// gates refuse never enters the map at all.
func TestTierMemoryIsBoundedAndKeepsTheCurrentThread(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}

	// A thinking-enabled request is refused before turns++; it must not leave a tier behind.
	thinking := strings.Replace(ttlBody1h, `{"model"`, `{"thinking":{"type":"enabled","budget_tokens":1024},"model"`, 1)
	recordThread(t, k, tn, "think", "", thinking, start, bill1h)
	k.mu.Lock()
	grew := len(k.tier1h)
	k.mu.Unlock()
	if grew != 0 {
		t.Fatalf("a refused request wrote %d tier entries", grew)
	}

	once := func(session string) {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		k.record(tn, session, "", start, []byte(ttlBody1h), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, bill1h, true)
	}
	k.mu.Lock()
	for i := 0; i < maxKeepAliveTurnKeys-1; i++ {
		key := kaThreadKey("t1", "filler"+strconv.Itoa(i), "")
		k.turns[key] = 1
		k.tier1h[key] = true
	}
	k.mu.Unlock()
	once("fills-the-bound")
	once("trips-the-reset")
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.tier1h[kaThreadKey("t1", "trips-the-reset", "")] {
		t.Error("the reset dropped the tier of the thread that triggered it")
	}
	if len(k.tier1h) != 1 || len(k.turns) != 1 {
		t.Errorf("reset left turns=%d tier1h=%d, want 1 and 1", len(k.turns), len(k.tier1h))
	}
}
