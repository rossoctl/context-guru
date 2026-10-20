package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/proxyapp"
)

// DefaultArms is what a campaign may choose from: the 5-minute tier only — fixed-5m, the keep-alive
// with 1 to 11 pings per idle span, and the stop-reason gate. no-cache and every 1-hour arm are
// excluded by operator decision (2026-10-20).
func DefaultArms() []string {
	arms := []string{kvcache.StrategyFixed5m}
	for k := 1; k <= kvcache.MaxKeepAlivePings; k++ {
		arms = append(arms, kvcache.KeepAlive5mPings(k).Name())
	}
	return append(arms, kvcache.StrategyStopReasonGated)
}

// Inputs is one run: which history to read, how to price it, and what to score it against.
type Inputs struct {
	// Arms is the candidate list every cell is decided over. nil = DefaultArms.
	Arms     []string
	DB       *dash.DB
	Pricer   proxyapp.Pricer
	Since    int64 // epoch ms, inclusive
	Until    int64 // epoch ms, exclusive
	Baseline string
	// Holdout is the share of the window, at its end, the campaign is scored on out of sample.
	// 0 skips the check.
	Holdout float64
}

// Result is the two files.
type Result struct {
	Campaign *dash.KVCacheSuggestions // the upload: GET /api/kvcache/suggest's own shape
	Report   *Report
}

// Policy is one way to run the history, priced over every cell.
type Policy struct {
	Name string `json:"name"`
	// Kind is campaign (each cell's best LIVE-enforceable arm: the upload), arm (one arm in every
	// cell: a naive policy), observed (the tier each request asked for), ceiling (optimal: reads the
	// future, so no policy can reach it), simulated (each cell's best arm including ones with no live
	// path), or segmented (each cell's winner on a finer key; the upload format cannot carry it).
	Kind      string  `json:"kind"`
	CostUSD   float64 `json:"cost_usd"`
	SavingUSD float64 `json:"saving_usd"` // baseline - this; negative = costs more than the baseline
	SavingPct float64 `json:"saving_pct"`
	// OfOptimalPct is SavingUSD / the ceiling's SavingUSD x 100: how much of the hindsight-best
	// saving this policy keeps.
	OfOptimalPct float64 `json:"of_optimal_pct"`
}

// Breakdown is the comparison inside one slice: one account, one hour, one weekday...
type Breakdown struct {
	Key      string `json:"key"`
	Requests int64  `json:"requests"`
	// What each policy cost in this slice. BestArm is the cheapest single arm in the slice.
	BaselineUSD float64 `json:"baseline_usd"`
	CampaignUSD float64 `json:"campaign_usd"`
	ObservedUSD float64 `json:"observed_usd"`
	OptimalUSD  float64 `json:"optimal_usd"`
	BestArm     string  `json:"best_arm"`
	BestArmUSD  float64 `json:"best_arm_usd"`
	// CampaignArms counts the cells in this slice by the arm the campaign gives them.
	CampaignArms map[string]int `json:"campaign_arms,omitempty"`
}

// Report is what the campaign was decided from, and what it is worth.
type Report struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
	// HistorySince and HistoryUntil are the oldest and newest request actually read. Retention can
	// make this much shorter than Since..Until; every figure covers this span, not the asked one.
	HistorySince int64    `json:"history_since"`
	HistoryUntil int64    `json:"history_until"`
	Baseline     string   `json:"baseline"`
	Requests     int64    `json:"requests"`
	Accounts     []string `json:"accounts"`
	// Policies is every policy, cheapest first.
	Policies []Policy `json:"policies"`
	// Campaign counts the upload's cells, and which ones the dashboard will not enforce.
	Campaign CampaignSummary `json:"campaign"`
	// Current is what the live keep-alive ACTUALLY did over the same span, from core's own ledger
	// (dash.DB.KeepAliveLedger). observed-policy above replays the tier each request asked for and
	// cannot see the proxy's pings; this is the measured part it leaves out.
	Current *dash.KeepAliveLedger `json:"current_keepalive,omitempty"`
	// Holdout is the campaign chosen on the first part of the window and scored on the rest.
	Holdout *dash.KVCacheHoldout `json:"holdout,omitempty"`
	// Breakdowns by account, hour (UTC), weekday (UTC), model and agent.
	Breakdowns map[string][]Breakdown `json:"breakdowns"`
	Notes      []string               `json:"notes"`
}

