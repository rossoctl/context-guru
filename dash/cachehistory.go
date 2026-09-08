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
// What would make a retroactive value legitimate rather than a guess: the stable half is a
// property of the AGENT'S SYSTEM PROMPT, not of the request, so where it is measurably constant
// for a model, the constant may stand in for the rows that never recorded it.
//
// It is NOT measurably constant. This comment used to assert that per model the recorded minimum
// and maximum are the same number; that was checked with CachesplitSizeSpread — the very query
// written so an operator would not have to take it on trust — and it is false on every corpus
// large enough to test: 7 of 9 models on production traffic have a spread, the widest 5.5x
// (1,301..7,190 tokens). So this estimate no longer values those models at all. Where the spread
// is real the request is counted in Uncovered and skipped, which is what Uncovered is for: "we
// cannot value this model's history" is an answer, and picking an end of a 5.5x spread is not.
//
// Why not simply pick the conservative end. `stable` is not only a price here, it is also the
// GATE (read >= stable > write): a smaller constant admits far more rows, a larger one admits
// far fewer, and the two roles want opposite ends. On production traffic the two ends differ by
// 31x in what they credit — MIN admits 280 qualifying requests, MAX admits 11 — so any choice
// is a 31x swing on a dollar figure resting on a premise that has been disproved. Refusing is
// the only reading of that spread that does not invent one.
//
// Four limits, all of them in the direction of under-crediting:
//
//   - Only models whose stable half is actually CONSTANT. A model whose recorded minimum and
//     maximum disagree has no single stable half to retro-apply, and none is guessed for it.
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
	// Models is how many models contributed. Zero means nothing could be valued — no model has
	// a measured stable half yet, none has a CONSTANT one, or none is priced.
	Models int `json:"models"`
	// Uncovered counts pre-instrumentation session-first requests that could NOT be valued —
	// their model has no measured stable half, or the one it has is not a single number
	// (recorded min != max, so there is nothing constant to retro-apply). Reported so the
	// figure is never mistaken for complete.
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
	// The ranking CTE emits (id, rn) and is joined back on the primary key, rather than pulling
	// `r.*` through the PARTITION BY sort for the sake of one integer per row. Same rewrite as
	// CompactionResets in overview.go, same reason, and the same caveat: the CTE stays UNFILTERED,
	// which is the whole correctness argument above — rn=1 has to mean "nothing earlier in this
	// session, filtered or not", so the filter may only ever be applied outside it.
	//
	// Measured: 199-208 ms to 38-44 ms on the 16,444-request corpus, and 1,103 ms to 270 ms
	// read-only against the production database. Results identical, checked on the 6,697 rows
	// this predicate actually returns there and on 16,167 rows with the split/cache filters
	// dropped, plus each tenant scope separately -- the frozen corpus returns ZERO rows for the
	// shipped predicate, so a check against it alone proves nothing.
	rows, err := d.sql.QueryContext(d.readCtx(), `WITH rn AS (
		SELECT r.id AS rid, ROW_NUMBER() OVER (PARTITION BY r.session_id ORDER BY r.ts, r.id) AS n
		FROM requests r
	) SELECT r.model, r.cache_read, r.cache_write FROM requests r JOIN rn ON rn.rid = r.id AND rn.n = 1
		WHERE `+cond+` AND r.split_stable_tokens = 0 AND r.cache_read > 0`, args...)
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
		if !ok || sp[0] != sp[1] {
			// No measured stable half, or no CONSTANT one. See the file comment: min != max
			// on most models here, and the two ends of that spread differ by 31x in what they
			// credit, so neither end may stand in for the rows that recorded nothing.
			out.Uncovered++
			continue
		}
		stable := int64(sp[0])
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
	// The ranking CTE emits (id, rn) and is joined back on the primary key, rather than pulling
	// `r.*` through the PARTITION BY sort for the sake of one integer per row. Same rewrite as
	// CompactionResets in overview.go, same reason, and the same caveat: the CTE stays UNFILTERED,
	// which is the whole correctness argument above — rn=1 has to mean "nothing earlier in this
	// session, filtered or not", so the filter may only ever be applied outside it.
	//
	// Measured: 199-208 ms to 38-44 ms on the 16,444-request corpus, and 1,103 ms to 270 ms
	// read-only against the production database. Results identical, checked on the 6,697 rows
	// this predicate actually returns there and on 16,167 rows with the split/cache filters
	// dropped, plus each tenant scope separately -- the frozen corpus returns ZERO rows for the
	// shipped predicate, so a check against it alone proves nothing.
	rows, err := d.sql.QueryContext(d.readCtx(), `WITH rn AS (
		SELECT r.id AS rid, ROW_NUMBER() OVER (PARTITION BY r.session_id ORDER BY r.ts, r.id) AS n
		FROM requests r
	) SELECT r.tenant_id, r.model, r.cache_read, r.cache_write FROM requests r JOIN rn ON rn.rid = r.id AND rn.n = 1
		WHERE `+cond+` AND r.split_stable_tokens = 0 AND r.cache_read > 0`, args...)
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
		if !ok || sp[0] != sp[1] {
			h.Uncovered++ // same refusal as CachesplitHistoricalUSD; see its file comment
			continue
		}
		stable := int64(sp[0])
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
// estimate above credits a model only where those are equal; this is where that gets checked
// instead of assumed, and it is what decides the refusal rather than merely reporting on it.
func (d *DB) CachesplitSizeSpread() (map[string][2]int, error) {
	out := map[string][2]int{}
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT model, MIN(split_stable_tokens), MAX(split_stable_tokens)
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
