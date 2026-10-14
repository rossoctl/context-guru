package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// The "auto" builder: POST /api/cache/campaigns/build with "builder":"auto" picks the tenant's plan from the menu "ping k times, 270 s apart"
// (k = 0..autoK) with predictive.AutoTune, which shrinks the tenant's own evidence toward the fleet's. It runs in-process: no model, no Python.
//
// The window's rows of EVERY tenant are the fleet prior; the target tenant's last 30% is held out: the plan is tuned on rows resolved before that cut
// and replayed on the held-out rows against blanket K1..K3 and the fleet plan, with session-bootstrap intervals. The shipped plan is the same tuner refitted
// on the whole window. "session_end": true scores the rows as if the client's SessionEnd signal cancels the pings after a session's last response
// (the keeper does this when the client hook is installed); the default scores without it.

const (
	autoK = 6
	// autoX is the menu spacing. 270 s, not 280: live, pings 275-280 s after a response end miss the provider's cache 19% of the time,
	// and at 270 s the replayed result no longer depends on where the cliff is (simfid's live A/B and cliff sensitivity).
	autoX    = campaignKeepAlive5mIdle // = config.DefaultKeepAliveIdle (270 s); apply() also caps it at the tenant's own interval
	autoBoot = 1000
)

// The evidence floors of a build: held-out rows and sessions (the same floors as scripts/campaign/build_campaign.py). Variables only so tests can
// run the whole build on a small fixture.
var autoMinTst, autoMinSes = 500, 20

// planOutcome is the closed-form replay of one decision: ping j starts at j*x - u + late after the response end and fires if the conversation has not
// returned (and the client has not ended the session) by then; the entry survives to the return if the last touch is within the cache lifetime of it.
func planOutcome(k int, x, u, gap float64, returned bool, stop float64) (fired int, rescued bool) {
	if !returned {
		gap = math.Inf(1)
	}
	touch := 0.0
	for j := 1; j <= k; j++ {
		s := float64(j)*x - u + cfLate
		if s >= gap || s >= stop {
			break
		}
		fired++
		touch = s + cfPingDur
	}
	return fired, k > 0 && returned && gap-touch < cfTC && !(gap < cfTC)
}

// autoRows turns the dashboard dataset into tuner rows. A row whose outcome is not known by untilMs (its response ended less than the last ping plus the
// cache lifetime before it) is dropped, and so is one with no price or incomplete accounting.
func autoRows(reqs []*kvcache.Request, price func(model string) (pIn, pOut, pRead, pWrite float64, ok bool), sessionEnd bool, untilMs int64) []predictive.AutoRow {
	rows, _ := autoRowsX(reqs, price, sessionEnd, untilMs)
	return rows
}

// rowFacts is what a decision point carries besides its tuner row: its timing (to replay any plan, not only the menu), its dollars at the gateway's
// rates, and the facts a campaign segment tests.
type rowFacts struct {
	u, gap             float64 // request duration and the gap from its response end to the next request, seconds
	returned           bool
	toEnd              float64 // seconds from this response's end to the end of the session's last response (any model): SessionEnd cancels pings after it
	stake, ping        float64 // one avoided rewrite (prefix x (write - read)) and one ping, gateway $
	turn               int     // requests of the conversation before this one
	prefix             int64
	hour, dow          int // request start, UTC
	model, agent, stop string
	pRead, pWrite      float64 // gateway rates of the row's model
	nextPrefix         int64   // the next request's cached prefix (0 = none or unknown)
	span               float64 // seconds from the response end to the next request of the same session thread (any model): the keeper's entry is per thread, so its pings stop then
}

// outcome replays plan (k pings x s apart) on the row; sessionEnd stops the pings after the session's last response.
func (f *rowFacts) outcome(k int, x float64, sessionEnd bool) (fired int, rescued bool) {
	stop := math.Inf(1)
	if sessionEnd {
		stop = f.toEnd
	}
	return planOutcome(k, x, f.u, f.gap, f.returned, stop)
}

// The shipped default keep-alive (config.Default*): K2 at 270 s on a prefix of at least 80K tokens, at most $0.50 a ping, never after a thread's first
// request, with SessionEnd.
func (f *rowFacts) defaultK() int {
	if f.turn < 1 || f.prefix < campaignDefaultMinPrefixTokens || f.ping > DefaultMaxUSDPerPing {
		return 0
	}
	return campaignDefaultMaxPings
}

