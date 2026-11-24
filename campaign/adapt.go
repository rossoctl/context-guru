package campaign

import (
	"fmt"
	"slices"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/rossoctl/context-guru/waitcause"
)

// Tally is one strategy's replay over a set of rows.
type Tally struct {
	Net, Engine, Rescue, Ping float64
	Pings, Rescues            float64 // expected counts
}

// Plan is a strategy's decision on row i: k pings, x seconds apart, cancelled by SessionEnd or not.
type Plan func(i int) (k int, x float64, se bool)

// ScoreRows replays plan on the rows idx: the totals, and each row's net in gateway dollars (per) and in the builder's units (eng).
func ScoreRows(facts []RowFacts, idx []int, plan Plan, fp *FidParams) (t Tally, per, eng []float64) {
	per, eng = make([]float64, len(idx)), make([]float64, len(idx))
	for j, i := range idx {
		f := &facts[i]
		k, x, se := plan(i)
		rescue, ping, pings, rescues := f.Expected(k, x, se, fp)
		t.Pings += pings
		t.Ping += ping
		t.Rescues += rescues
		t.Rescue += rescue
		fired, rescued := f.Outcome(k, x, se)
		ge := -PingScale * float64(fired) * f.Ping
		if rescued {
			ge += BenefitScale * f.Stake
		}
		t.Net += rescue - ping
		t.Engine += ge
		per[j], eng[j] = rescue-ping, ge
	}
	return t, per, eng
}

// Active is a live campaign whose plan a replay follows.
type Active struct {
	ID   string
	Spec *predictive.Spec
}

// CurrentPlan replays the caller's own policy as configured now: its live campaign's plan where one is active (rules campaigns only), else the
// account's keep-alive with its floor, ceiling, first-request skip and predictor gate. SessionEnd counts only if this deployment honours the signal (se).
// A manager strategy (time windows) and per-session overrides are not replayed; a model campaign is reported as not replayable (ok=false).
// camp is the account's ACTIVE campaign, nil when it has none or its mode is not active.
func CurrentPlan(pol keepalive.Policy, se bool, camp *Active, facts []RowFacts) (plan Plan, label string, ok bool) {
	x := pol.Idle.Seconds()
	if x <= 0 {
		x = AutoX
	}
	if camp != nil && pol.KeepAlive {
		spec, id := camp.Spec, camp.ID
		if spec.Policy.Model != nil {
			return nil, "active campaign " + id + " runs a model; it cannot be replayed offline", false
		}
		return func(i int) (int, float64, bool) {
			ff := facts[i].Facts()
			k, _ := spec.PlanOf(spec.RuleFor(&ff))
			return k, x, se
		}, "active campaign " + id, true
	}
	if !pol.On() {
		return func(int) (int, float64, bool) { return 0, 0, false }, "keep-alive off", true
	}
	gate, _ := keepalive.PredictorFor(pol.PredictorID)
	return func(i int) (int, float64, bool) {
		f := &facts[i]
		if f.Turn < 1 || f.Prefix < int64(RowFloor(pol, f)) || f.Ping > pol.Ceiling() || (pol.PredictorID != "" && (gate == nil || gate(f.Stop) < pol.PredictorThreshold)) {
			return 0, x, se
		}
		return pol.MaxPings, x, se
	}, fmt.Sprintf("K%d every %.0f s, prefix >= %d tokens for a main thread that waits for a person (%d otherwise), <= $%.2f a ping", pol.MaxPings, x, pol.MinPrefixTokens, pol.WaitMinPrefix(waitcause.WaitingOwnTool, false), pol.Ceiling()), true
}

// RowFloor is the prefix floor the keeper would apply to a replayed row (Policy.WaitMinPrefix). A row
// has the stop reason, not the wait cause or the tool names: a tool stop is read as a tool wait, any
// other stop as a wait for a person (80k). A row that waited for background work or a teammate gets 80k
// here, where the keeper gives it 20k, so the replay under-counts those rows. A row carries no stored
// cause here, so every tool stop reads as a tool wait (20k). Dash's storedWait differs: it keeps a tool
// stop with a stored human cause as human. So a main-thread tool stop that asks a person reads as a tool
// wait here, and the replay can admit a few rows the keeper would refuse.
// An OpenAI row gets the general floor only: the keeper plans no wait for OpenAI.
//
// KNOWN GAP, kept on purpose: this gap is a behaviour change held for a human decision in #1298. Do not fix it here.
func RowFloor(pol keepalive.Policy, f *RowFacts) int {
	if f.OpenAI {
		return pol.MinPrefixTokens
	}
	cause := waitcause.WaitingHuman
	if slices.Contains(keepalive.ToolStopReasons, f.Stop) {
		cause = waitcause.WaitingOwnTool
	}
	return pol.WaitMinPrefix(cause, f.Subagent)
}

// ForcePolicy makes an "apply anyway" run the plan the user chose: a non-ship build's spec holds the fleet plan as its policy and the tenant's
// own tuned plan under own_policy; this swaps them in the spec document. false when the spec holds no own plan.
func ForcePolicy(spec map[string]any) bool {
	if spec == nil || spec["own_policy"] == nil {
		return false
	}
	spec["policy"], spec["forced"] = spec["own_policy"], true
	if ev, ok := spec["evidence"].(map[string]any); ok {
		if sh, ok := ev["shipped"].(map[string]any); ok {
			sh["plan"], sh["note"] = spec["own_plan"], "applied anyway: the tenant's own plan, although the held-out gate did not pass"
		}
	}
	return true
}
