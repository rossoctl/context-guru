package keepalive

import (
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// Record reports the plan it built (#1198): the count, the floor of the wait and the spacing. A
// refused request leaves the plan unbuilt.
func TestRecordReportsThePlanInForce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       Key
		seq       []resp
		wantPings int
		wantFloor int
	}{
		{"main tool wait", sess1, []resp{toolCall, toolCall}, 8, DefaultToolWaitMinPrefix},
		{"main wait for a person", sess1, []resp{toolCall, endTurn}, 3, 80000},
		{"subagent wait", Key{Tenant: "t1", Session: "sess-1", Thread: "a:abc"}, []resp{toolCall, endTurn}, 2, DefaultToolWaitMinPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeperWith(t, nil)
			pol := planPolicy()
			pol.MinPrefixTokens = 80000 // the general floor, so the tool-wait floor (20k) differs from it
			var plan PlanInForce
			for i, r := range tc.seq {
				p := &PlanInForce{}
				k.Record(Record{Key: tc.key, StartedAt: clock.now().Add(-time.Duration(len(tc.seq)-1-i) * time.Second), Body: []byte(kaBody),
					Provider: bschemas.Anthropic, Route: "/v1/messages", Status: http.StatusOK,
					Usage:        Usage{CacheRead: 90000, CacheWrite: 1200, StopReason: r.stop},
					ToolUseNames: r.names, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(tc.key), Plan: p})
				plan = *p
			}
			if !plan.Built || plan.MaxPings != tc.wantPings || plan.MinPrefixTokens != tc.wantFloor ||
				plan.IdleSeconds != int(pol.Idle.Seconds()) {
				t.Fatalf("plan = %+v, want pings %d floor %d idle %d", plan, tc.wantPings, tc.wantFloor, int(pol.Idle.Seconds()))
			}
		})
	}
	t.Run("a first turn builds none", func(t *testing.T) {
		k, _, clock := testKeeperWith(t, nil)
		p := &PlanInForce{}
		k.Record(Record{Key: sess1, StartedAt: clock.now(), Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: toolCall.stop},
			ToolUseNames: toolCall.names, UsageOK: true, Policy: planPolicy(), Header: kaHeader(), Hold: fakeOf(k).hold(sess1), Plan: p})
		if p.Built {
			t.Fatalf("a first turn is refused by the gate but reported a plan: %+v", p)
		}
	})
	t.Run("a prefix below the floor builds none", func(t *testing.T) {
		k, _, clock := testKeeperWith(t, nil)
		pol := planPolicy()
		pol.MinPrefixTokens = 500000
		pol.ToolWaitMinPrefix = 500000
		p := &PlanInForce{}
		for i := 0; i < 2; i++ {
			p = &PlanInForce{}
			k.Record(Record{Key: sess1, StartedAt: clock.now().Add(-time.Duration(1-i) * time.Second), Body: []byte(kaBody), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: toolCall.stop},
				ToolUseNames: toolCall.names, UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(sess1), Plan: p})
		}
		if p.Built {
			t.Fatalf("a request below the prefix floor reported a plan: %+v", p)
		}
	})
	t.Run("keep-alive off builds none", func(t *testing.T) {
		k, _, clock := testKeeperWith(t, nil)
		p := &PlanInForce{}
		k.Record(Record{Key: sess1, StartedAt: clock.now(), Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: Policy{}, Header: kaHeader(), Plan: p})
		if p.Built {
			t.Fatalf("plan built with the policy off: %+v", p)
		}
	})
}
