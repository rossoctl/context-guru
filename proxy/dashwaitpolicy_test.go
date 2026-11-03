package proxy

import (
	"testing"

	"github.com/rossoctl/context-guru/tenant"
)

// The dashboard replays the policy that RUNS (#868). DashWaitPolicy says the plan is on only when the
// request path would apply it: an active campaign and a strategy in the 1-hour band turn it off.
func TestDashWaitPolicyReportsTheEffectivePlan(t *testing.T) {
	cache := CachePolicy{KeepAlive: true, WaitPlan: true, ToolWaitMaxPings: 6}
	setup := func(t *testing.T) (*keeper, func() CachePolicy) {
		k, _, _ := testKeeper(t, Limits{})
		k.h.keeper = k
		k.h.static.Store(&Tenancy{ID: "t1", Cache: cache})
		return k, func() CachePolicy { return k.h.DashWaitPolicy()("t1") }
	}

	k, get := setup(t)
	if p := get(); !p.WaitPlan || p.ToolWaitMaxPings != 6 {
		t.Errorf("plain account: %+v, want the plan on with its own counts", p)
	}

	// An active campaign owns the count: plan off.
	k, get = setup(t)
	rt, _ := campaignRuntime(t, planSpec, nil, campaignModeActive)
	k.pred = rt
	if p := get(); p.WaitPlan {
		t.Errorf("active campaign: plan still on")
	}
	// A shadow campaign decides nothing: plan on.
	k, get = setup(t)
	rt, _ = campaignRuntime(t, planSpec, nil, campaignModeShadow)
	k.pred = rt
	if p := get(); !p.WaitPlan {
		t.Errorf("shadow campaign: plan off")
	}

	// A strategy in the 1-hour band (Idle 300 s or more): plan off. A 5-minute strategy: plan on.
	k, get = setup(t)
	k.setStrategies([]tenant.Strategy{{ID: "s1", Active: true, IdleSeconds: 3540, MaxPings: 1, Target: tenant.Target{Mode: tenant.TargetAll}}})
	if p := get(); p.WaitPlan {
		t.Errorf("1-hour strategy: plan still on")
	}
	k, get = setup(t)
	k.setStrategies([]tenant.Strategy{{ID: "s2", Active: true, IdleSeconds: 240, MaxPings: 2, Target: tenant.Target{Mode: tenant.TargetAll}}})
	if p := get(); !p.WaitPlan {
		t.Errorf("5-minute strategy: plan off")
	}

	// The account switch.
	k, get = setup(t)
	cache.WaitPlan = false
	k.h.static.Store(&Tenancy{ID: "t1", Cache: cache})
	if p := get(); p.WaitPlan {
		t.Errorf("keepalive_wait_plan false: plan on")
	}
}
