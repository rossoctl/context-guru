package dash

import (
	"context"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Read-time valuation of figures the store recorded without pricing.
//
// Same principle as CachesplitHistoricalUSD next door: the store holds absolutes, the rates
// live outside it, and a dollar figure is therefore computed on every query rather than
// frozen into a row. Two things are valued here.
//
// 1. request_components.saved_usd on rows that PREDATE the column. It is an additive column
// (schema.go additiveColumns), so every row written before it shipped reads exactly 0.00 —
// and on a live deployment that is essentially the whole table: the column arrived with a
// restart, and only requests served after that restart carry it. Measured on production,
// 6 rows of 100,579. The per-component view is the most-read page in the dashboard and it
// said $0.00 for every component over all visible history, which reads as "this product is
// worthless" rather than "this column is younger than these rows".
//
// The estimate is not a model of anything: it is the SAME arithmetic the write path runs
// (Event.Price), over inputs the row already carries — saved_gross, saved_unique, and the
// request's own billed tiers. The unique part at the cache-write rate it would have entered
// as, the re-sent remainder at the tier that request actually paid (Event.repeatRate). It is
// reported in its OWN field, never merged into saved_usd, so a stored figure and an estimated
// one are never confused; rows whose accounting was incomplete, or whose model has no rate,
// are counted as uncovered rather than valued at zero.
//
// 2. The bill split by tier. Nothing stores per-tier cost, only per-tier TOKENS, so "how much
// of this bill could compaction ever have touched?" was unanswerable — and every savings
// percentage on the page was therefore divided by a bill that is mostly OUTPUT tokens, which
// no input-side transformation can reach. Measured on production: 67% of the bill is output,
// so a saving reported as 0.28% of spend is 0.87% of the spend it could address.

// TierCosts is the filtered window's bill split into the four tiers the provider charges on,
// priced on read. Absent (nil) rather than zeroed when no rates are available.
type TierCosts struct {
	FreshUSD      float64 `json:"fresh_usd"`
	CacheReadUSD  float64 `json:"cache_read_usd"`
	CacheWriteUSD float64 `json:"cache_write_usd"`
	OutputUSD     float64 `json:"output_usd"`
	// AddressableUSD is the INPUT side — fresh + cache read + cache write. It is the only
	// part of the bill a context transformation can act on, and so the only denominator a
	// compaction saving may honestly be expressed as a percentage of. Output tokens are the
	// model's own generation; removing transcript content does not shorten them.
	AddressableUSD float64 `json:"addressable_usd"`
	// TotalUSD is all four tiers at today's rates. StoredUSD is SUM(cost_usd) over the same
	// rows, priced when each request was served. They are shown together on purpose: the gap
	// between them is rate drift over the window, and a split that silently disagreed with
	// the billed total would be worse than no split at all.
	TotalUSD  float64 `json:"total_usd"`
	StoredUSD float64 `json:"stored_usd"`
	// FrozenReadUSD prices the tokens cache-aware compaction deliberately left alone at the
	// cache-READ rate they were actually billed at. This is the benefit half of SafetyCost,
	// which the panel has always promised ("its benefit is the cache reads it preserved") and
	// never computed. FrozenWriteRiskUSD is what re-creating that same prefix instead would
	// have ADDED — the spread between the write and read rates, i.e. what the freeze bought.
	FrozenReadUSD      float64 `json:"frozen_read_usd"`
	FrozenWriteRiskUSD float64 `json:"frozen_write_risk_usd"`
	// Requests is how many priced requests are behind these figures; Uncovered is how many
	// were left out because their model has no rate or their accounting was incomplete.
	Requests  int64 `json:"requests"`
	Uncovered int64 `json:"uncovered_requests"`
}

// TierCosts prices the window by tier. Read-only, one row per model.
func (d *DB) TierCosts(f Filter, p modelinfo.Pricer) (*TierCosts, error) {
	if d == nil || p == nil {
		return nil, nil
	}
	cond, args := f.where()
	// Incomplete accounting is excluded, not priced as zero: Event.Price refuses to price
	// such a request at all, and an estimate that quietly included them would report a
	// smaller bill than the one that was billed.
	rows, err := d.sql.Query(`SELECT r.model, COUNT(*),
		COALESCE(SUM(r.fresh_input),0), COALESCE(SUM(r.cache_read),0), COALESCE(SUM(r.cache_write),0),
		COALESCE(SUM(r.output_tokens),0), COALESCE(SUM(r.frozen_tokens),0), COALESCE(SUM(r.cost_usd),0)
		FROM requests r WHERE `+cond+` AND r.token_accounting = 'complete' GROUP BY r.model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &TierCosts{}
	any := false
	for rows.Next() {
		var model string
		var n, fresh, read, write, output, frozen int64
		var stored float64
		if err := rows.Scan(&model, &n, &fresh, &read, &write, &output, &frozen, &stored); err != nil {
			return nil, err
		}
		price, ok := p.Price(context.Background(), model)
		if !ok || price.Zero() {
			out.Uncovered += n
			continue
		}
		any = true
		out.Requests += n
		out.FreshUSD += float64(fresh) * price.Input
		out.CacheReadUSD += float64(read) * price.CacheRead
		out.CacheWriteUSD += float64(write) * price.CacheWrite
		out.OutputUSD += float64(output) * price.Output
		out.FrozenReadUSD += float64(frozen) * price.CacheRead
		if spread := price.CacheWrite - price.CacheRead; spread > 0 {
			out.FrozenWriteRiskUSD += float64(frozen) * spread
		}
		out.StoredUSD += stored
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !any {
		return nil, nil // absent, not a bill of zero
	}
	out.AddressableUSD = out.FreshUSD + out.CacheReadUSD + out.CacheWriteUSD
	out.TotalUSD = out.AddressableUSD + out.OutputUSD
	return out, nil
}

// EstimateComponentSavedUSD fills SavedUSDEstimated on component rows whose stored saved_usd
// predates the column, and recomputes NetUSDWithEstimate. Rows that already carry a stored
// figure are untouched, so the estimate can only ever ADD to history and never restate the
// present. Nothing happens without a Pricer — an unpriced deployment keeps reading 0.00,
// which is the honest answer there.
func (d *DB) EstimateComponentSavedUSD(f Filter, p modelinfo.Pricer, out []*ComponentRow) error {
	if d == nil || p == nil || len(out) == 0 {
		return nil
	}
	by := make(map[string]*ComponentRow, len(out))
	for _, c := range out {
		by[c.Component] = c
	}
	cond, args := f.where()
	// The clamps are Event.Price's, in SQL: a negative gross is nothing saved, and a unique
	// figure larger than this turn's gross is two components stashing the same content key,
	// which must not be allowed to make the replay term negative. min()/max() with two
	// arguments are SQLite's scalar forms, not the aggregates.
	//
	// The tier bucket is Event.repeatRate's three cases, evaluated per request and grouped,
	// so the estimate is priced at the tier each request actually paid rather than at a
	// window-wide average that would flatter warm traffic.
	const gross = `max(c.saved_gross,0)`
	const uniq = `min(max(c.saved_unique,0), max(c.saved_gross,0))`
	rows, err := d.sql.Query(`SELECT c.component, r.model,
		CASE WHEN r.cache_read > 0 THEN 'read'
		     WHEN r.cache_write > 0 AND r.cache_write >= r.fresh_input THEN 'write'
		     ELSE 'fresh' END,
		COUNT(*), COALESCE(SUM(`+uniq+`),0), COALESCE(SUM(`+gross+` - `+uniq+`),0)
		FROM request_components c JOIN requests r ON r.id = c.request_id
		WHERE `+cond+` AND c.saved_usd = 0 AND c.saved_gross > 0
		  AND r.token_accounting = 'complete'
		GROUP BY 1, 2, 3`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, model, tier string
		var n, unique, replay int64
		if err := rows.Scan(&name, &model, &tier, &n, &unique, &replay); err != nil {
			return err
		}
		c, ok := by[name]
		if !ok {
			continue
		}
		price, priced := p.Price(context.Background(), model)
		if !priced || price.Zero() {
			c.SavedUSDUnpricedRows += n
			continue
		}
		rate := price.Input
		switch tier {
		case "read":
			rate = price.CacheRead
		case "write":
			rate = price.CacheWrite
		}
		c.SavedUSDEstimated += float64(unique)*price.CacheWrite + float64(replay)*rate
		c.SavedUSDEstimatedRows += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Rows that removed tokens but whose request was never priced at all. Counted, not
	// valued: "we cannot say" and "it was worth nothing" are different answers.
	urows, err := d.sql.Query(`SELECT c.component, COUNT(*)
		FROM request_components c JOIN requests r ON r.id = c.request_id
		WHERE `+cond+` AND c.saved_usd = 0 AND c.saved_gross > 0
		  AND r.token_accounting <> 'complete' GROUP BY 1`, args...)
	if err != nil {
		return err
	}
	defer urows.Close()
	for urows.Next() {
		var name string
		var n int64
		if err := urows.Scan(&name, &n); err != nil {
			return err
		}
		if c, ok := by[name]; ok {
			c.SavedUSDUnpricedRows += n
		}
	}
	if err := urows.Err(); err != nil {
		return err
	}
	for _, c := range out {
		c.NetUSDWithEstimate = c.SavedUSD + c.SavedUSDEstimated - c.LLMCostUSD
	}
	return nil
}