// autoRowsX is autoRows plus each row's facts (same order). Gains are in the builder's units (calibrated to the pinned replay engine).
func autoRowsX(reqs []*kvcache.Request, price func(model string) (pIn, pOut, pRead, pWrite float64, ok bool), sessionEnd bool, untilMs int64) ([]predictive.AutoRow, []rowFacts) {
	type sk struct{ user, conv string }
	lastEnd := map[sk]int64{}
	byThr := map[sk][]int64{} // request starts per thread of a session: the keeper's turns and entries are per thread
	for _, r := range reqs {
		if r.KeepAlive {
			continue
		}
		k := sk{r.User, r.ConversationID}
		lastEnd[k] = max(lastEnd[k], r.TS+int64(r.UpstreamMs))
		t := sk{r.User, r.ConversationID + "\x00" + r.ThreadID} // the keeper counts turns per thread
		byThr[t] = append(byThr[t], r.TS)
	}
	for _, ts := range byThr {
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	}
	byID := make(map[int64]*kvcache.Request, len(reqs))
	for _, r := range reqs {
		byID[r.ID] = r
	}
	out := make([]predictive.AutoRow, 0, len(reqs))
	facts := make([]rowFacts, 0, len(reqs))
	for _, r := range reqs {
		end := r.TS + int64(r.UpstreamMs)
		if r.KeepAlive || r.Rejected || !r.CostKnown || r.CachedContext <= 0 || float64(end)+2700e3 > float64(untilMs) {
			continue
		}
		pIn, pOut, pRead, pWrite, ok := price(r.Model)
		if !ok || pWrite <= pRead {
			continue
		}
		key, tk := sk{r.User, r.ConversationID}, sk{r.User, r.ConversationID + "\x00" + r.ThreadID}
		st := time.UnixMilli(r.TS).UTC()
		f := rowFacts{u: r.UpstreamMs / 1000, returned: r.HasNext, toEnd: float64(max(0, lastEnd[key]-end)) / 1000,
			stake: float64(r.CachedContext) * (pWrite - pRead), ping: (&predictive.Response{Prefix: r.CachedContext, PIn: pIn, POut: pOut, PRead: pRead}).PingCost(),
			turn: sort.Search(len(byThr[tk]), func(i int) bool { return byThr[tk][i] >= r.TS }), prefix: r.CachedContext,
			hour: st.Hour(), dow: int(st.Weekday()), model: r.Model, agent: r.Agent, stop: r.StopReason, pRead: pRead, pWrite: pWrite}
		f.span = math.Inf(1)
		if ss := byThr[tk]; true {
			if j := sort.Search(len(ss), func(i int) bool { return ss[i] > r.TS }); j < len(ss) {
				f.span = float64(ss[j]-end) / 1000
			}
		}
		if r.HasNext {
			f.gap = float64(r.NextTS-end) / 1000
			if n := byID[r.NextID]; n != nil {
				f.nextPrefix = n.CachedContext
			}
		}
		g := make([]float64, autoK+1)
		for k := 1; k <= autoK; k++ {
			g[k] = f.gain(k, autoX, sessionEnd, cfScale, cfPingScale)
		}
		out = append(out, predictive.AutoRow{Scope: r.User, Session: r.ConversationID, EndMs: end, Gain: g})
		facts = append(facts, f)
	}
	return out, facts
}

// gain is the net of plan (k, x) on the row: bScale x the avoided rewrite, minus pScale x the pings. Scales 1 = gateway dollars.
func (f *rowFacts) gain(k int, x float64, sessionEnd bool, bScale, pScale float64) float64 {
	fired, rescued := f.outcome(k, x, sessionEnd)
	g := -pScale * float64(fired) * f.ping
	if rescued {
		g += bScale * f.stake
	}
	return g
}

// sessionCI is the 95% interval of sum(v) by bootstrap over sessions.
func sessionCI(v []float64, sess []string, seed int64) [2]float64 {
	idx := map[string]int{}
	var tot []float64
	for i, s := range sess {
		j, ok := idx[s]
		if !ok {
			j = len(tot)
			idx[s] = j
			tot = append(tot, 0)
		}
		tot[j] += v[i]
	}
	if len(tot) == 0 {
		return [2]float64{}
	}
	rng := rand.New(rand.NewSource(seed))
	b := make([]float64, autoBoot)
	for i := range b {
		for range tot {
			b[i] += tot[rng.Intn(len(tot))]
		}
	}
	sort.Float64s(b)
	r2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	return [2]float64{r2(b[autoBoot*25/1000]), r2(b[autoBoot*975/1000-1])}
}

