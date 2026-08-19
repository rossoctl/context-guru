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
	rows, err := d.sql.Query(`SELECT r.model, r.cache_read, r.cache_write FROM requests r
		WHERE `+cond+` AND r.split_stable_tokens = 0 AND r.cache_read > 0
		  AND NOT EXISTS (SELECT 1 FROM requests p WHERE p.session_id = r.session_id
		      AND (p.ts < r.ts OR (p.ts = r.ts AND p.id < r.id)))`, args...)
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
