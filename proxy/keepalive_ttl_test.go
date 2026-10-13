package proxy

import (
	"net/http"
	"net/http/httptest"
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
