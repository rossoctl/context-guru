package dash

import (
	"errors"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// SavingsTotals is the cost/savings arithmetic Overview() and /stats both need — one
// query, one definition, so the two can never disagree about what "total saved" means.
// See Overview's own KeepAlive*/TotalSavedUSD fields for what each addend means; this
// type carries only the subset a caller asking "did this save money" needs, none of the
// SSE/breakpoint/prefix-change columns Overview also computes.
type SavingsTotals struct {
	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	NetSavedUSD     float64 `json:"net_saved_usd"`

	CachesplitSavedUSD float64 `json:"cachesplit_saved_usd"`
	// DeferNetUSD is tool deferral's net saving (see deferNetSum).
	DeferNetUSD float64 `json:"defer_net_usd"`

	KeepAlivePingUSD  float64 `json:"keepalive_ping_usd"`
	KeepAliveSavedUSD float64 `json:"keepalive_saved_usd"`
	KeepAliveNetUSD   float64 `json:"keepalive_net_usd"`

	// ManualPings is the user's own pings, a category of its own (see ManualPings).
	ManualPings ManualPings `json:"manual_pings"`

	TotalSavedUSD float64 `json:"total_saved_usd"`

	// CapCorrectionUSD is what the summarize and replay ledgers moved the baseline by (a
	// negative number: the credit past the cap). Zero unless SavingsTotalsCapped ran and the
	// cap found something. The figures above already include it.
	CapCorrectionUSD float64 `json:"cap_correction_usd"`
	// CapNotApplied is true when the figures above are the stored, uncapped credit: plain
	// SavingsTotals never applies the cap, and SavingsTotalsCapped sets it when the ledgers
	// could not run. Such a figure can be higher than the headline for the same rows (#586).
	CapNotApplied bool `json:"cap_not_applied"`
}

// errCapUnavailable is returned when there is no price table or window resolver to cap against.
var errCapUnavailable = errors.New("dash: replay cap needs a price table and a window resolver")

// SavingsTotals computes the combined savings figure for the filtered window: two
// queries (agent traffic, then ping cost — see Filter.WithKeepAlive for why those
// cannot be one query) rather than Overview's sixteen. Cheap enough to run
// synchronously per request on a single-tenant deployment's own DB, which is the shape
// proxy.Handler.stats() runs against. A caller summing several sessions (e.g. every
// live keep-alive session) should pass Filter.SessionIn rather than looping — the two
// queries above already aggregate across however many session ids the IN-clause names.
func (d *DB) SavingsTotals(f Filter) (*SavingsTotals, error) {
	cond, args := f.where()
	s := &SavingsTotals{}
	// kaSavedSince on the saving line, because the ping COST below is summed inside the same
	// window: a ping just before `since` must not credit a row just after it while its cost
	// stays outside the window (#678).
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT
		COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
		COALESCE(SUM(r.cachesplit_saved_usd),0), COALESCE(SUM(`+kaSavedSince("r.", f.Since)+`),0), `+deferNetSum+`
		FROM requests r WHERE `+cond, args...).Scan(
		&s.CostUSD, &s.BaselineCostUSD, &s.CGLLMCostUSD, &s.CachesplitSavedUSD, &s.KeepAliveSavedUSD, &s.DeferNetUSD)
	if err != nil {
		return nil, err
	}
	kaCond, kaArgs := withKeepAlive(f).where()
	var manual manualAgg
	if err := d.sql.QueryRowContext(d.readCtx(), `SELECT
		COALESCE(SUM(CASE WHEN `+autoPing+` THEN r.cost_usd ELSE 0 END),0), `+manualSelect+`
		FROM requests r WHERE `+kaCond, kaArgs...).Scan(append([]any{&s.KeepAlivePingUSD}, manual.dest()...)...); err != nil {
		return nil, err
	}
	s.ManualPings = manual.pings()
	s.NetSavedUSD, s.KeepAliveNetUSD, s.TotalSavedUSD = savingsArithmetic(
		s.CostUSD, s.BaselineCostUSD, s.CGLLMCostUSD, s.CachesplitSavedUSD, s.KeepAliveSavedUSD, s.KeepAlivePingUSD,
		s.ManualPings.SavedUSD, s.DeferNetUSD)
	return s, nil
}

// savingsArithmetic is the ONE formula behind NetSavedUSD/KeepAliveNetUSD/TotalSavedUSD,
// shared by SavingsTotals and Overview so the two can never disagree about what "total
// saved" means. See Overview's own KeepAliveNetUSD/TotalSavedUSD field comments for why
// each term is signed the way it is.
//
// manualNet is the user's own pings' saved-minus-cost (ManualPings.SavedUSD, already net). It is its own addend and
// not part of kaNet, so the cache strategy's net stays the strategy's.
//
// deferNet is tool deferral's net saving (#888): the deferred definitions are in the top-level
// `tools` array, which tokens_before never counted, so no other addend holds them.
func savingsArithmetic(cost, baseline, cgllm, cachesplitSaved, kaSaved, kaPing, manualNet, deferNet float64) (net, kaNet, total float64) {
	net = baseline - cost - cgllm
	kaNet = kaSaved - kaPing
	total = net + cachesplitSaved + kaNet + manualNet + deferNet
	return net, kaNet, total
}

// CapCorrectionUSD is the correction the headline applies for the capped credit of one filter:
// the summarize ledger's plus the replay ledger's, both negative or zero. It is the one place the
// two ledgers are added, so a reader that cannot take a ledger row by row still lands on the
// headline's figure. Both ledgers stream the rows f selects, so the cost follows f: give it a
// session or a window, not the whole table (#502).
func (d *DB) CapCorrectionUSD(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (float64, error) {
	if windows == nil || p == nil {
		return 0, errCapUnavailable
	}
	sum, err := d.SummarizeLedgerFor(f, windows, p)
	if err != nil {
		return 0, err
	}
	rep, err := d.ReplayLedgerFor(f, windows, p)
	if err != nil {
		return 0, err
	}
	return sum.DeltaUSD() + rep.DeltaUSD(), nil
}

// SavingsTotalsCapped is SavingsTotals with the cap applied, so a scope agrees with the
// headline for the same rows. When the cap cannot be computed the stored figure is returned
// with CapNotApplied set: a reader gets a labelled figure and not an error.
//
// Use it for a scope that one session or one window bounds. A lifetime read (an empty filter) streams
// every row through the ledgers: use SavingsTotals there and say so.
func (d *DB) SavingsTotalsCapped(f Filter, windows modelinfo.Resolver, p modelinfo.Pricer) (*SavingsTotals, error) {
	s, err := d.SavingsTotals(f)
	if err != nil {
		return nil, err
	}
	corr, err := d.CapCorrectionUSD(f, windows, p)
	if err != nil {
		s.CapNotApplied = true
		return s, nil
	}
	s.CapCorrectionUSD = corr
	s.BaselineCostUSD += corr
	s.NetSavedUSD += corr
	s.TotalSavedUSD += corr
	return s, nil
}
