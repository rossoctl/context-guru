package campaign

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/predictive"
)

// The "auto" builder picks the tenant's plan from the menu "ping k times, AutoX s apart" (k = 0..AutoK) with predictive.AutoTune, which shrinks
// the tenant's own evidence toward the fleet's. The window's rows of EVERY tenant are the fleet prior; the target tenant's last 30% is held out: the
// plan is tuned on rows resolved before that cut and replayed on the held-out rows against blanket K1..K3 and the fleet plan, with session-bootstrap
// intervals. The shipped plan is the same tuner refitted on the whole window. sessionEnd scores the rows as if the client's SessionEnd signal cancels
// the pings after a session's last response (the keeper does this when the client hook is installed).

// SegmentOf is the segment function of a segmentation name (cacheBuildIn.SegmentBy), nil when the name is unknown: a row's segment key and the
// runtime predicates that select the same requests live.
func SegmentOf(segBy string) func(f *RowFacts) (key string, when []any) { return segFeatures[segBy] }

// jsonNum is a tuner parameter on the wire: +Inf (the fleet plan / no decay) is null.
func jsonNum(v float64) any {
	if math.IsInf(v, 0) {
		return nil
	}
	return v
}

// segFeatures are what an auto campaign may be segmented by (cacheBuildIn.SegmentBy): a row's segment key, and the runtime predicates
// (predictive.Facts) that select the same requests live. One feature at a time: finer segments lose the most on held-out data.
var segFeatures = map[string]func(f *RowFacts) (key string, when []any){
	"dow": func(f *RowFacts) (string, []any) {
		return time.Weekday(f.Dow).String(), []any{[]any{"dow_utc", "==", float64(f.Dow)}}
	},
	"hour": func(f *RowFacts) (string, []any) {
		return fmt.Sprintf("%02d UTC", f.Hour), []any{[]any{"hour_utc", "==", float64(f.Hour)}}
	},
	"prefix": func(f *RowFacts) (string, []any) {
		for i, hi := range prefixEdges {
			if f.Prefix < hi {
				lo := 0.0
				if i > 0 {
					lo = float64(prefixEdges[i-1])
				}
				return prefixLabels[i], []any{[]any{"prefix_tok", ">=", lo}, []any{"prefix_tok", "<", float64(hi)}}
			}
		}
		return prefixLabels[len(prefixEdges)], []any{[]any{"prefix_tok", ">=", float64(prefixEdges[len(prefixEdges)-1])}}
	},
	"model":       func(f *RowFacts) (string, []any) { return f.Model, []any{[]any{"model", "==", f.Model}} },
	"client":      func(f *RowFacts) (string, []any) { return f.Agent, []any{[]any{"agent", "==", f.Agent}} },
	"stop_reason": func(f *RowFacts) (string, []any) { return f.Stop, []any{[]any{"stop_reason", "==", f.Stop}} },
}

var (
	prefixEdges  = []int64{20000, 80000, 200000}
	prefixLabels = []string{"<20K", "20K-80K", "80K-200K", ">=200K"}
)

// EvalResult is one build's held-out evaluation, shared by the builder (BuildSpec) and the host's adapt report.
type EvalResult struct {
	Insufficient string // why there was nothing to evaluate; "" when evaluated
	Cut          int64
	TenantRows   []int // indices of the tenant's rows
	Test         []int // indices of the tenant's held-out rows (ending at or after cut)
	NSessTest    int
	Scope        []string // the tuner scope of every row: tenant[\x1fsegment] for the tenant's rows when segmented
	Fit, Final   *predictive.AutoFit
}

// HeldPlan is the subject's plan on row i, tuned before the cut; InPlan is the plan refitted on the whole window (in-sample).
func (e *EvalResult) HeldPlan(i int) int { return e.Fit.PlanFor(e.Scope[i]) }
func (e *EvalResult) InPlan(i int) int   { return e.Final.PlanFor(e.Scope[i]) }

