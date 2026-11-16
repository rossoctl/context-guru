package dash

// The requests the accountant library could not price (requests.acc_fallback, #1004). Each books its cost, and
// it can still book some saving that the library did not price: the deferral ledger's net, the measured filter
// credit, and minus its own model-call cost (savingsArithmetic's net is baseline - cost - cg_llm, and a
// fallback row has baseline = cost). The Overview share leaves these requests out on both sides. So the reads
// return their cost and their saving, part by part, and the UI takes both off.

// FallbackSaving is the saving the fallback requests booked, by the Overview part it lands in. The filter
// part is a read-time credit; SetDeclCredit (full read) and OverviewLean fill it.
type FallbackSaving struct {
	CompactionUSD float64 `json:"compaction_usd"` // baseline - cost - cg_llm: the net_saved_usd part
	DeferUSD      float64 `json:"defer_usd"`      // the deferral ledger's net: the defer_net_usd part
	KeepAliveUSD  float64 `json:"keepalive_usd"`  // the keep-alive credit: the keepalive_saved_usd part
	ReorderUSD    float64 `json:"reorder_usd"`    // cachesplit_saved_usd: the cache reorder part
	FilterUSD     float64 `json:"filter_usd"`     // the measured filter credit: the decl_filter_usd part
}

// fallbackTerms is the SQL for a fallbackAgg: 6 sums over the rows guard selects ("" for every row the query
// reads). since is the window start, for the keep-alive credit (kaSavedSince).
func fallbackTerms(guard string, since int64) string {
	col := func(x string) string {
		if guard == "" {
			return "COALESCE(SUM(" + x + "),0)"
		}
		return "COALESCE(SUM(CASE WHEN " + guard + " THEN " + x + " ELSE 0 END),0)"
	}
	return col("1") + ", " + col("r.cost_usd + r.cg_llm_cost_usd") + ", " +
		col("r.baseline_cost_usd - r.cost_usd - r.cg_llm_cost_usd") + ", " + col(deferNetCol) + ", " +
		col(kaSavedSince("r.", since)) + ", " + col("r.cachesplit_saved_usd")
}

// fallbackAgg is one fallbackTerms read.
type fallbackAgg struct {
	n    int64
	cost float64
	FallbackSaving
}

func (a *fallbackAgg) dest() []any {
	return []any{&a.n, &a.cost, &a.CompactionUSD, &a.DeferUSD, &a.KeepAliveUSD, &a.ReorderUSD}
}

func (a *fallbackAgg) add(b fallbackAgg) {
	a.n += b.n
	a.cost += b.cost
	a.CompactionUSD += b.CompactionUSD
	a.DeferUSD += b.DeferUSD
	a.KeepAliveUSD += b.KeepAliveUSD
	a.ReorderUSD += b.ReorderUSD
}

// apply sets the Overview's fallback fields. The filter part is not in the SQL, so it is kept.
func (a fallbackAgg) apply(o *Overview) {
	filter := o.FallbackSaved.FilterUSD
	o.FallbackRequests, o.FallbackCostUSD, o.FallbackSaved = a.n, a.cost, a.FallbackSaving
	o.FallbackSaved.FilterUSD = filter
}