// CampaignSummary counts the upload's cells.
type CampaignSummary struct {
	Cells int            `json:"cells"`
	ByArm map[string]int `json:"by_arm"`
	// Enforceable cells are the ones the dashboard will activate, or keep on the baseline.
	Enforceable int `json:"enforceable"`
	// NotEnforced counts the cells the dashboard records but will not activate, by reason.
	NotEnforced map[string]int `json:"not_enforced"`
	// PredictedSavingUSD sums the enforceable cells' savings.
	PredictedSavingUSD float64 `json:"predicted_saving_usd"`
	// Repicked cells had a simulated winner with no live enforcement path, and were given the best
	// arm a campaign can run instead. RepickCostUSD is the saving that choice gives up.
	Repicked      int     `json:"repicked_cells"`
	RepickCostUSD float64 `json:"repick_cost_usd"`
}

// cellCosts is one decided cell, as core reported it: every arm's cost on the cell's own rows.
type cellCosts struct {
	user     string
	hour     int
	segment  string
	requests int64
	winner   string
	winUSD   float64
	optUSD   float64
	base     float64
	arms     map[string]float64 // arm -> its cost on this cell's rows
}

func costsOf(c dash.KVCacheSuggestion, segment string) cellCosts {
	cc := cellCosts{user: c.User, hour: c.HourUTC, segment: segment, requests: c.Requests,
		winner: c.BestStrategy, winUSD: c.BestUSD, optUSD: c.OptimalUSD, base: c.BaselineUSD,
		arms: map[string]float64{}}
	for _, s := range c.Candidates {
		cc.arms[s.Strategy] = s.StrategyUSD
	}
	if cc.winner == "" { // nothing valued: the cell stays on the baseline
		cc.winner, cc.winUSD = c.Baseline, c.BaselineUSD
	}
	if !c.OptimalKnown {
		cc.optUSD = cc.winUSD
	}
	return cc
}

