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
	autoX      = campaignKeepAlive5mIdle // = config.DefaultKeepAliveIdle (270 s); apply() also caps it at the tenant's own interval
	autoBoot   = 1000
	autoMinTst = 500 // the same floors as scripts/campaign/build_campaign.py
	autoMinSes = 20
)

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
	type sk struct{ user, conv string }
	last := map[sk]int64{}
	for _, r := range reqs {
		k := sk{r.User, r.ConversationID}
		last[k] = max(last[k], r.TS)
	}
	out := make([]predictive.AutoRow, 0, len(reqs))
	for _, r := range reqs {
		end := r.TS + int64(r.UpstreamMs)
		if r.KeepAlive || r.Rejected || !r.CostKnown || r.CachedContext <= 0 || float64(end)+2700e3 > float64(untilMs) {
			continue
		}
		pIn, pOut, pRead, pWrite, ok := price(r.Model)
		if !ok || pWrite <= pRead {
			continue
		}
		u := r.UpstreamMs / 1000
		gap := 0.0
		if r.HasNext {
			gap = float64(r.NextTS-end) / 1000
		}
		stop := math.Inf(1)
		if sessionEnd && !r.HasNext && r.TS == last[sk{r.User, r.ConversationID}] {
			stop = 0
		}
		stake := float64(r.CachedContext) * (pWrite - pRead)
		ping := (&predictive.Response{Prefix: r.CachedContext, PIn: pIn, POut: pOut, PRead: pRead}).PingCost()
		g := make([]float64, autoK+1)
		for k := 1; k <= autoK; k++ {
			fired, rescued := planOutcome(k, autoX, u, gap, r.HasNext, stop)
			g[k] = -cfPingScale * float64(fired) * ping
			if rescued {
				g[k] += cfScale * stake
			}
		}
		out = append(out, predictive.AutoRow{Scope: r.User, Session: r.ConversationID, EndMs: end, Gain: g})
	}
	return out
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

// autoSpec builds the campaign document for one tenant from the fleet's rows. It is the whole build; runAutoBuild only does the I/O around it.
func autoSpec(rows []predictive.AutoRow, tenant string, since, until int64, sessionEnd bool) (spec map[string]any, rec string) {
	var ends []int64
	sess := map[string]bool{}
	for _, r := range rows {
		if r.Scope == tenant {
			ends = append(ends, r.EndMs)
			sess[r.Session] = true
		}
	}
	spec = map[string]any{"schema": predictive.SpecSchema, "version": "2.0.0", "built_at": time.Now().UTC().Format(time.RFC3339),
		"builder": map[string]any{"engine": "auto", "backend": "closed-form", "code": "predictive/autotune.go"}, "scope": map[string]any{"type": "tenant", "id": tenant},
		"window": map[string]any{"start_ms": since, "end_ms": until, "n_decision_points": len(ends), "n_sessions": len(sess)}, "objective": "net_usd",
		"session_end": sessionEnd, "menu": map[string]any{}}
	menu := spec["menu"].(map[string]any)
	for k := 1; k <= autoK; k++ {
		gaps := make([]float64, k)
		for j := range gaps {
			gaps[j] = autoX
		}
		menu[planName(k)] = map[string]any{"gaps_s": gaps, "max_pings": k, "tier": "5m"}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i] < ends[j] })
	var cut int64
	var train, test []predictive.AutoRow
	if len(ends) > 0 {
		cut = ends[int(0.7*float64(len(ends)-1))]
		for _, r := range rows {
			if float64(r.EndMs)+2700e3 <= float64(cut) {
				train = append(train, r)
			}
			if r.Scope == tenant && r.EndMs >= cut {
				test = append(test, r)
			}
		}
	}
	nsTest := map[string]bool{}
	for _, r := range test {
		nsTest[r.Session] = true
	}
	policy := func(k int) map[string]any {
		rule := map[string]any{"kind": "none"}
		if k > 0 {
			rule = map[string]any{"kind": "plan", "plan": planName(k)}
		}
		return map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "all", "when": []any{}, "rule": rule}}, "fallback": map[string]any{"kind": "none"}}
	}
	if len(test) < autoMinTst || len(nsTest) < autoMinSes || len(train) == 0 {
		spec["policy"] = policy(0)
		spec["evidence"] = map[string]any{"recommendation": "insufficient_data",
			"reason": fmt.Sprintf("held-out rows %d (need %d), held-out sessions %d (need %d), training rows %d", len(test), autoMinTst, len(nsTest), autoMinSes, len(train))}
		return spec, "insufficient_data"
	}
	fit := predictive.AutoTune(train, cut, predictive.AutoGrid{})
	kAuto := fit.PlanFor(tenant)
	ss := make([]string, len(test))
	for i, r := range test {
		ss[i] = r.Session
	}
	net := func(k int) []float64 {
		v := make([]float64, len(test))
		for i, r := range test {
			v[i] = r.Gain[k]
		}
		return v
	}
	sum := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	}
	orc := 0.0
	for _, r := range test {
		b := 0.0
		for _, g := range r.Gain {
			b = max(b, g)
		}
		orc += b
	}
	subject := fmt.Sprintf("auto(%s)", planName(kAuto))
	type cand struct {
		name string
		k    int
	}
	cands := []cand{{subject, kAuto}, {"never", 0}, {"blanket-K1", 1}, {"blanket-K2", 2}, {"blanket-K3", 3}, {fmt.Sprintf("fleet-plan(%s)", planName(fit.FleetPlan)), fit.FleetPlan}}
	r2 := func(x float64) float64 { return math.Round(x*100) / 100 }
	// The gate is the plan the tenant would get anyway: the fleet plan, and blanket K3 (the usual default). A hindsight-best blanket plan on the
	// held-out rows is reported but is no gate: when the tuner picks a fixed plan it can at best tie it.
	var table, paired []any
	best, bestNet, ship := "", math.Inf(-1), sum(net(kAuto)) > 0
	for i, c := range cands {
		v := net(c.k)
		n := sum(v)
		table = append(table, map[string]any{"strategy": c.name, "net_usd": r2(n), "ci95": sessionCI(v, ss, 0), "gap_captured": math.Round(n/max(orc, 1e-9)*1000) / 1000})
		if i == 0 {
			continue
		}
		a := net(kAuto)
		for j := range a {
			a[j] -= v[j]
		}
		ci := sessionCI(a, ss, 1)
		paired = append(paired, map[string]any{"vs": c.name, "diff_usd": r2(sum(a)), "ci95": ci})
		if c.k == 3 || i == len(cands)-1 {
			ship = ship && ci[0] > 0
			if n > bestNet {
				best, bestNet = c.name, n
			}
		}
	}
	rec = "baseline"
	if ship {
		rec = "ship"
	}
	final := predictive.AutoTune(rows, until, predictive.AutoGrid{})
	kShip := final.PlanFor(tenant)
	if rec != "ship" {
		// the tenant's own plan passed no held-out check: run the fleet plan, which is what the tenant would get anyway
		kShip = final.FleetPlan
	}
	spec["policy"] = policy(kShip)
	spec["evidence"] = map[string]any{
		"klass":   "counterfactual: closed-form replay on this tenant's own history; oracle row = class oracle (hindsight best plan per decision, never deployable)",
		"split":   fmt.Sprintf("plan tuned on the fleet's rows resolved before %d (the tenant's 70%% point), scored on the tenant's last 30%%: %d decision points, %d sessions", cut, len(test), len(nsTest)),
		"subject": subject, "table": table, "paired_vs_baselines": paired, "best_simple_baseline": best, "recommendation": rec, "oracle_net_usd": r2(orc),
		"tuner": map[string]any{"half_life_days": jsonNum(fit.HalfLifeDays), "kappa": jsonNum(fit.Kappa), "fleet_plan": planName(fit.FleetPlan), "tenant_plan": planName(kAuto),
			"tenant_weight": r2(fit.Weight[tenant])},
		"shipped": map[string]any{"plan": planName(kShip), "half_life_days": jsonNum(final.HalfLifeDays), "kappa": jsonNum(final.Kappa), "fleet_plan": planName(final.FleetPlan),
			"note": "ship: the same tuner refitted on the whole window (the table above is the held-out evidence for the procedure); otherwise the refitted fleet plan"},
		"engine": map[string]any{"benefit_scale": cfScale, "ping_scale": cfPingScale, "sim2_sha": "55373000938a"},
		"note":   "dollars are closed-form units calibrated to the pinned replay engine; compare strategies, not absolute levels",
	}
	spec["caveats"] = []string{"the tenant's last 30% of rows is one held-out period; a tenant with few sessions gets wide intervals, see recommendation",
		"returns not seen before the window end count as no-return (pings wasted)", "closed-form replay: pings land on schedule and do not fail"}
	return spec, rec
}

