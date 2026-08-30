package dash

import (
	"context"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The volatile-tail split's value on requests that PREDATE the instrumentation for it.
//
// Computed on read, never written. The two facts the live figure needs — the size of the half
// the split moved, and the identity of the tail it moved off — are recorded per request only
// since that instrumentation shipped, so every earlier row prices at $0.00, which on the page
// is indistinguishable from "the component did nothing". This answers what it actually earned
// over the stored history without touching a single stored row: history stays exactly as it was
// recorded, and the estimate is recomputed from it on every query.
//
// What makes a retroactive value legitimate rather than a guess: the stable half is a property
// of the AGENT'S SYSTEM PROMPT, not of the request, and it is measurably constant. Per model on
// this deployment the recorded minimum and maximum are the same number (5,697 / 2,256 / 2,175 /
// 1,634 tokens) — see CachesplitSizeSpread, which is how an operator checks that rather than
// taking it on trust.
//
// Three limits, all of them in the direction of under-crediting:
//
//   - Only the SESSION'S FIRST request. Mid-session the credit has to know whether the snapshot
//     had moved, and that comparison needs the tail hash these rows do not carry. A first
//     request needs no comparison: there was nothing there to match.
//   - Only models whose stable half was actually measured. No service-wide median stands in for
//     a model nobody has run.
//   - The same read/write test as the live path: the provider must have read at least the
//     stable half from cache while writing less than it.
//
// Priced through the same Pricer the request path uses, so a gateway's own rates apply.

// CachesplitHistorical is what the split earned before it was instrumented, and how much of the
// window that covers.
type CachesplitHistorical struct {
	// USD is the value, and Requests the number of session-first requests behind it.
	USD      float64 `json:"usd"`
	Requests int64   `json:"requests"`
	// Models is how many models contributed. Zero means nothing could be valued — either no
	// model has a measured stable half yet, or none is priced.
	Models int `json:"models"`
	// Uncovered counts pre-instrumentation session-first requests that could NOT be valued
	// because their model has no measured stable half. Reported so the figure is never
	// mistaken for complete.
	Uncovered int64 `json:"uncovered"`
}

// CachesplitHistoricalUSD values the pre-instrumentation window. Read-only.
func (d *DB) CachesplitHistoricalUSD(f Filter, p modelinfo.Pricer) (CachesplitHistorical, error) {
	var out CachesplitHistorical
	if d == nil || p == nil {
		return out, nil
	}
	sizes, err := d.CachesplitSizeSpread()
	if err != nil {
		return out, err
	}
	cond, args := f.where()
	// One pass: every pre-instrumentation session-first request, by model, with the read and
	// write it was billed. The read/write test needs that model's stable half, which is known
	// only here in Go, so the rows come back grouped and the test is applied per group.
	//
	// A window function, not the correlated NOT EXISTS this used to be (same fix as
	// DB.Overview's CompactionResets, see its comment): split_stable_tokens = 0 AND cache_read
	// > 0 matches 69% of requests on this corpus, so a correlated subquery per candidate row
	// paid this driver's per-invocation overhead ~79k times over instead of sorting once.
	// ROW_NUMBER ranks EVERY row in the table by session, unfiltered, so rn=1 means exactly
	// what the old subquery's NOT EXISTS meant — "nothing earlier in this session, filtered or
	// not" — before the outer WHERE narrows to the rows this query actually values.
	rows, err := d.sql.Query(`WITH s AS (
		SELECT r.*, ROW_NUMBER() OVER (PARTITION BY r.session_id ORDER BY r.ts, r.id) AS rn
		FROM requests r
	) SELECT r.model, r.cache_read, r.cache_write FROM s r
		WHERE `+cond+` AND r.split_stable_tokens = 0 AND r.cache_read > 0 AND r.rn = 1`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	priced := map[string]float64{} // model -> value of one qualifying request
	seen := map[string]bool{}
	for rows.Next() {
		var model string
		var read, write int64
		if err := rows.Scan(&model, &read, &write); err != nil {
			return out, err
		}
		sp, ok := sizes[model]
		if !ok {
			out.Uncovered++
			continue
		}
		stable := int64(sp[1])
		if read < stable || write >= stable {
			continue // the stable half was not what the provider served from cache
		}
		v, done := priced[model]
		if !done {
			price, ok := p.Price(context.Background(), model)
			if !ok || price.Zero() {
				out.Uncovered++
				continue
			}
			miss := price.CacheWrite
			if miss < price.Input {
				miss = price.Input
			}
			if delta := miss - price.CacheRead; delta > 0 {
				v = float64(stable) * delta
			}
			priced[model] = v
		}
		if v <= 0 {
			out.Uncovered++
			continue
		}
		out.USD += v
		out.Requests++
		seen[model] = true
	}
	out.Models = len(seen)
	return out, rows.Err()
}

// CachesplitHistoricalUSDByTenant is CachesplitHistoricalUSD for every tenant since a given
// time, in one pass instead of one call per tenant.
//
// The Prometheus exporter used to call CachesplitHistoricalUSD once per tenant. Each call's
// ROW_NUMBER ranks the WHOLE unfiltered requests table (that's what makes it correct — see
// CachesplitHistoricalUSD's comment), so calling it N times re-sorted that same table N
// times over: on this deployment, 19 tenants at ~0.8s a sort, every single scrape. Tenant is
// only ever the outer filter here, never part of the session-first ranking itself, so it is
// safe to compute the ranking once and group the qualifying rows by tenant afterward.
func (d *DB) CachesplitHistoricalUSDByTenant(since int64, p modelinfo.Pricer) (map[string]CachesplitHistorical, error) {
	out := map[string]CachesplitHistorical{}
	if d == nil || p == nil {
		return out, nil
	}
	sizes, err := d.CachesplitSizeSpread()
	if err != nil {
		return out, err
	}
	cond, args := (Filter{Since: since, TenantAll: true}).where()
	rows, err := d.sql.Query(`WITH s AS (
		SELECT r.*, ROW_NUMBER() OVER (PARTITION BY r.session_id ORDER BY r.ts, r.id) AS rn
		FROM requests r
	) SELECT r.tenant_id, r.model, r.cache_read, r.cache_write FROM s r
		WHERE `+cond+` AND r.split_stable_tokens = 0 AND r.cache_read > 0 AND r.rn = 1`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	acc := map[string]*CachesplitHistorical{}
	seen := map[string]map[string]bool{}
	priced := map[string]float64{} // model -> value of one qualifying request, shared across tenants
	for rows.Next() {
		var tenant, model string
		var read, write int64
		if err := rows.Scan(&tenant, &model, &read, &write); err != nil {
			return out, err
		}
		h, ok := acc[tenant]
		if !ok {
			h = &CachesplitHistorical{}
			acc[tenant] = h
			seen[tenant] = map[string]bool{}
		}
		sp, ok := sizes[model]
		if !ok {
			h.Uncovered++
			continue
		}
		stable := int64(sp[1])
		if read < stable || write >= stable {
			continue // the stable half was not what the provider served from cache
		}
		v, done := priced[model]
		if !done {
			price, ok := p.Price(context.Background(), model)
			if !ok || price.Zero() {
				h.Uncovered++
				continue
			}
			miss := price.CacheWrite
			if miss < price.Input {
				miss = price.Input
			}
			if delta := miss - price.CacheRead; delta > 0 {
				v = float64(stable) * delta
			}
			priced[model] = v
		}
		if v <= 0 {
			h.Uncovered++
			continue
		}
		h.USD += v
		h.Requests++
		seen[tenant][model] = true
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	for tenant, h := range acc {
		h.Models = len(seen[tenant])
		out[tenant] = *h
	}
	return out, nil
}

// CachesplitSizeSpread reports the recorded minimum and maximum stable half per model. The
// estimate above rests on those being equal; this is how that gets checked instead of assumed.
func (d *DB) CachesplitSizeSpread() (map[string][2]int, error) {
	out := map[string][2]int{}
	rows, err := d.sql.Query(`SELECT model, MIN(split_stable_tokens), MAX(split_stable_tokens)
		FROM requests WHERE split_stable_tokens > 0 GROUP BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		var lo, hi int
		if err := rows.Scan(&m, &lo, &hi); err != nil {
			return nil, err
		}
		out[m] = [2]int{lo, hi}
	}
	return out, rows.Err()
}