// Run asks core every question and assembles both files. It prices nothing itself.
//
// EVERY TOTAL IS A SUM OF CORE'S OWN CELL RESULTS. KVCacheSuggest replays each (account, hour) cell
// under every arm (Candidates), its winner (BestUSD) and the exact ceiling (OptimalUSD). A naive
// policy — one arm everywhere — is therefore that arm's cost summed over the cells, and the
// campaign is the winners' costs summed. Every policy is measured on the same cells, the same rows
// and the same rates as the dashboard's own campaign preview.
func Run(in Inputs) (*Result, error) {
	f := dash.Filter{TenantAll: true, Since: in.Since, Until: in.Until}
	arms := in.Arms
	if len(arms) == 0 {
		arms = DefaultArms()
	}
	// MaxPings is the simulator's ceiling on EVERY arm; it must be at least the largest K-ping
	// budget, or keepalive-5m-x11 would silently be scored as x2 (the default ceiling).
	cfg := dash.KVCacheSimConfig{Baseline: in.Baseline, Semantics: kvcache.DefaultSemantics(),
		Strategies: arms, MaxPings: kvcache.MaxKeepAlivePings}

	sug, err := in.DB.KVCacheSuggest(f, dash.KVCacheOptions{}, in.Pricer, cfg)
	if err != nil {
		return nil, fmt.Errorf("suggest: %w", err)
	}
	if sug.Truncated {
		return nil, fmt.Errorf("the window holds %d requests and the read stops at %d; use a shorter -days "+
			"so that every cell is decided on all of its rows", sug.Total, sug.Scanned)
	}
	// AN UNPRICED UPLOAD IS REFUSED. A cell no arm could be priced on has no winner, and an upload
	// made of such cells creates a campaign that changes nothing while looking like a decision.
	priced := 0
	for _, c := range sug.Cells {
		if c.Valued {
			priced++
		}
	}
	if len(sug.Cells) > 0 && priced == 0 {
		return nil, fmt.Errorf("none of the %d cells could be priced: the price list knows none of this "+
			"history's models; pass -prices with the gateway's MODEL_PRICES table", len(sug.Cells))
	}
	// The simulated per-cell best, kept for the report before the upload is re-picked.
	simBest := 0.0
	for _, c := range sug.Cells {
		simBest += c.BestUSD
	}
	lost, changed := enforceableWinners(sug)
	cells := make([]cellCosts, 0, len(sug.Cells))
	rep := &Report{Since: in.Since, Until: in.Until, Baseline: sug.Baseline, Accounts: sug.Users,
		Breakdowns: map[string][]Breakdown{}}
	for _, c := range sug.Cells {
		cells = append(cells, costsOf(c, ""))
		rep.Requests += c.Requests
	}
	// THE CEILING MUST BE ONE. If any arm beats optimal in a cell, every "of optimal" figure is
	// wrong, so the run stops rather than publish them. (It did, on CG2, before core's NewOptimal
	// learned to skip the rejected no-op rows Simulate skips.)
	for _, c := range sug.Cells {
		if !c.OptimalKnown {
			continue
		}
		for _, s := range c.Candidates {
			if s.StrategyUSD < c.OptimalUSD-0.01 {
				return nil, fmt.Errorf("%s at %02d:00 UTC: %s costs $%.4f, below the optimum's $%.4f; the "+
					"ceiling is not a ceiling on this history, so no comparison against it can be reported",
					c.User, c.HourUTC, s.Strategy, s.StrategyUSD, c.OptimalUSD)
			}
		}
	}
	rep.Policies = policies(cells)
	// What the campaign would save if every simulated arm could run live. Same cells, same baseline.
	var baseUSD float64
	for _, c := range cells {
		baseUSD += c.base
	}
	if simBest < rep.Policies[0].CostUSD-1e-9 {
		rep.Policies = append(rep.Policies, withShare(Policy{Name: "campaign-if-every-arm-were-live", Kind: "simulated",
			CostUSD: simBest, SavingUSD: baseUSD - simBest, SavingPct: pct(baseUSD-simBest, baseUSD)}, rep.Policies))
	}
	models := map[string][]string{}
	for _, u := range sug.Users {
		if ms, err := in.DB.KVCacheModels(dash.Filter{Tenant: u, Since: in.Since, Until: in.Until}); err == nil {
			models[u] = ms
		}
	}
	rep.Campaign = summarise(sug, models)
	rep.Campaign.Repicked, rep.Campaign.RepickCostUSD = changed, lost
	if led, err := in.DB.KeepAliveLedger(f); err == nil {
		rep.Current = led
	}

	// Per-cell winners on a finer key: what knowing the weekday (or model, or agent) would add.
	for _, seg := range segmenters() {
		segs, err := in.DB.KVCacheSuggestBy(f, dash.KVCacheOptions{}, in.Pricer, cfg, seg.fn)
		if err != nil {
			return nil, fmt.Errorf("suggest by %s: %w", seg.name, err)
		}
		var sc []cellCosts
		for _, c := range segs.Cells {
			sc = append(sc, costsOf(c.KVCacheSuggestion, c.Segment))
		}
		base, win := 0.0, 0.0
		for _, c := range sc {
			base += c.base
			win += c.winUSD
		}
		rep.Policies = append(rep.Policies, withShare(Policy{Name: "campaign-by-" + seg.name,
			Kind: "segmented", CostUSD: win, SavingUSD: base - win, SavingPct: pct(base-win, base)}, rep.Policies))
		rep.Breakdowns[seg.name] = segmentBreakdown(sc, winners(cells))
	}
	sort.Slice(rep.Policies, func(i, j int) bool { return rep.Policies[i].CostUSD < rep.Policies[j].CostUSD })

	rep.Breakdowns["account"] = breakdown(cells, func(c cellCosts) string { return c.user })
	rep.Breakdowns["hour_utc"] = breakdown(cells, func(c cellCosts) string { return fmt.Sprintf("%02d", c.hour) })

	if in.Holdout > 0 && in.Holdout < 1 {
		// SPLIT THE HISTORY THAT EXISTS, not the window asked for. Retention keeps far less than a
		// 30-day -days on a busy deployment (CG2 held 6.7 days), and a split of the asked-for window
		// then put every row in the test side: an empty train window and a holdout that says nothing.
		lo, hi, err := span(in.DB, f)
		if err != nil {
			return nil, fmt.Errorf("holdout: %w", err)
		}
		rep.HistorySince, rep.HistoryUntil = lo, hi
		cut := hi - int64(float64(hi-lo)*in.Holdout)
		ho, err := in.DB.KVCacheSuggestHoldout(dash.Filter{TenantAll: true}, dash.KVCacheOptions{}, in.Pricer, cfg,
			dash.Window{Since: lo, Until: cut}, dash.Window{Since: cut, Until: hi + 1})
		if err != nil {
			return nil, fmt.Errorf("holdout: %w", err)
		}
		rep.Holdout = ho
	}
	rep.Notes = notes(rep)
	return &Result{Campaign: upload(sug), Report: rep}, nil
}