func planName(k int) string {
	if k == 0 {
		return "none"
	}
	return fmt.Sprintf("K%d", k)
}

// jsonNum is a tuner parameter on the wire: +Inf (the fleet plan / no decay) is null.
func jsonNum(v float64) any {
	if math.IsInf(v, 0) {
		return nil
	}
	return v
}

// segFeatures are what an auto campaign may be segmented by (cacheBuildIn.SegmentBy): a row's segment key, and the runtime predicates
// (predictive.Facts) that select the same requests live. One feature at a time: finer segments lose the most on held-out data.
var segFeatures = map[string]func(f *rowFacts) (key string, when []any){
	"dow": func(f *rowFacts) (string, []any) {
		return time.Weekday(f.dow).String(), []any{[]any{"dow_utc", "==", float64(f.dow)}}
	},
	"hour": func(f *rowFacts) (string, []any) {
		return fmt.Sprintf("%02d UTC", f.hour), []any{[]any{"hour_utc", "==", float64(f.hour)}}
	},
	"prefix": func(f *rowFacts) (string, []any) {
		for i, hi := range prefixEdges {
			if f.prefix < hi {
				lo := 0.0
				if i > 0 {
					lo = float64(prefixEdges[i-1])
				}
				return prefixLabels[i], []any{[]any{"prefix_tok", ">=", lo}, []any{"prefix_tok", "<", float64(hi)}}
			}
		}
		return prefixLabels[len(prefixEdges)], []any{[]any{"prefix_tok", ">=", float64(prefixEdges[len(prefixEdges)-1])}}
	},
	"model":       func(f *rowFacts) (string, []any) { return f.model, []any{[]any{"model", "==", f.model}} },
	"client":      func(f *rowFacts) (string, []any) { return f.agent, []any{[]any{"agent", "==", f.agent}} },
	"stop_reason": func(f *rowFacts) (string, []any) { return f.stop, []any{[]any{"stop_reason", "==", f.stop}} },
}

var (
	prefixEdges  = []int64{20000, 80000, 200000}
	prefixLabels = []string{"<20K", "20K-80K", "80K-200K", ">=200K"}
)

// gateLeg is one baseline of the ship gate: its per-row net on the held-out rows.
type gateLeg struct {
	name string
	v    []float64
}

// gateLegs are the ship gate's baselines, in one place for the build and the adapt report: what the tenant would get anyway, blanket K3, the
// fleet plan and (when known) the shipped default. A hindsight-best blanket plan is no gate: when the tuner picks a fixed plan it can at best tie it.
func gateLegs(k3, fleet, def []float64, fleetPlan int) []gateLeg {
	legs := []gateLeg{{"blanket K3", k3}, {"the fleet plan " + planName(fleetPlan), fleet}}
	if def != nil {
		legs = append(legs, gateLeg{"the default", def})
	}
	return legs
}

// shipGate is the auto builder's ship rule, shared by the build and the adapt report: the subject's held-out net is positive and its paired
// session-bootstrap interval against every leg lies above zero. failed names each leg (and the sign) that did not hold.
func shipGate(subject []float64, legs []gateLeg, sess []string) (pass bool, failed []string) {
	pass = true
	if t := sumOf(subject); t <= 0 {
		pass = false
		failed = append(failed, fmt.Sprintf("its own net %+.2f is not positive", t))
	}
	for _, l := range legs {
		a := make([]float64, len(subject))
		for j := range a {
			a[j] = subject[j] - l.v[j]
		}
		if c := sessionCI(a, sess, 1); c[0] <= 0 {
			pass = false
			failed = append(failed, fmt.Sprintf("vs %s %+.2f [%.2f, %.2f]", l.name, sumOf(a), c[0], c[1]))
		}
	}
	return pass, failed
}

// autoEvalResult is one build's held-out evaluation, shared by the builder (autoSpec) and the adapt report (adaptcache.go).
type autoEvalResult struct {
	insufficient string // why there was nothing to evaluate; "" when evaluated
	cut          int64
	tenantRows   []int // indices of the tenant's rows
	test         []int // indices of the tenant's held-out rows (ending at or after cut)
	nSessTest    int
	scope        []string // the tuner scope of every row: tenant[\x1fsegment] for the tenant's rows when segmented
	fit, final   *predictive.AutoFit
}

