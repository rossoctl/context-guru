package campaign

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/predictive"
)

func planSpec(t *testing.T, plan string) *predictive.Spec {
	t.Helper()
	fb := map[string]any{"kind": "plan", "plan": plan}
	if plan == "none" {
		fb = map[string]any{"kind": "none"}
	}
	spec := map[string]any{"schema": predictive.SpecSchema, "menu": map[string]any{"K2": map[string]any{"gaps_s": []float64{240, 240}}},
		"policy": map[string]any{"kind": "rules", "segments": []any{}, "fallback": fb}}
	b, _ := json.Marshal(spec)
	ps, err := predictive.ParseSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// A 400 s return after a 2 s request: the first ping (at 239 s) keeps the entry warm to the return, the second (at 479 s) never starts.
func TestScoreDecisionFollowsTheClosedForm(t *testing.T) {
	s := ScoreDecision(2, 240, 2, 400, true, math.Inf(1), 0.03, 0.3)
	if s.Fired != 1 || !s.Rescued {
		t.Fatalf("fired %d rescued %v, want 1 ping and a rescue", s.Fired, s.Rescued)
	}
	if want := PingScale * 0.03; math.Abs(s.PingUSD-want) > 1e-12 {
		t.Errorf("ping USD %v, want %v", s.PingUSD, want)
	}
	if want := BenefitScale*0.3 - PingScale*0.03; math.Abs(s.NetUSD-want) > 1e-12 || math.Abs(s.RescUSD-BenefitScale*0.3) > 1e-12 {
		t.Errorf("net %v rescue %v, want %v", s.NetUSD, s.RescUSD, want)
	}
	// a ping that would start at the very moment the conversation returns is never sent
	if n := ScoreDecision(1, 240, 2, 240-2+Late, true, math.Inf(1), 0.03, 0.3); n.Fired != 0 {
		t.Errorf("a ping starting at the return was sent: %+v", n)
	}
	// no return: every ping is wasted, nothing is rescued
	if n := ScoreDecision(2, 240, 2, 0, false, math.Inf(1), 0.03, 0.3); n.Fired != 2 || n.Rescued || n.NetUSD >= 0 {
		t.Errorf("no return: %+v", n)
	}
	// the client ends the session at once: no ping is sent
	if n := ScoreDecision(2, 240, 2, 0, false, 0, 0.03, 0.3); n.Fired != 0 || n.NetUSD != 0 {
		t.Errorf("session end: %+v", n)
	}
}

func TestPricedNeedsAWriteDearerThanARead(t *testing.T) {
	for _, c := range []struct {
		r, w float64
		want bool
	}{{3e-7, 3.75e-6, true}, {0, 3.75e-6, false}, {3e-7, 3e-7, false}, {3e-7, 1e-7, false}} {
		if got := Priced(c.r, c.w); got != c.want {
			t.Errorf("Priced(%v, %v) = %v", c.r, c.w, got)
		}
	}
	if got := Stake(1000, 1, 3); got != 2000 {
		t.Errorf("Stake = %v, want 2000", got)
	}
}

func TestChooseReadsThePlanRule(t *testing.T) {
	ps := planSpec(t, "K2")
	f := NewFacts("claude-sonnet-x", "end_turn", "claude-cli", 90000, 0.03, 7, 2)
	k, idle := Choose(ps, nil, nil, &f, predictive.Econ{})
	if k != 2 || idle != 240 {
		t.Errorf("plan K2: k %d idle %v, want 2 and 240", k, idle)
	}
	k, idle = Choose(planSpec(t, "none"), nil, nil, &f, predictive.Econ{})
	if k != 0 {
		t.Errorf("plan none: k %d idle %v, want 0", k, idle)
	}
}

func TestApplyWritesOnlyAnActiveDecision(t *testing.T) {
	pol := keepalive.Policy{KeepAlive: true, Idle: 60 * time.Second, MaxPings: 2, MinPrefixTokens: 80000, PredictorID: "x", PredictorThreshold: 0.5}
	if got := Apply(pol, 3, false); got != pol {
		t.Error("a campaign that is not active changed the policy")
	}
	if got := Apply(pol, 0, true); got.KeepAlive {
		t.Error("k=0 left keep-alive on")
	}
	got := Apply(pol, 3, true)
	if !got.KeepAlive || got.MaxPings != 3 || got.Idle != 60*time.Second || got.MinPrefixTokens != 0 || got.PredictorID != "" || got.PredictorThreshold != 0 {
		t.Errorf("active k=3: %+v", got)
	}
	if want := min(modelinfo.AnthropicDefaultKeepAliveMaxPings1h, 3); got.MaxPings1h != want {
		t.Errorf("the one-hour count %d, want the default capped by k = %d", got.MaxPings1h, want)
	}
	if got := Apply(keepalive.Policy{KeepAlive: true, MaxPings1h: 9}, 3, true); got.MaxPings1h != 3 || got.Idle != 240*time.Second {
		t.Errorf("one-hour count %d idle %v, want 3 and the shipped 240 s", got.MaxPings1h, got.Idle)
	}
}

func TestCurrentPlanReplaysTheAccountsPolicy(t *testing.T) {
	facts := []RowFacts{{Turn: 1, Prefix: 100000, Ping: 0.03, Stop: "end_turn"}, {Turn: 0, Prefix: 100000, Ping: 0.03}, {Turn: 1, Prefix: 50000, Ping: 0.03, Stop: "end_turn"}}
	pol := keepalive.Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2, MinPrefixTokens: 80000}
	plan, _, ok := CurrentPlan(pol, true, nil, facts)
	if !ok {
		t.Fatal("a plain policy must replay")
	}
	for i, want := range []int{2, 0, 0} { // an eligible row; a thread's first request; a prefix under the floor
		if k, x, se := plan(i); k != want || x != AutoX || !se {
			t.Errorf("row %d: k %d x %v se %v, want k %d", i, k, x, se, want)
		}
	}
	off, label, ok := CurrentPlan(keepalive.Policy{}, true, nil, facts)
	if k, _, _ := off(0); !ok || k != 0 || label != "keep-alive off" {
		t.Errorf("keep-alive off: k %d label %q ok %v", k, label, ok)
	}
	camp, label, ok := CurrentPlan(pol, false, &Active{ID: "c1", Spec: planSpec(t, "K2")}, facts)
	if k, _, se := camp(2); !ok || k != 2 || se || label != "active campaign c1" {
		t.Errorf("an active plan campaign: k %d se %v label %q ok %v", k, se, label, ok)
	}
	// a campaign applies only where the account's own keep-alive is on
	if k, _, _ := func() (int, float64, bool) {
		p, _, _ := CurrentPlan(keepalive.Policy{}, false, &Active{ID: "c1", Spec: planSpec(t, "K2")}, facts)
		return p(0)
	}(); k != 0 {
		t.Errorf("a campaign over a keep-alive that is off pinged %d times", k)
	}
}

func TestForcePolicySwapsTheOwnPlanIn(t *testing.T) {
	spec := map[string]any{"policy": "fleet", "own_policy": "mine", "own_plan": "K1",
		"evidence": map[string]any{"shipped": map[string]any{"plan": "K3", "note": "n"}}}
	if !ForcePolicy(spec) || spec["policy"] != "mine" || spec["forced"] != true {
		t.Fatalf("spec after force: %v", spec)
	}
	if sh := spec["evidence"].(map[string]any)["shipped"].(map[string]any); sh["plan"] != "K1" {
		t.Errorf("shipped plan %v, want the own plan K1", sh["plan"])
	}
	if ForcePolicy(map[string]any{"policy": "fleet"}) || ForcePolicy(nil) {
		t.Error("a spec with no own plan must not be forced")
	}
}