// MaxUpload is the campaign create route's request ceiling (proxy's maxControlBody, 256 KiB), less
// room for the name and the envelope the Campaigns tab wraps the file in.
const MaxUpload = 240 << 10

// enforceableWinners re-picks each cell's winner among the arms a campaign can run live.
//
// Core's suggester picks over every simulated arm, including ones with no live enforcement path
// (keepalive-budget, historical-probability). A cell whose winner is one of those is RECORDED by the
// create route but never activated, so it keeps the baseline while the campaign claims its saving.
// Re-picking from the cell's own Candidates — core's own per-arm costs, nothing recomputed — gives
// the best arm the dashboard will actually run. The report keeps both figures.
func enforceableWinners(sug *dash.KVCacheSuggestions) (lost float64, changed int) {
	for i := range sug.Cells {
		c := &sug.Cells[i]
		if proxy.CampaignArmEnforceable(c.BestStrategy) || c.BestStrategy == "" {
			continue
		}
		best, found := kvcache.Savings{}, false
		for _, s := range c.Candidates {
			if !proxy.CampaignArmEnforceable(s.Strategy) {
				continue
			}
			if !found || s.AbsoluteUSD > best.AbsoluteUSD {
				best, found = s, true
			}
		}
		if !found {
			continue
		}
		lost += c.SavingUSD - best.AbsoluteUSD
		changed++
		c.BestStrategy, c.BestUSD, c.SavingUSD, c.SavingPct, c.SavingKnown = best.Strategy, best.StrategyUSD,
			best.AbsoluteUSD, best.PercentUSD, best.Known
		c.BestDescription = "Best arm the campaign can enforce live; the simulated best had no live path."
	}
	return lost, changed
}

// upload is the suggest payload without each cell's per-arm Candidates. The create route reads only
// the winner, its saving and the cell's flags; the per-arm costs are in the report. A full payload
// for CG2's 121 cells was 509 KiB, and the route refused it as "request body too large".
func upload(sug *dash.KVCacheSuggestions) *dash.KVCacheSuggestions {
	out := *sug
	out.Cells = make([]dash.KVCacheSuggestion, len(sug.Cells))
	for i, c := range sug.Cells {
		c.Candidates = nil
		out.Cells[i] = c
	}
	return &out
}

// policies is the campaign, every arm everywhere, and the ceiling, over the same cells.
func policies(cells []cellCosts) []Policy {
	var base, win, opt float64
	armUSD := map[string]float64{}
	armCells := map[string]int{}
	for _, c := range cells {
		base += c.base
		win += c.winUSD
		opt += c.optUSD
		for a, usd := range c.arms {
			armUSD[a] += usd
			armCells[a]++
		}
	}
	out := []Policy{
		{Name: "campaign", Kind: "campaign", CostUSD: win},
		{Name: kvcache.StrategyOptimal, Kind: "ceiling", CostUSD: opt},
	}
	for a, usd := range armUSD {
		if armCells[a] != len(cells) {
			continue // an arm missing from some cell would be a total over a different population
		}
		kind := "arm"
		if a == kvcache.StrategyObserved {
			kind = "observed"
		}
		out = append(out, Policy{Name: a, Kind: kind, CostUSD: usd})
	}
	for i := range out {
		out[i].SavingUSD = base - out[i].CostUSD
		out[i].SavingPct = pct(out[i].SavingUSD, base)
		if ceil := base - opt; ceil > 0 {
			out[i].OfOptimalPct = out[i].SavingUSD / ceil * 100
		}
	}
	return out
}

// withShare fills OfOptimalPct from the ceiling already in ps.
func withShare(p Policy, ps []Policy) Policy {
	for _, q := range ps {
		if q.Kind == "ceiling" && q.SavingUSD > 0 {
			p.OfOptimalPct = p.SavingUSD / q.SavingUSD * 100
		}
	}
	return p
}

func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}