// heldPlan is the subject's plan on row i, tuned before the cut; inPlan is the plan refitted on the whole window (in-sample).
func (e *autoEvalResult) heldPlan(i int) int { return e.fit.PlanFor(e.scope[i]) }
func (e *autoEvalResult) inPlan(i int) int   { return e.final.PlanFor(e.scope[i]) }

// autoEval tunes the tenant's plan(s) on the fleet's rows resolved before the tenant's 70% point and refits on all of them. segBy "" is one plan
// for the tenant; otherwise one tuner scope per (tenant, segment), each shrunk toward the fleet.
func autoEval(rows []predictive.AutoRow, facts []rowFacts, tenant string, until int64, segBy string) *autoEvalResult {
	e := &autoEvalResult{scope: make([]string, len(rows))}
	seg := segFeatures[segBy]
	var ends []int64
	sess := map[string]bool{}
	for i, r := range rows {
		e.scope[i] = r.Scope
		if r.Scope != tenant {
			continue
		}
		e.tenantRows = append(e.tenantRows, i)
		ends = append(ends, r.EndMs)
		if seg != nil {
			k, _ := seg(&facts[i])
			e.scope[i] = tenant + "\x1f" + k
		}
	}
	tuned := rows
	if seg != nil {
		tuned = make([]predictive.AutoRow, len(rows))
		for i, r := range rows {
			r.Scope = e.scope[i]
			tuned[i] = r
		}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	var train []predictive.AutoRow
	if len(ends) > 0 {
		e.cut = ends[int(0.7*float64(len(ends)-1))]
		for _, r := range tuned {
			if float64(r.EndMs)+2700e3 <= float64(e.cut) {
				train = append(train, r)
			}
		}
		for _, i := range e.tenantRows {
			if rows[i].EndMs >= e.cut {
				e.test = append(e.test, i)
				sess[rows[i].Session] = true
			}
		}
	}
	e.nSessTest = len(sess)
	if len(e.test) < autoMinTst || e.nSessTest < autoMinSes || len(train) == 0 {
		e.insufficient = fmt.Sprintf("held-out rows %d (need %d), held-out sessions %d (need %d), training rows %d", len(e.test), autoMinTst, e.nSessTest, autoMinSes, len(train))
		return e
	}
	e.fit = predictive.AutoTune(train, e.cut, predictive.AutoGrid{})
	e.final = predictive.AutoTune(tuned, until, predictive.AutoGrid{})
	return e
}

// autoSpec builds the campaign document for one tenant from the fleet's rows. It is the whole build; runAutoBuild only does the I/O around it.
// facts (same order as rows, may be nil) add the shipped default to the gate and allow segBy.
func autoSpec(rows []predictive.AutoRow, facts []rowFacts, tenant string, since, until int64, sessionEnd bool, segBy string) (spec map[string]any, rec string) {
	e := autoEval(rows, facts, tenant, until, segBy)
	sess := map[string]bool{}
	for _, i := range e.tenantRows {
		sess[rows[i].Session] = true
	}
	spec = map[string]any{"schema": predictive.SpecSchema, "version": "2.0.0", "built_at": time.Now().UTC().Format(time.RFC3339),
		"builder": map[string]any{"engine": "auto", "backend": "closed-form", "code": "predictive/autotune.go"}, "scope": map[string]any{"type": "tenant", "id": tenant},
		"window": map[string]any{"start_ms": since, "end_ms": until, "n_decision_points": len(e.tenantRows), "n_sessions": len(sess)}, "objective": "net_usd",
		"session_end": sessionEnd, "menu": map[string]any{}}
	if segBy != "" {
		spec["segment_by"] = segBy
	}
	menu := spec["menu"].(map[string]any)
	for k := 1; k <= autoK; k++ {
		gaps := make([]float64, k)
		for j := range gaps {
			gaps[j] = autoX
		}
		menu[planName(k)] = map[string]any{"gaps_s": gaps, "max_pings": k, "tier": "5m"}
	}
	ruleOf := func(k int) map[string]any {
		if k > 0 {
			return map[string]any{"kind": "plan", "plan": planName(k)}
		}
		return map[string]any{"kind": "none"}
	}
	policy := func(k int) map[string]any {
		return map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "all", "when": []any{}, "rule": ruleOf(k)}}, "fallback": map[string]any{"kind": "none"}}
	}
	if e.insufficient != "" {
		spec["policy"] = policy(0)
		spec["evidence"] = map[string]any{"recommendation": "insufficient_data", "reason": e.insufficient}
		return spec, "insufficient_data"
	}
	test := e.test
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
	kAuto := e.fit.PlanFor(tenant)
	subject := fmt.Sprintf("auto(%s)", planName(kAuto))
	if segBy != "" {
		subject = "auto(by " + segBy + ")"
	}
	type cand struct {
		name string
		v    []float64
		gate bool
	}
	cands := []cand{{subject, net(e.heldPlan), false}, {"never", net(fixed(0)), false}, {"blanket-K1", net(fixed(1)), false}, {"blanket-K2", net(fixed(2)), false},
		{"blanket-K3", net(fixed(3)), true}, {fmt.Sprintf("fleet-plan(%s)", planName(e.fit.FleetPlan)), net(fixed(e.fit.FleetPlan)), true}}
	if facts != nil {
		v := make([]float64, len(test))
		for t, i := range test {
			v[t] = facts[i].gain(facts[i].defaultK(), autoX, true, cfScale, cfPingScale)
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
	ship, _ := shipGate(a0, gateLegs(net(fixed(3)), net(fixed(e.fit.FleetPlan)), defV, e.fit.FleetPlan), ss)
	best, bestNet := "", math.Inf(-1)
	for i, c := range cands {
		n := sum(c.v)
		table = append(table, map[string]any{"strategy": c.name, "net_usd": r2(n), "ci95": sessionCI(c.v, ss, 0), "gap_captured": math.Round(n/max(orc, 1e-9)*1000) / 1000})
		if i == 0 {
			continue
		}
		a := make([]float64, len(a0))
		for j := range a {
			a[j] = a0[j] - c.v[j]
		}
		ci := sessionCI(a, ss, 1)
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
	ownName := planName(e.final.PlanFor(tenant))
	if segBy == "" {
		own = policy(e.final.PlanFor(tenant))
	} else {
		seen := map[string][]any{}
		for _, i := range e.tenantRows {
			k, when := segFeatures[segBy](&facts[i])
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
			p := e.final.PlanFor(tenant + "\x1f" + k)
			plans[k] = planName(p)
			segs = append(segs, map[string]any{"id": segBy + ":" + k, "when": seen[k], "rule": ruleOf(p)})
		}
		own = map[string]any{"kind": "rules", "segments": segs, "fallback": ruleOf(e.final.FleetPlan)}
		ownName = fmt.Sprintf("by %s: %v, otherwise %s", segBy, plans, planName(e.final.FleetPlan))
	}
	spec["own_policy"], spec["own_plan"] = own, ownName
	shipped := planName(e.final.FleetPlan)
	if rec == "ship" {
		spec["policy"], shipped = own, ownName
	} else {
		// the tenant's own plan passed no held-out check: run the fleet plan, which is what the tenant would get anyway
		spec["policy"] = policy(e.final.FleetPlan)
	}
	spec["evidence"] = map[string]any{
		"klass":   "counterfactual: closed-form replay on this tenant's own history; oracle row = class oracle (hindsight best plan per decision, never deployable)",
		"split":   fmt.Sprintf("plan tuned on the fleet's rows resolved before %d (the tenant's 70%% point), scored on the tenant's last 30%%: %d decision points, %d sessions", e.cut, len(test), e.nSessTest),
		"subject": subject, "table": table, "paired_vs_baselines": paired, "best_simple_baseline": best, "recommendation": rec, "oracle_net_usd": r2(orc),
		"tuner": map[string]any{"half_life_days": jsonNum(e.fit.HalfLifeDays), "kappa": jsonNum(e.fit.Kappa), "fleet_plan": planName(e.fit.FleetPlan), "tenant_plan": planName(kAuto),
			"tenant_weight": r2(e.fit.Weight[tenant])},
		"shipped": map[string]any{"plan": shipped, "half_life_days": jsonNum(e.final.HalfLifeDays), "kappa": jsonNum(e.final.Kappa), "fleet_plan": planName(e.final.FleetPlan),
			"note": "ship: the same tuner refitted on the whole window (the table above is the held-out evidence for the procedure); otherwise the refitted fleet plan"},
		"engine": map[string]any{"benefit_scale": cfScale, "ping_scale": cfPingScale},
		"note":   "dollars are closed-form units calibrated to an offline replay engine; compare strategies, not absolute levels",
	}
	spec["caveats"] = []string{"the tenant's last 30% of rows is one held-out period; a tenant with few sessions gets wide intervals, see recommendation",
		"returns not seen before the window end count as no-return (pings wasted)", "closed-form replay: pings land on schedule and do not fail"}
	return spec, rec
}

// autoData is the fleet's history an auto build reads for the tuner's prior: one read capped at the dashboard's newest-rows limit. A tenant's
// own rows are read separately by autoRowsFor, so a busy fleet can never push them out of the tenant's build. Truncation is reported, never silent.
type autoData struct {
	fleet                   []*kvcache.Request
	since                   int64
	fleetRead, fleetMatched int64
	fleetRows               map[bool]autoRowSet // autoRowsX of the fleet read, by session_end
}

// autoRowSet is tuner rows with their facts, in the same order.
type autoRowSet struct {
	rows  []predictive.AutoRow
	facts []rowFacts
}

func (h *Handler) autoFleet(since, until int64) (*autoData, error) {
	fleet, total, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{TenantAll: true, Since: since, Until: until}, dash.KVCacheOptions{ByThread: true})
	if err != nil {
		return nil, err
	}
	return &autoData{fleet: fleet, since: since, fleetRead: int64(len(fleet)), fleetMatched: total, fleetRows: map[bool]autoRowSet{}}, nil
}

// autoOwn reads one tenant's own requests in full (ownTruncated: the read hit the dashboard's row cap). The adapt report needs them raw.
func (h *Handler) autoOwn(d *autoData, tenant string, untilMs int64) (own []*kvcache.Request, ownTruncated bool, err error) {
	own, n, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{Tenant: tenant, Since: d.since, Until: untilMs}, dash.KVCacheOptions{ByThread: true})
	return own, n > int64(len(own)), err
}

