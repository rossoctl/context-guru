package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/tenant"
)

// recordResp records two turns of "sess-1" on thread th for tenant t1, the second ending as u says.
func recordResp(t *testing.T, k *keeper, pol CachePolicy, th string, at time.Time, u Usage) {
	t.Helper()
	tn := &Tenancy{ID: "t1", Cache: pol}
	for i, ts := range []time.Time{at.Add(-time.Second), at} {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		use := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use", ToolUseNames: []string{"Bash"}}
		if i == 1 {
			use = u
			use.CacheRead, use.CacheWrite = 48576, 1200
		}
		k.record(tn, "sess-1", th, ts, []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, use, true)
	}
}

// #858: a strategy whose window is shut still pings a tool wait (any hour), and a wait for a person
// outside the window gets today's result: the account config, which here is off.
func TestStrategyWindowsApplyOnlyToAWaitForAPerson(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	closed := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 240, MaxPings: 4, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
		Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
		Target:  tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now,
	}
	k.setStrategies([]tenant.Strategy{closed})
	account := CachePolicy{WaitPlan: true} // keep-alive off on the account

	for _, tc := range []struct {
		name      string
		u         Usage
		wantHeld  bool
		wantPings int
	}{
		{"tool wait, outside the window", Usage{StopReason: "tool_use", ToolUseNames: []string{"Bash"}}, true, 8},
		{"background wait is not asked here: end_turn is a person", Usage{StopReason: "end_turn"}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k.Keeper.Forget("t1")
			recordResp(t, k, account, "", now, tc.u)
			v, ok := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"})
			if ok != tc.wantHeld {
				t.Fatalf("held = %v, want %v", ok, tc.wantHeld)
			}
			if ok {
				if v.Tag != "s" {
					t.Errorf("tag = %q, want the strategy", v.Tag)
				}
				if v.Policy.MaxPings != tc.wantPings {
					t.Errorf("pings = %d, want %d", v.Policy.MaxPings, tc.wantPings)
				}
			}
		})
	}

	// With the plan off, even a tool wait outside the window is not pinged (today).
	k.Keeper.Forget("t1")
	recordResp(t, k, CachePolicy{}, "", now, Usage{StopReason: "tool_use", ToolUseNames: []string{"Bash"}})
	if _, ok := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"}); ok {
		t.Fatal("with the plan off, a tool wait outside the window was held")
	}
}

// A subagent that waits for its main agent is not a wait for a person: the windows are ignored.
func TestStrategyWindowsIgnoredForASubagentEndTurn(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.setStrategies([]tenant.Strategy{{
		ID: "s", Active: true, IdleSeconds: 240, MaxPings: 2, MinPrefixTokens: 20000, MaxUSDPerPing: 0.5,
		Windows: []tenant.Window{{Start: "00:00", End: "00:01"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}})
	recordResp(t, k, CachePolicy{WaitPlan: true}, "a:sub1", now, Usage{StopReason: "end_turn"})
	v, ok := k.Keeper.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1", Thread: "a:sub1"})
	if !ok || v.Policy.MaxPings != 2 {
		t.Fatalf("subagent end_turn: held=%v policy=%+v, want held with 2 pings", ok, v.Policy)
	}
}

// The strategy picked ignoring windows follows the same priority rule, and no targeting strategy
// leaves the account config.
func TestBestStrategyIgnoringWindowsKeepsPriorityAndTarget(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	win := []tenant.Window{{Start: "00:00", End: "00:01"}}
	k.setStrategies([]tenant.Strategy{
		{ID: "all", Active: true, Windows: win, Target: tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now},
		{ID: "list", Active: true, Windows: win, Target: tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}}, UpdatedAt: now.Add(-time.Hour)},
		{ID: "off", Active: false, Windows: win, Target: tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t2"}}, UpdatedAt: now},
	})
	if b := k.bestStrategy("t1", now, true); b == nil || b.ID != "list" {
		t.Errorf("t1 got %+v, want list", b)
	}
	if b := k.bestStrategy("t2", now, true); b == nil || b.ID != "all" {
		t.Errorf("t2 got %+v, want all (the list strategy is inactive)", b)
	}
	if b := k.bestStrategy("t1", now, false); b != nil {
		t.Errorf("windows respected: got %+v, want none", b)
	}
	k.setStrategies(nil)
	if b := k.bestStrategy("t1", now, true); b != nil {
		t.Errorf("no strategies: got %+v", b)
	}
}