// breakdown sums the cells into slices.
func breakdown(cells []cellCosts, key func(cellCosts) string) []Breakdown {
	type acc struct {
		Breakdown
		arms map[string]float64
	}
	by := map[string]*acc{}
	for _, c := range cells {
		k := key(c)
		a := by[k]
		if a == nil {
			a = &acc{Breakdown: Breakdown{Key: k, CampaignArms: map[string]int{}}, arms: map[string]float64{}}
			by[k] = a
		}
		a.Requests += c.requests
		a.BaselineUSD += c.base
		a.CampaignUSD += c.winUSD
		a.OptimalUSD += c.optUSD
		a.ObservedUSD += c.arms[kvcache.StrategyObserved]
		a.CampaignArms[c.winner]++
		for arm, usd := range c.arms {
			a.arms[arm] += usd
		}
	}
	out := make([]Breakdown, 0, len(by))
	for _, a := range by {
		a.BestArm, a.BestArmUSD = cheapest(a.arms)
		out = append(out, a.Breakdown)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// segmentBreakdown slices by segment. The CAMPAIGN column is the uploaded campaign's own arm for
// that (account, hour) — not the segment's own winner — priced on the segment's rows, which core
// already did (it is one of the segment cell's Candidates).
func segmentBreakdown(sc []cellCosts, campaignArm map[[2]any]string) []Breakdown {
	for i := range sc {
		arm := campaignArm[[2]any{sc[i].user, sc[i].hour}]
		if usd, ok := sc[i].arms[arm]; ok {
			sc[i].winner, sc[i].winUSD = arm, usd
		} else {
			sc[i].winner, sc[i].winUSD = "(baseline)", sc[i].base
		}
	}
	return breakdown(sc, func(c cellCosts) string { return c.segment })
}

func winners(cells []cellCosts) map[[2]any]string {
	out := map[[2]any]string{}
	for _, c := range cells {
		out[[2]any{c.user, c.hour}] = c.winner
	}
	return out
}

func cheapest(arms map[string]float64) (string, float64) {
	name, best := "", 0.0
	for a, usd := range arms {
		if a == kvcache.StrategyObserved {
			continue // what ran is not a policy to pick
		}
		if name == "" || usd < best || (usd == best && a < name) {
			name, best = a, usd
		}
	}
	return name, best
}

// summarise counts the upload's cells exactly as the dashboard's campaign create will treat them:
// proxy.CampaignCellOutcome is that route's own decision. modelsByTenant feeds its 1h-tier check.
func summarise(sug *dash.KVCacheSuggestions, modelsByTenant map[string][]string) CampaignSummary {
	honors := func(tenantID string) bool {
		ms := modelsByTenant[tenantID]
		for _, m := range ms {
			if !proxy.HeadTTL1hHonored(m) {
				return false
			}
		}
		return len(ms) > 0
	}
	cs := CampaignSummary{ByArm: map[string]int{}, NotEnforced: map[string]int{}}
	for _, c := range sug.Cells {
		if c.User == "" {
			continue // the create route drops these too
		}
		cs.Cells++
		cs.ByArm[c.BestStrategy]++
		if ok, reason := proxy.CampaignCellOutcome(c, honors); !ok {
			cs.NotEnforced[reason]++
			continue
		}
		cs.Enforceable++
		cs.PredictedSavingUSD += c.SavingUSD
	}
	return cs
}

// segmenter is one finer key for KVCacheSuggestBy. Every one keeps the Sunday-Thursday filter, so
// a segmented total is over the same rows as the campaign's.
type segmenter struct {
	name string
	fn   dash.Segmenter
}

func segmenters() []segmenter {
	inWeek := func(r *kvcache.Request) bool {
		d := time.UnixMilli(r.TS).UTC().Weekday()
		return d != time.Friday && d != time.Saturday
	}
	return []segmenter{
		{"weekday", func(r *kvcache.Request) (string, bool) {
			return time.UnixMilli(r.TS).UTC().Weekday().String(), inWeek(r)
		}},
		{"model", func(r *kvcache.Request) (string, bool) { return r.Model, inWeek(r) }},
		{"agent", func(r *kvcache.Request) (string, bool) { return r.Agent, inWeek(r) }},
	}
}

// span is the oldest and newest request in the filter, from the same dataset read the suggester uses.
func span(db *dash.DB, f dash.Filter) (lo, hi int64, err error) {
	rows, _, err := db.KVCacheDataset(f, dash.KVCacheOptions{})
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, fmt.Errorf("no requests in the window")
	}
	lo, hi = rows[0].TS, rows[0].TS
	for _, r := range rows {
		lo, hi = min(lo, r.TS), max(hi, r.TS)
	}
	return lo, hi, nil
}