// rowsWith is the tuner's rows for one tenant: the fleet's rows of every other tenant, plus the tenant's own rows built from own (its rows in
// the fleet read are replaced, so each is counted once).
func (d *autoData) rowsWith(own []*kvcache.Request, tenant string, price func(string) (float64, float64, float64, float64, bool), sessionEnd bool, untilMs int64) autoRowSet {
	fr, ok := d.fleetRows[sessionEnd]
	if !ok {
		fr.rows, fr.facts = autoRowsX(d.fleet, price, sessionEnd, untilMs)
		d.fleetRows[sessionEnd] = fr
	}
	mine, mineFacts := autoRowsX(own, price, sessionEnd, untilMs)
	out := autoRowSet{rows: make([]predictive.AutoRow, 0, len(fr.rows)+len(mine)), facts: make([]rowFacts, 0, len(fr.rows)+len(mine))}
	for i, r := range fr.rows {
		if r.Scope != tenant {
			out.rows, out.facts = append(out.rows, r), append(out.facts, fr.facts[i])
		}
	}
	out.rows, out.facts = append(out.rows, mine...), append(out.facts, mineFacts...)
	return out
}

// autoRowsFor is rowsWith on the tenant's own read. The tenant's requests are read here and dropped once its rows are built, so a refresh pass
// holds the fleet read plus one tenant's read at a time (each at most the dashboard's row cap). ownTruncated: the tenant's own read hit the cap.
func (h *Handler) autoRowsFor(d *autoData, tenant string, price func(string) (float64, float64, float64, float64, bool), sessionEnd bool, untilMs int64) (set autoRowSet, ownTruncated bool, err error) {
	own, ownTruncated, err := h.autoOwn(d, tenant, untilMs)
	if err != nil {
		return autoRowSet{}, false, err
	}
	return d.rowsWith(own, tenant, price, sessionEnd, untilMs), ownTruncated, nil
}