func (h *Handler) runAutoBuild(id, out string, in cacheBuildIn, since, until int64) {
	defer func() { buildMu.Lock(); delete(buildActive, id); buildMu.Unlock() }()
	t0 := time.Now()
	fail := func(msg string, err error) {
		slog.Warn("context-guru: auto campaign build failed", "campaign", id, "err", err)
		_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "failed", "error": msg})
	}
	reqs, _, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{TenantAll: true, Since: since, Until: until}, dash.KVCacheOptions{})
	if err != nil {
		fail("could not read the fleet's history", err)
		return
	}
	spec, rec := autoSpec(autoRows(reqs, h.autoPrice(), in.SessionEnd, until), in.ID, since, until, in.SessionEnd)
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
	// One read of the fleet's history per pass, for the longest window any live auto campaign uses, shared by all of them (the dataset read is
	// capped at the dashboard's kvCacheMaxRows newest rows), and released when the pass ends.
	type job struct {
		pc     *predCampaign
		base   string
		since  int64
		se     bool
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
		pc.mu.Lock()
		active := pc.meta.Mode == campaignModeActive
		pc.mu.Unlock()
		j := job{pc, base, u - int64(until-since), se, active}
		jobs = append(jobs, j)
		oldest = min(oldest, j.since)
	}
	if len(jobs) == 0 {
		return
	}
	reqs, _, err := h.opts.Dashboard.DB().KVCacheDataset(dash.Filter{TenantAll: true, Since: oldest, Until: u}, dash.KVCacheOptions{})
	if err != nil {
		slog.Warn("context-guru: auto campaign refresh could not read the history", "err", err)
		return
	}
	price := h.autoPrice()
	rowsBySE := map[bool][]predictive.AutoRow{}
	for _, j := range jobs {
		all, ok := rowsBySE[j.se]
		if !ok {
			all = autoRows(reqs, price, j.se, u)
			rowsBySE[j.se] = all
		}
		rows := make([]predictive.AutoRow, 0, len(all))
		for _, r := range all {
			if r.EndMs >= j.since {
				rows = append(rows, r)
			}
		}
		spec, rec := autoSpec(rows, j.pc.meta.Tenant, j.since, u, j.se)
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