// Eval tunes the tenant's plan(s) on the fleet's rows resolved before the tenant's 70% point and refits on all of them. segBy "" is one plan
// for the tenant; otherwise one tuner scope per (tenant, segment), each shrunk toward the fleet.
func Eval(rows []predictive.AutoRow, facts []RowFacts, tenant string, until int64, segBy string) *EvalResult {
	e := &EvalResult{Scope: make([]string, len(rows))}
	seg := segFeatures[segBy]
	var ends []int64
	sess := map[string]bool{}
	for i, r := range rows {
		e.Scope[i] = r.Scope
		if r.Scope != tenant {
			continue
		}
		e.TenantRows = append(e.TenantRows, i)
		ends = append(ends, r.EndMs)
		if seg != nil {
			k, _ := seg(&facts[i])
			e.Scope[i] = tenant + "\x1f" + k
		}
	}
	tuned := rows
	if seg != nil {
		tuned = make([]predictive.AutoRow, len(rows))
		for i, r := range rows {
			r.Scope = e.Scope[i]
			tuned[i] = r
		}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	var train []predictive.AutoRow
	if len(ends) > 0 {
		e.Cut = ends[int(0.7*float64(len(ends)-1))]
		for _, r := range tuned {
			if float64(r.EndMs)+2700e3 <= float64(e.Cut) {
				train = append(train, r)
			}
		}
		for _, i := range e.TenantRows {
			if rows[i].EndMs >= e.Cut {
				e.Test = append(e.Test, i)
				sess[rows[i].Session] = true
			}
		}
	}
	e.NSessTest = len(sess)
	if len(e.Test) < AutoMinTst || e.NSessTest < AutoMinSes || len(train) == 0 {
		e.Insufficient = fmt.Sprintf("held-out rows %d (need %d), held-out sessions %d (need %d), training rows %d", len(e.Test), AutoMinTst, e.NSessTest, AutoMinSes, len(train))
		return e
	}
	e.Fit = predictive.AutoTune(train, e.Cut, predictive.AutoGrid{})
	e.Final = predictive.AutoTune(tuned, until, predictive.AutoGrid{})
	return e
}

// BuildSpec builds the campaign document for one tenant from the fleet's rows. It is the whole build; the host only does the I/O around it.
// facts (same order as rows, may be nil) add the shipped default to the gate and allow segBy.
func BuildSpec(rows []predictive.AutoRow, facts []RowFacts, tenant string, since, until int64, sessionEnd bool, segBy string) (spec map[string]any, rec string) {
	e := Eval(rows, facts, tenant, until, segBy)
	sess := map[string]bool{}
	for _, i := range e.TenantRows {
		sess[rows[i].Session] = true
	}
	spec = map[string]any{"schema": predictive.SpecSchema, "version": "2.0.0", "built_at": time.Now().UTC().Format(time.RFC3339),
		"builder": map[string]any{"engine": "auto", "backend": "closed-form", "code": "predictive/autotune.go"}, "scope": map[string]any{"type": "tenant", "id": tenant},
		"window": map[string]any{"start_ms": since, "end_ms": until, "n_decision_points": len(e.TenantRows), "n_sessions": len(sess)}, "objective": "net_usd",
		"session_end": sessionEnd, "menu": map[string]any{}}
	if segBy != "" {
		spec["segment_by"] = segBy
	}
	menu := spec["menu"].(map[string]any)
	for k := 1; k <= AutoK; k++ {
		gaps := make([]float64, k)
		for j := range gaps {
			gaps[j] = AutoX
		}
		menu[PlanName(k)] = map[string]any{"gaps_s": gaps, "max_pings": k, "tier": "5m"}
	}
	ruleOf := func(k int) map[string]any {
		if k > 0 {
			return map[string]any{"kind": "plan", "plan": PlanName(k)}
		}
		return map[string]any{"kind": "none"}
	}
	policy := func(k int) map[string]any {
		return map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "all", "when": []any{}, "rule": ruleOf(k)}}, "fallback": map[string]any{"kind": "none"}}
	}
	if e.Insufficient != "" {
		spec["policy"] = policy(0)
		spec["evidence"] = map[string]any{"recommendation": "insufficient_data", "reason": e.Insufficient}
		return spec, "insufficient_data"
	}
	test := e.Test
	ss := make([]string, len(test))
	for i, j := range test {
		ss[i] = rows[j].Session
	}
	// net returns each held-out row's net under a per-row plan.
	net := func(plan func(i int) int) []float64 {
		v := make([]float64, len(test))
		for t, i := range test {
			v[t] = rows[i].Gain[plan(i)]
		}
		return v
	}
	fixed := func(k int) func(int) int { return func(int) int { return k } }
	sum := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	}
	orc := 0.0
	for _, i := range test {
		b := 0.0
		for _, g := range rows[i].Gain {
			b = max(b, g)
		}
		orc += b
	}
	kAuto := e.Fit.PlanFor(tenant)
	subject := fmt.Sprintf("auto(%s)", PlanName(kAuto))
	if segBy != "" {
		subject = "auto(by " + segBy + ")"
	}
	type cand struct {
		name string
		v    []float64
		gate bool
	}
	cands := []cand{{subject, net(e.HeldPlan), false}, {"never", net(fixed(0)), false}, {"blanket-K1", net(fixed(1)), false}, {"blanket-K2", net(fixed(2)), false},
		{"blanket-K3", net(fixed(3)), true}, {fmt.Sprintf("fleet-plan(%s)", PlanName(e.Fit.FleetPlan)), net(fixed(e.Fit.FleetPlan)), true}}
	if facts != nil {
		v := make([]float64, len(test))
		for t, i := range test {
			v[t] = facts[i].Gain(facts[i].DefaultK(), AutoX, true, BenefitScale, PingScale)
		}
		cands = append(cands, cand{"default(K2+SessionEnd)", v, true})
	}
	r2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	// The gate is what the tenant would get anyway: the fleet plan, blanket K3 and the shipped default. A hindsight-best blanket plan on the
	// held-out rows is reported but is no gate: when the tuner picks a fixed plan it can at best tie it.
	var table, paired []any
	a0 := cands[0].v
	var defV []float64
	if facts != nil {
		defV = cands[len(cands)-1].v
	}
	ship, _ := ShipGate(a0, GateLegs(net(fixed(3)), net(fixed(e.Fit.FleetPlan)), defV, e.Fit.FleetPlan), ss)
	best, bestNet := "", math.Inf(-1)
	for i, c := range cands {
		n := sum(c.v)
		table = append(table, map[string]any{"strategy": c.name, "net_usd": r2(n), "ci95": SessionCI(c.v, ss, 0), "gap_captured": math.Round(n/max(orc, 1e-9)*1000) / 1000})
		if i == 0 {
			continue
		}
		a := make([]float64, len(a0))
		for j := range a {
			a[j] = a0[j] - c.v[j]
		}
		ci := SessionCI(a, ss, 1)
		paired = append(paired, map[string]any{"vs": c.name, "diff_usd": r2(sum(a)), "ci95": ci})
		if c.gate {
			if n > bestNet {
				best, bestNet = c.name, n
			}
		}
	}
	rec = "baseline"
	if ship {
		rec = "ship"
	}
	// own is the tenant's own tuned plan(s). It ships when the gate passes; otherwise the fleet plan runs, and own is kept so that an explicit
	// "apply anyway" runs the plan the user chose (forcePolicy), not the fleet plan.
	var own map[string]any
	ownName := PlanName(e.Final.PlanFor(tenant))
	if segBy == "" {
		own = policy(e.Final.PlanFor(tenant))
	} else {
		seen := map[string][]any{}
		for _, i := range e.TenantRows {
			k, when := SegmentOf(segBy)(&facts[i])
			seen[k] = when
		}
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var segs []any
		plans := map[string]string{}
		for _, k := range keys {
			p := e.Final.PlanFor(tenant + "\x1f" + k)
			plans[k] = PlanName(p)
			segs = append(segs, map[string]any{"id": segBy + ":" + k, "when": seen[k], "rule": ruleOf(p)})
		}
		own = map[string]any{"kind": "rules", "segments": segs, "fallback": ruleOf(e.Final.FleetPlan)}
		ownName = fmt.Sprintf("by %s: %v, otherwise %s", segBy, plans, PlanName(e.Final.FleetPlan))
	}
	spec["own_policy"], spec["own_plan"] = own, ownName
	shipped := PlanName(e.Final.FleetPlan)
	if rec == "ship" {
		spec["policy"], shipped = own, ownName
	} else {
		// the tenant's own plan passed no held-out check: run the fleet plan, which is what the tenant would get anyway
		spec["policy"] = policy(e.Final.FleetPlan)
	}
	spec["evidence"] = map[string]any{
		"klass":   "counterfactual: closed-form replay on this tenant's own history; oracle row = class oracle (hindsight best plan per decision, never deployable)",
		"split":   fmt.Sprintf("plan tuned on the fleet's rows resolved before %d (the tenant's 70%% point), scored on the tenant's last 30%%: %d decision points, %d sessions", e.Cut, len(test), e.NSessTest),
		"subject": subject, "table": table, "paired_vs_baselines": paired, "best_simple_baseline": best, "recommendation": rec, "oracle_net_usd": r2(orc),
		"tuner": map[string]any{"half_life_days": jsonNum(e.Fit.HalfLifeDays), "kappa": jsonNum(e.Fit.Kappa), "fleet_plan": PlanName(e.Fit.FleetPlan), "tenant_plan": PlanName(kAuto),
			"tenant_weight": r2(e.Fit.Weight[tenant])},
		"shipped": map[string]any{"plan": shipped, "half_life_days": jsonNum(e.Final.HalfLifeDays), "kappa": jsonNum(e.Final.Kappa), "fleet_plan": PlanName(e.Final.FleetPlan),
			"note": "ship: the same tuner refitted on the whole window (the table above is the held-out evidence for the procedure); otherwise the refitted fleet plan"},
		"engine": map[string]any{"benefit_scale": BenefitScale, "ping_scale": PingScale},
		"note":   "dollars are closed-form units calibrated to an offline replay engine; compare strategies, not absolute levels",
	}
	spec["caveats"] = []string{"the tenant's last 30% of rows is one held-out period; a tenant with few sessions gets wide intervals, see recommendation",
		"returns not seen before the window end count as no-return (pings wasted)", "closed-form replay: pings land on schedule and do not fail"}
	return spec, rec
}