// note records in the spec what the build read, and says so when a read was cut at the cap.
func (d *autoData) note(spec map[string]any, ownTruncated bool) {
	if ev, _ := spec["evidence"].(map[string]any); ev != nil {
		ev["data"] = map[string]any{"fleet_rows_read": d.fleetRead, "fleet_rows_in_window": d.fleetMatched, "fleet_truncated": d.fleetMatched > d.fleetRead,
			"tenant_truncated": ownTruncated}
	}
	cv, _ := spec["caveats"].([]string)
	if d.fleetMatched > d.fleetRead {
		cv = append(cv, fmt.Sprintf("the fleet prior used the newest %d of %d rows in the window (the tenant's own rows are all read)", d.fleetRead, d.fleetMatched))
	}
	if ownTruncated {
		cv = append(cv, "the tenant's own history exceeds the read cap: only its newest rows are used")
	}
	spec["caveats"] = cv
}
func (h *Handler) runAutoBuild(id, out string, in cacheBuildIn, since, until int64) {
	defer func() { buildMu.Lock(); delete(buildActive, id); buildMu.Unlock() }()
	t0 := time.Now()
	fail := func(msg string, err error) {
		slog.Warn("context-guru: auto campaign build failed", "campaign", id, "err", err)
		_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "failed", "error": msg})
	}
	data, err := h.autoFleet(since, until)
	var set autoRowSet
	var cut bool
	if err == nil {
		set, cut, err = h.autoRowsFor(data, in.ID, h.autoPrice(), in.SessionEnd, until)
	}
	if err != nil {
		fail("could not read the fleet's history", err)
		return
	}
	spec, rec := autoSpec(set.rows, set.facts, in.ID, since, until, in.SessionEnd, in.SegmentBy)
	data.note(spec, cut)
	if err := writeSpecValidated(out, spec); err != nil {
		fail("the built spec did not validate: "+err.Error(), err)
		return
	}
	_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "done", "recommendation": rec, "seconds": math.Round(time.Since(t0).Seconds()*10) / 10})
}

