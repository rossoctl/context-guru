package campaign

import (
	"fmt"
	"math"

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
		if f.Turn < 1 || f.Prefix < int64(RowFloor(pol, f)) || pol.PingVerdict(f.Ping, f.Miss, replayChain(pol, f)) != keepalive.VerdictOK || (pol.PredictorID != "" && (gate == nil || gate(f.Stop) < pol.PredictorThreshold)) {
			return 0, x, se
		}
		return pol.MaxPings, x, se
	}, fmt.Sprintf("K%d every %.0f s, prefix >= %d tokens for a main thread that waits for a person (%d otherwise), a ping chain that costs less than one miss%s", pol.MaxPings, x, pol.MinPrefixTokens, pol.WaitMinPrefix(waitcause.WaitingOwnTool, false), capNote(pol)), true
}

// RowFloor is the prefix floor the keeper would apply to a replayed row. It asks Policy.FloorFor, the
// one rule, with the row's stored wait cause and stop reason. A main-thread row with neither (an old row) gets the
// general floor. A tool stop stored as a human cause stays human, so a tool stop that asks a person reads
// as a person wait (80k), as the dashboard replay reads it; the keeper can read such a stop as a tool wait.
// An OpenAI row gets the general floor only: the keeper plans no wait for OpenAI.
func RowFloor(pol keepalive.Policy, f *RowFacts) int {
	if f.OpenAI {
		return pol.MinPrefixTokens
	}
	if f.Cause == "" && f.Subagent {
		// A row with no stored cause (stored before waiting_for existed), on a subagent thread. A subagent waits on the tool floor
		// whatever the cause, and RowFloor has always read it so. FloorFor would give the general floor.
		return pol.WaitMinPrefix(waitcause.WaitingHuman, true)
	}
	return pol.FloorFor(f.Cause, f.Stop, f.Subagent)
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

// capNote names the operator's own flat cap, when there is one.
func capNote(pol keepalive.Policy) string {
	if c := pol.PingCap(); !math.IsInf(c, 1) {
		return fmt.Sprintf(", <= $%.2f a ping", c)
	}
	return ""
}

// replayChain is the planned ping count the value rule weighs for a replayed row: the live keeper's
// wait-plan count when the row stores its wait cause, the base count otherwise (never more than live).
// The replay prices a miss at the 5-minute write rate, so on an entry billed at 1 hour it is less
// permissive than live, never more.
func replayChain(pol keepalive.Policy, f *RowFacts) int {
	if f.OpenAI {
		return pol.MaxPings
	}
	return max(pol.MaxPings, pol.WaitPings(f.Cause, f.Subagent))
}
