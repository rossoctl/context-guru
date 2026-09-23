package dash

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

	KeepAlivePingUSD  float64 `json:"keepalive_ping_usd"`
	KeepAliveSavedUSD float64 `json:"keepalive_saved_usd"`
	KeepAliveNetUSD   float64 `json:"keepalive_net_usd"`

	TotalSavedUSD float64 `json:"total_saved_usd"`
}

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
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT
		COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
		COALESCE(SUM(r.cachesplit_saved_usd),0), COALESCE(SUM(`+kaSaved("r.")+`),0)
		FROM requests r WHERE `+cond, args...).Scan(
		&s.CostUSD, &s.BaselineCostUSD, &s.CGLLMCostUSD, &s.CachesplitSavedUSD, &s.KeepAliveSavedUSD)
	if err != nil {
		return nil, err
	}
	kaCond, kaArgs := withKeepAlive(f).where()
	if err := d.sql.QueryRowContext(d.readCtx(), `SELECT
		COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN r.cost_usd ELSE 0 END),0)
		FROM requests r WHERE `+kaCond, kaArgs...).Scan(&s.KeepAlivePingUSD); err != nil {
		return nil, err
	}
	s.NetSavedUSD, s.KeepAliveNetUSD, s.TotalSavedUSD = savingsArithmetic(
		s.CostUSD, s.BaselineCostUSD, s.CGLLMCostUSD, s.CachesplitSavedUSD, s.KeepAliveSavedUSD, s.KeepAlivePingUSD)
	return s, nil
}

// savingsArithmetic is the ONE formula behind NetSavedUSD/KeepAliveNetUSD/TotalSavedUSD,
// shared by SavingsTotals and Overview so the two can never disagree about what "total
// saved" means. See Overview's own KeepAliveNetUSD/TotalSavedUSD field comments for why
// each term is signed the way it is.
func savingsArithmetic(cost, baseline, cgllm, cachesplitSaved, kaSaved, kaPing float64) (net, kaNet, total float64) {
	net = baseline - cost - cgllm
	kaNet = kaSaved - kaPing
	total = net + cachesplitSaved + kaNet
	return net, kaNet, total
}