// Refresh: a live (shadow or active) auto campaign is rebuilt in place every autoRefreshEvery on the same window length ending now, so its plans
// follow the traffic. In offline replay a plan built once and left for weeks drifted and lost money; rebuilt daily it did not. The mode is kept; a rebuild that finds too little history keeps the previous plan.
// CG_CAMPAIGN_AUTO_REFRESH is a Go duration ("24h", the default); "0" or "off" turns it off.
func autoRefreshEvery() time.Duration {
	v := os.Getenv("CG_CAMPAIGN_AUTO_REFRESH")
	if v == "" {
		return 24 * time.Hour
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return 0
}

var autoRefreshLast atomic.Int64

// maybeRefreshAutoCampaigns is called from the keeper's sweep tick; it starts at most one refresh per period, never alongside a build.
func (h *Handler) maybeRefreshAutoCampaigns(now time.Time) {
	every := autoRefreshEvery()
	last := autoRefreshLast.Load()
	if every <= 0 || h.predRT() == nil || h.opts.Dashboard == nil || h.opts.Dashboard.DB() == nil || now.UnixMilli()-last < every.Milliseconds() ||
		!autoRefreshLast.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	if last == 0 { // the first tick after a start only sets the clock: a restart is not a reason to rebuild
		return
	}
	go h.refreshAutoCampaigns(now)
}

func (h *Handler) refreshAutoCampaigns(now time.Time) {
	buildMu.Lock()
	if len(buildActive) > 0 {
		buildMu.Unlock()
		return
	}
	buildActive["auto-refresh"] = true
	buildMu.Unlock()
	defer func() { buildMu.Lock(); delete(buildActive, "auto-refresh"); buildMu.Unlock() }()
	rt := h.predRT()
	rt.mu.RLock()
	live := make([]*predCampaign, 0, len(rt.byTenant))
	for _, pc := range rt.byTenant {
		live = append(live, pc)
	}
	rt.mu.RUnlock()
	// One read of the fleet's history per pass, for the longest window any live auto campaign uses, shared by all of them (the fleet read is
	// capped at the dashboard's newest-rows limit; each campaign tenant's own rows are read in turn, see autoRowsFor), released when the pass ends.
	type job struct {
		pc     *predCampaign
		base   string
		since  int64
		se     bool
		segBy  string
		active bool
	}
	u := now.UnixMilli()
	var jobs []job
	oldest := u
	for _, pc := range live {
		base := filepath.Join(rt.dir, pc.meta.ID)
		var old, req map[string]any
		if readJSONFile(filepath.Join(base, "spec.json"), &old) != nil || readJSONFile(filepath.Join(base, "request.json"), &req) != nil {
			continue
		}
		if b, _ := old["builder"].(map[string]any); b == nil || b["engine"] != "auto" {
			continue
		}
		since, _ := req["since"].(float64)
		until, _ := req["until"].(float64)
		se, _ := req["session_end"].(bool)
		segBy, _ := req["segment_by"].(string)
		pc.mu.Lock()
		active := pc.meta.Mode == campaignModeActive
		pc.mu.Unlock()
		j := job{pc, base, u - int64(until-since), se, segBy, active}
		jobs = append(jobs, j)
		oldest = min(oldest, j.since)
	}
	if len(jobs) == 0 {
		return
	}
	data, err := h.autoFleet(oldest, u)
	if err != nil {
		slog.Warn("context-guru: auto campaign refresh could not read the history", "err", err)
		return
	}
	price := h.autoPrice()
	for _, j := range jobs { // one tenant's own read at a time, dropped before the next (see autoRowsFor)
		set, cut, err := h.autoRowsFor(data, j.pc.meta.Tenant, price, j.se, u)
		if err != nil {
			slog.Warn("context-guru: auto campaign refresh could not read a tenant's history", "campaign", j.pc.meta.ID, "err", err)
			continue
		}
		rows := make([]predictive.AutoRow, 0, len(set.rows))
		facts := make([]rowFacts, 0, len(set.rows))
		for i, r := range set.rows {
			if r.EndMs >= j.since {
				rows = append(rows, r)
				facts = append(facts, set.facts[i])
			}
		}
		spec, rec := autoSpec(rows, facts, j.pc.meta.Tenant, j.since, u, j.se, j.segBy)
		data.note(spec, cut)
		if rec == "insufficient_data" || (j.active && rec != "ship") {
			// an ACTIVE campaign changes its plan only on a rebuild that passed the same ship gate the mode change needs
			_ = writeJSONFile(filepath.Join(j.base, "status.json"), map[string]any{"state": "done", "recommendation": readStatus(j.base)["recommendation"],
				"refresh_kept_previous_plan": rec, "refreshed_at": now.UnixMilli()})
			continue
		}
		spec["refreshed_at"] = now.UTC().Format(time.RFC3339)
		b, err := json.Marshal(spec)
		var ps *predictive.Spec
		if err == nil {
			ps, err = predictive.ParseSpec(b)
		}
		if err != nil {
			slog.Warn("context-guru: auto campaign refresh produced an invalid spec; the previous one stays", "campaign", j.pc.meta.ID, "err", err)
			continue
		}
		if !j.pc.swapSpec(j.base, spec, ps, rec) {
			continue
		}
		_ = writeJSONFile(filepath.Join(j.base, "status.json"), map[string]any{"state": "done", "recommendation": rec, "refreshed_at": now.UnixMilli()})
	}
}

// swapSpec installs a refreshed, already-parsed spec, in memory AND on disk, unless the campaign is ACTIVE and the rebuild was not "ship". The
// mode check, the file write and the swap happen under one lock, so a campaign switched to active while its rebuild ran never takes a plan that
// failed the gate, now or after a restart (which reloads spec.json).
func (pc *predCampaign) swapSpec(dir string, spec map[string]any, ps *predictive.Spec, rec string) bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.meta.Mode == campaignModeActive && rec != "ship" {
		return false
	}
	// The write runs under pc.mu, which the request path also takes; it is one small file once a day, so the hold is short.
	if err := writeJSONFile(filepath.Join(dir, "spec.json"), spec); err != nil {
		return false
	}
	pc.spec = ps
	return true
}

// writeSpecValidated parses the spec with the runtime's own parser BEFORE it replaces dir/spec.json (written to a temp file and renamed), so a bad
// rebuild never overwrites a working campaign.
func writeSpecValidated(dir string, spec map[string]any) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if _, err := predictive.ParseSpec(b); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "spec.json"), spec)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// autoPrice is the per-model price lookup of an auto build, memoised for one build.
func (h *Handler) autoPrice() func(string) (float64, float64, float64, float64, bool) {
	type pr struct {
		in, out, read, write float64
		ok                   bool
	}
	prices := map[string]pr{}
	return func(m string) (float64, float64, float64, float64, bool) {
		p, seen := prices[m]
		if !seen && h.opts.Prices != nil {
			if q, ok := h.opts.Prices.Price(context.Background(), m); ok && !q.Zero() {
				p = pr{q.Input, q.Output, q.CacheRead, q.CacheWrite, true}
			}
			prices[m] = p
		}
		return p.in, p.out, p.read, p.write, p.ok
	}
}

func sumOf(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t
}
