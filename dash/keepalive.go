package dash

import (
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The keep-alive tab's read side: one ledger, one behavioural read, one session list, one
// calculator and one recommendation — computed here, in Go and SQL, and never in the browser.
//
// # Why the arithmetic is on this side
//
// The browser has twice duplicated a table the server owns and drifted from it: the defaults
// table (R8) and the regex pipeline writer. Money arithmetic is the worst case of that class,
// so the UI on this tab posts inputs and renders answers. Nothing below is recomputed client
// side.
//
// # The one rule none of this may break
//
// PR #86 excludes ping rows from every agent aggregate with a single predicate,
// `r.keepalive = 0` in Filter.where(). A ping is a request context-guru sent while nobody was
// at the keyboard, so counted in COUNT(*), SUM(cache_read) or AVG(upstream_ms) it makes an
// account's own statistics wrong. That predicate does not change here. Where a ping IS the
// subject, this file runs a SECOND query with withKeepAlive(f) and joins in Go — the same
// pattern DB.Overview already uses for the cost half of the ledger. There is no CASE over
// `keepalive` inside an agent-traffic aggregate anywhere in this file.
//
// # Addressability, and why it is not optional
//
// A `ttl_expiry` row with `cache_write = 0` is a PHANTOM: 385 of the 742 in the production
// corpus are exactly that and every one of them is an HTTP 400. They wrote nothing, so there
// was no prefix to protect and no charge a ping could have avoided. `cache_write > 0` is the
// addressability test and it gates every count, every dollar and every percentile on this tab.

// ttlTTL is the provider's default prompt-cache lifetime. The coverage arithmetic rests on it.
const ttlTTL = 300 * time.Second

// addressableCTE is the shared read used by the behaviour panels, the calculator's own history
// and the recommendation. ONE definition, so the panels cannot disagree with each other about
// how many expiries an account had — which is how two tiles on the same page come to report
// different denominators.
//
// The gap is computed at READ time with LAG: there is no `since_last_ms` column, because
// Event.SinceLastMs is transient and was never stored. Window functions are available
// (modernc.org/sqlite v1.56.0) and `idx_requests_session(session_id, ts)` already covers the
// partition.
//
// prev_prefix is the PREVIOUS request's billed prefix (`cache_read + cache_write`), which is
// the size of the entry that lapsed. Not `tokens_before`, which is message text only and runs
// a median 3.38x low.
const addressableCTE = `WITH s AS (
	SELECT r.tenant_id, r.session_id, r.ts, r.cost_usd, r.model, r.cache_miss_reason, r.cache_write,
	       LAG(r.ts) OVER (PARTITION BY r.tenant_id, r.session_id ORDER BY r.ts) AS prev_ts,
	       LAG(r.cache_read + r.cache_write) OVER
	         (PARTITION BY r.tenant_id, r.session_id ORDER BY r.ts) AS prev_prefix
	FROM requests r WHERE %s
), addressable AS (
	SELECT *, (ts - prev_ts) / 1000.0 AS gap_s FROM s
	WHERE cache_miss_reason = 'ttl_expiry' AND cache_write > 0 AND prev_ts IS NOT NULL
)`

// addressable renders the CTE for one filter.
func addressable(f Filter) (string, []any) {
	cond, args := f.where()
	return fmt.Sprintf(addressableCTE, cond), args
}

// kaSaved is keepalive_saved_usd with the REACHABILITY test the write path could not make,
// applied on read.
//
// The credit written at capture time asks four questions (Event.keepaliveSavedUSD) and not one
// of them is an upper bound on the idle gap: it requires that the gap EXCEEDED the provider's
// lifetime — the floor — and then credits the row whatever the gap was, on the strength of
// keepalive_pings alone. That column is the whole of the write path's evidence, and on real rows
// it is not sufficient evidence. On /tmp/cg.db — 133,064 real rows, opened read-only, never
// pruned — 7 of 105 credited rows ($11.81 of $167.28, 7.1%) carry keepalive_pings >= 1 while NO
// ping row exists anywhere in the idle span they ended, two of them with no ping within 16.7
// hours ($152.36 credited after the correction). On rev-keepalive's larger pre-prune corpus the
// same query removes $59.15 of $76.46
// (77.4%) from 179 credited rows, which is where this costs real money.
//
// The two figures are far apart because THE LEAK IS NOT THE SAME ONE, and a reader who assumes
// one of them is simply wrong will draw the wrong conclusion from both. That corpus has idle gaps
// running to 2,750 s, so its credits fail for the reason the paragraph above describes: a gap
// longer than any ping could bridge. Production has no gap over 821.8 s against 860 s of
// coverage, so the missing upper bound is real code with no trigger there — what leaks instead is
// a keepalive_pings set on rows whose span held no ping at all. One test catches both, which is
// the argument for measuring coverage rather than gating on the gap. The credit on those rows belongs to
// whatever else refreshed the entry — another session sending byte-identical content — and not
// to us.
//
// It is corrected HERE, and not in the arithmetic that computes it, for two reasons. The Event
// cannot answer the question: it carries KeepAlive, KeepAlivePings, KeepAliveRefreshed and
// KeepAliveStrategyID and nothing about the policy that sent the pings, and the requests table
// has the same four columns and no (X, K) either — which is why the check is absent rather than
// wrong. And a read-time test repairs the history already on disk: 105 rows here, with no
// migration and no backfill, on a column whose docstring says it is never backfilled.
//
// The test MEASURES coverage instead of predicting it. A ping is itself a stored row
// (keepalive = 1) with a ts and a session, so "was this row's prefix actually still under a
// ping's 5-minute window" is a fact on disk, not an inference from a strategy's (X, K) — which
// would assume every ping fired on schedule and would say nothing at all about the 8 credited
// rows that carry no strategy id.
//
// `kp.cache_read > 0` is the second half of the test and it is the DIRECT observation rather than
// a proxy: a ping refreshed the entry exactly when it read the entry. An earlier version asked
// for status = 200 instead, which is weaker in both directions — it credits the 18 ping rows in
// this corpus that returned 200 while reading nothing (a ping that arrived after the entry had
// already expired re-CREATED it, which is why keepalive.go publishes PingsThatReadNothing and
// PingsThatWrote beside the saving), and it refuses a ping whose refresh is visible but whose
// status was never recorded. Both of the 502/503 ping rows here read nothing, so this subsumes
// the status test on real data. It is also stricter: $155.47 under status = 200, $152.36 here.
//
// alias is the outer table's qualifier and it MUST be one: an un-aliased caller resolves
// `ts` to the SUBQUERY's own requests row (inner scope wins in SQL), turning `kp.ts < ts` into
// `kp.ts < kp.ts` — always false, every credit silently zeroed, no error. A test caught exactly
// that, which is why the two un-aliased callers were given an alias rather than this an empty
// default. The subquery's own alias is kp, which no caller uses.
//
// Cost, and this ORDER IS LOAD-BEARING: the probe is guarded behind `> 0` in the same AND, so it
// runs once per CREDITED row (105 of 133,064 on the corpus above) rather than once per row in
// scope. Measured on that 2.1 GB database, a full-table SUM goes from 141 ms to 314 ms with the
// guard and to 61.9 SECONDS without it — 197x, same answer — which is the same shape as the
// correlated subquery that once made Overview a 25-second page. Put the EXISTS first and this
// becomes that bug. It seeks idx_requests_tenant_session (tenant_id, session_id, ts), which
// already exists.
//
// Same reason sumBySession's row FILTER stays `keepalive_saved_usd > 0` while its VALUE is this
// expression. The filter is a superset of the value's condition, so no contributing row is lost —
// a session whose credit is entirely unreachable reports $0.00, and all three callers look up by
// session id rather than treating map presence as "credited". Narrowing the filter would move the
// EXISTS into the WHERE, once per row IN SCOPE instead of once per credited row: the 61.9s one.
//
// Two deliberate looseness's, both stated rather than hidden. `kp.ts <= r.ts` rather than `<`,
// because a ping and the request it rescued can land in the same millisecond and the tie is
// harmless — kp.keepalive = 1 makes a row matching itself impossible. And ts is the row's
// recorded instant, where the provider measures its lifetime from the request START, so the
// window can be overstated by one request's duration. Both make this a CEILING on coverage,
// which is the same direction SavedUSD is already documented to err in.
func kaSaved(alias string) string {
	if alias == "" {
		panic("dash: kaSaved needs a table alias; an un-aliased one resolves to the subquery")
	}
	col := alias + "keepalive_saved_usd"
	// The `> 0` FIRST is not style: it short-circuits the probe to the credited rows. Measured on
	// a 2.1 GB database, a full-table SUM is 314 ms this way and 61.9 SECONDS with the EXISTS
	// first, for the identical answer. Do not reorder.
	return `(CASE WHEN ` + col + ` > 0 AND EXISTS (
			SELECT 1 FROM requests kp
			WHERE kp.keepalive = 1 AND kp.cache_read > 0
			  AND kp.tenant_id = ` + alias + `tenant_id AND kp.session_id = ` + alias + `session_id
			  AND kp.ts <= ` + alias + `ts
			  AND kp.ts + ` + strconv.FormatInt(providerCacheTTLMs, 10) + ` >= ` + alias + `ts)
		THEN ` + col + ` ELSE 0 END)`
}

// KeepAliveCoverage is the "is this figure a measurement or an absence?" statement that
// accompanies every number on the tab.
//
// It exists because `keepalive_pings` and `keepalive_saved_usd` arrived as ADDITIVE columns with
// DEFAULT 0. On a row written before PR #86 a zero means NOT RECORDED, and rendering that as
// "no pings" would be a fabricated measurement — the failure this project has hit repeatedly.
// Three distinct states, never conflated: never ran, partially recorded, or plain figures.
type KeepAliveCoverage struct {
	// RecordedFrom is MIN(ts) of any row carrying a ping or a ping-derived credit. 0 means the
	// keep-alive has never run on this account, which is not the same as having saved nothing.
	RecordedFrom int64 `json:"keepalive_recorded_from"`
	// RecordedRows is the rows at or after that instant, and Requests the rows in the window.
	// RecordedRows < Requests is the partial state.
	RecordedRows int64 `json:"keepalive_recorded_rows"`
	Requests     int64 `json:"requests"`
}

// KeepAliveLedger is panel 1 and panel 2: the verdict, and the losing majority beside it.
type KeepAliveLedger struct {
	Pings         int64   `json:"pings"`
	PingUSD       float64 `json:"ping_usd"`
	MissesAvoided int64   `json:"misses_avoided"`
	// SavedUSD is a CEILING, and every tile that shows it says so. The credit itself is exact
	// arithmetic — see Event.keepaliveSavedUSD — but the provider's cache is keyed on CONTENT,
	// so another session sending a byte-identical prefix would have refreshed the same entry
	// for nothing. That confound cannot be measured from our side; it can only be declared.
	SavedUSD float64 `json:"saved_usd"`
	NetUSD   float64 `json:"net_usd"`
	// The winner/loser split over the sessions the mechanism TOUCHED. 85 of 119 lose about a
	// third of a cent service-wide, funding a large rebate on 34 — the panel states that in
	// words, unconditionally, above the fold.
	Sessions       int64   `json:"sessions_touched"`
	Winners        int64   `json:"winners"`
	Losers         int64   `json:"losers"`
	WorstSession   string  `json:"worst_session"`
	WorstNetUSD    float64 `json:"worst_net_usd"`
	Addressable    int64   `json:"addressable_misses"`
	AddressableUSD float64 `json:"addressable_usd"`
	// PingsThatReadNothing is the operator's check on SavedUSD: a ping that read nothing
	// refreshed nothing. PingsThatWrote is worse — it CREATED an entry, which costs 12.5x a
	// read, and the keeper stops pinging that session.
	PingsThatReadNothing int64 `json:"pings_that_read_nothing"`
	PingsThatWrote       int64 `json:"pings_that_wrote"`
	// BytesPerDay is what this account's own ping rows cost on disk, measured rather than
	// assumed: 294 B of row plus ~103 B of index per ping. Here because per-session overrides
	// are the mechanism by which somebody could raise it.
	PingsPerDay float64 `json:"pings_per_day"`
	BytesPerDay float64 `json:"bytes_per_day"`
	// Latency is the window's own measured hit-vs-write speed difference for large-context
	// requests — see KeepAliveLatencyDiagnostic. Known is false (and every figure zero) on
	// a window too small to measure it, never a fabricated number.
	Latency kvcache.Latency `json:"latency"`
	KeepAliveCoverage
}

// bytesPerPingRow is one ping row's measured footprint: 294 B in `requests` plus ~103 B of
// index, from dbstat over the production snapshot. Rounded up to 400.
const bytesPerPingRow = 400

// KeepAliveLedger computes the ledger for one filtered window.
func (d *DB) KeepAliveLedger(f Filter) (*KeepAliveLedger, error) {
	var o KeepAliveLedger
	// The SAVING half and the coverage, over agent traffic only — the credit lives on the real
	// request that benefited, never on the ping.
	cond, args := f.where()
	var from sql.NullInt64
	if err := d.sql.QueryRow(`SELECT
		COALESCE(SUM(`+kaSaved("r.")+`),0),
		COALESCE(SUM(CASE WHEN `+kaSaved("r.")+` > 0 THEN 1 ELSE 0 END),0),
		COUNT(*)
		FROM requests r WHERE `+cond, args...).Scan(
		&o.SavedUSD, &o.MissesAvoided, &o.Requests); err != nil {
		return nil, err
	}
	// The COST half, with ping rows included. A second query and not a CASE: one predicate,
	// one meaning.
	kaCond, kaArgs := withKeepAlive(f).where()
	if err := d.sql.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.keepalive = 1 AND r.cache_read = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.keepalive = 1 AND r.cache_write > r.cache_read THEN 1 ELSE 0 END),0),
		MIN(CASE WHEN r.keepalive = 1 OR r.keepalive_pings > 0 OR r.keepalive_saved_usd > 0
			THEN r.ts END)
		FROM requests r WHERE `+kaCond, kaArgs...).Scan(
		&o.Pings, &o.PingUSD, &o.PingsThatReadNothing, &o.PingsThatWrote, &from); err != nil {
		return nil, err
	}
	o.NetUSD = o.SavedUSD - o.PingUSD
	o.RecordedFrom = from.Int64
	if o.RecordedFrom > 0 {
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond+` AND r.ts >= ?`,
			append(append([]any(nil), args...), o.RecordedFrom)...).Scan(&o.RecordedRows); err != nil {
			return nil, err
		}
	}
	// The winner/loser split, per session, over the sessions the mechanism touched. Two
	// grouped queries joined in Go for the same reason as above: the credit is on agent rows
	// and the cost is on ping rows, and one aggregate cannot honestly see both.
	saved, err := d.sumBySession(cond, args, kaSaved("r."), "r.keepalive_saved_usd > 0")
	if err != nil {
		return nil, err
	}
	spent, err := d.sumBySession(kaCond, kaArgs, "r.cost_usd", "r.keepalive = 1")
	if err != nil {
		return nil, err
	}
	nets := map[string]float64{}
	for s, v := range saved {
		nets[s] += v
	}
	for s, v := range spent {
		nets[s] -= v
	}
	for s, net := range nets {
		o.Sessions++
		switch {
		case net > 0:
			o.Winners++
		case net < 0:
			o.Losers++
		}
		if net < o.WorstNetUSD {
			o.WorstNetUSD, o.WorstSession = net, s
		}
	}
	// What is still on the table: the addressable expiries in this window, and their bill.
	aCond, aArgs := addressable(f)
	if err := d.sql.QueryRow(aCond+`
		SELECT COUNT(*), COALESCE(SUM(cost_usd),0) FROM addressable`, aArgs...).Scan(
		&o.Addressable, &o.AddressableUSD); err != nil {
		return nil, err
	}
	// Ping rate and its footprint on disk, over the window's own span. Nothing here stores
	// text: for contrast a sibling feature put 264.7 MB on disk in a day by duplicating it.
	if days := d.windowDays(kaCond, kaArgs); days > 0 {
		o.PingsPerDay = float64(o.Pings) / days
		o.BytesPerDay = o.PingsPerDay * bytesPerPingRow
	}
	lat, err := d.KeepAliveLatencyDiagnostic(f)
	if err != nil {
		return nil, err
	}
	o.Latency = lat
	return &o, nil
}

// KeepAliveNetUSDByTenant is the keep-alive net (credit minus ping spend) for every tenant
// since a given time, in one pass — the exporter's per-tenant series otherwise needs one
// query per tenant, and CachesplitHistoricalUSDByTenant (dash/cachehistory.go) is why that
// gets paid for once instead of N times over.
func (d *DB) KeepAliveNetUSDByTenant(since int64) (map[string]float64, error) {
	out := map[string]float64{}
	cond, args := (Filter{Since: since, TenantAll: true}).where()
	rows, err := d.sql.Query(`SELECT r.tenant_id, COALESCE(SUM(`+kaSaved("r.")+`),0)
		FROM requests r WHERE `+cond+` GROUP BY r.tenant_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tid string
		var v float64
		if err := rows.Scan(&tid, &v); err != nil {
			rows.Close()
			return nil, err
		}
		out[tid] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kaCond, kaArgs := withKeepAlive(Filter{Since: since, TenantAll: true}).where()
	rows2, err := d.sql.Query(`SELECT r.tenant_id, COALESCE(SUM(r.cost_usd),0)
		FROM requests r WHERE `+kaCond+` AND r.keepalive = 1 GROUP BY r.tenant_id`, kaArgs...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var tid string
		var v float64
		if err := rows2.Scan(&tid, &v); err != nil {
			return nil, err
		}
		out[tid] -= v
	}
	return out, rows2.Err()
}

// largeContextTokens is the combined (fresh_input+cache_read+cache_write) size above which a
// cache-hit-vs-write latency comparison stops being confounded by request size. Below it,
// live data shows the comparison running BACKWARDS (smaller cache-write requests are simple,
// fast one-shot calls; smaller cache-hit requests skew toward slower follow-up turns) — real
// on this deployment at 100K+, checked directly against production traffic rather than
// assumed. This is also where keep-alive/cache-split actually operate, so it is the population
// their latency benefit is meaningful to measure over.
const largeContextTokens = 100_000

// KeepAliveLatencyDiagnostic reports the window's own measured hit-vs-write latency
// differential for large-context requests, reusing kvcache.MeasureLatency's shape (mean per
// cohort, gated at N>=20 — Known=false below that rather than a difference of two noisy
// means). A diagnostic about the MECHANISM — a cache hit is faster than a cache write — not a
// per-request or per-dollar claim: see kvcache.Latency's own comment for why, and why no
// per-request counterfactual is computed anywhere in this codebase.
func (d *DB) KeepAliveLatencyDiagnostic(f Filter) (kvcache.Latency, error) {
	var l kvcache.Latency
	cond, args := f.where()
	var hitSum, missSum sql.NullFloat64
	row := d.sql.QueryRow(`SELECT
			SUM(CASE WHEN r.cache_read > r.cache_write AND r.cache_read > 0 THEN r.upstream_ms END),
			COALESCE(SUM(CASE WHEN r.cache_read > r.cache_write AND r.cache_read > 0 THEN 1 ELSE 0 END),0),
			SUM(CASE WHEN r.cache_write >= r.cache_read AND r.cache_write > 0 THEN r.upstream_ms END),
			COALESCE(SUM(CASE WHEN r.cache_write >= r.cache_read AND r.cache_write > 0 THEN 1 ELSE 0 END),0)
		FROM requests r
		WHERE `+cond+` AND r.upstream_ms > 0
		  AND (r.fresh_input + r.cache_read + r.cache_write) >= ?`,
		append(args, largeContextTokens)...)
	if err := row.Scan(&hitSum, &l.HitN, &missSum, &l.MissN); err != nil {
		return l, err
	}
	if l.HitN < kvcache.LatencyMinN || l.MissN < kvcache.LatencyMinN {
		return l, nil
	}
	l.HitMeanMs = hitSum.Float64 / float64(l.HitN)
	l.MissMean = missSum.Float64 / float64(l.MissN)
	l.PerMissMs = l.MissMean - l.HitMeanMs
	l.Known = true
	return l, nil
}

// sumBySession groups one column by session under an extra predicate.
func (d *DB) sumBySession(cond string, args []any, col, extra string) (map[string]float64, error) {
	rows, err := d.sql.Query(`SELECT r.session_id, COALESCE(SUM(`+col+`),0) FROM requests r
		WHERE `+cond+` AND `+extra+` AND r.session_id <> '' GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var s string
		var v float64
		if err := rows.Scan(&s, &v); err != nil {
			return nil, err
		}
		out[s] = v
	}
	return out, rows.Err()
}

// windowDays is the span the rows actually cover, in days. Derived from the data and not from
// the filter: a filter may be unbounded, and dividing by a window nobody has any rows in would
// report a rate per day of traffic that did not happen.
func (d *DB) windowDays(cond string, args []any) float64 {
	var lo, hi sql.NullInt64
	if err := d.sql.QueryRow(`SELECT MIN(r.ts), MAX(r.ts) FROM requests r WHERE `+cond,
		args...).Scan(&lo, &hi); err != nil {
		return 0
	}
	if !lo.Valid || !hi.Valid || hi.Int64 <= lo.Int64 {
		return 0
	}
	return float64(hi.Int64-lo.Int64) / 86400000.0
}

// Band is one bucket of an ORDERED histogram: idle-gap bands, prefix-size bands, hour bins.
//
// Label carries the meaning and the order is the data — a bar chart of these must preserve it
// and must not sort by size, because the order IS what the reader is reading.
//
// Deliberately NO since/until here. These bands bucket by GAP LENGTH, by PREFIX SIZE and by HOUR
// OF DAY — none of which is a time range, so a bar's drill-down is `reason=ttl_expiry` plus the
// window already on screen and nothing more. A pair of fields that no band could ever populate
// would be two zeroes on the wire that something eventually renders as a date.
type Band struct {
	Label string  `json:"label"`
	N     int64   `json:"n"`
	USD   float64 `json:"usd"`
	// Beyond marks the bands the CURRENT policy's coverage cannot reach, which the chart draws
	// in the de-emphasis gray beyond the coverage rule.
	Beyond bool `json:"beyond,omitempty"`
}

// DayPoint is one day of the "how often, and what did it cost" series.
type DayPoint struct {
	Day        string  `json:"day"`
	TS         int64   `json:"ts"`
	Misses     int64   `json:"misses"`
	USD        float64 `json:"usd"`
	Requests   int64   `json:"requests"`
	AllUSD     float64 `json:"all_usd"`
	SharePct   float64 `json:"share_pct"`
	MissRatePc float64 `json:"miss_rate_pct"`
}

// AccountGaps is panel 3d: how long between one account's own TTL expiries.
//
// Reported PER ACCOUNT and never blended. There is no service-wide "usually": per-account means
// run 0.64 h to 6.56 h with maxima to 30 h, so a single mean across a 10x spread would be a
// number that describes nobody.
type AccountGaps struct {
	Tenant   string  `json:"tenant"`
	N        int64   `json:"n"`
	MeanHrs  float64 `json:"mean_hours"`
	MedHrs   float64 `json:"median_hours"`
	MaxHrs   float64 `json:"max_hours"`
	Coverage bool    `json:"has_gaps"`
}

// KeepAliveBehaviour is panels 3a–3e.
type KeepAliveBehaviour struct {
	Daily []DayPoint `json:"daily"`
	// GapBands is the idle-gap histogram with FIXED edges, so the shape is comparable between
	// accounts and between windows. The edge at 580 s is not arbitrary: it is the shipped
	// policy's own coverage at X=280, K=1.
	GapBands    []Band        `json:"gap_bands"`
	PrefixBands []Band        `json:"prefix_bands"`
	HourBins    []Band        `json:"hour_bins"`
	Gaps        []AccountGaps `json:"account_gaps"`
	// PrefixP10/P50/P90 in tokens, over the lapsed entries. p50 is 292,527 on the corpus and
	// 347 of 356 are past 20k — which is the measured reason the shipped `prefix >= 20k` gate
	// costs almost nothing, and the panel says so.
	PrefixP10 int64 `json:"prefix_p10"`
	PrefixP50 int64 `json:"prefix_p50"`
	PrefixP90 int64 `json:"prefix_p90"`
	// GapP10/P50/P90 in seconds.
	GapP10 float64 `json:"gap_p10"`
	GapP50 float64 `json:"gap_p50"`
	GapP90 float64 `json:"gap_p90"`
	// CoverageSeconds is K*X + TTL under the policy the bands are drawn against, which is what
	// the threshold rule on the chart marks.
	CoverageSeconds float64 `json:"coverage_seconds"`
	AboveTwentyK    int64   `json:"prefix_above_20k"`
	Addressable     int64   `json:"addressable_misses"`
	Phantom         int64   `json:"phantom_ttl_rows"`
	KeepAliveCoverage
}

// gapEdges are the idle-gap bands' fixed edges in seconds. The last is unbounded.
var gapEdges = []float64{0, 300, 580, 600, 1800, 3600, 14400}

// prefixEdges are the billed-prefix bands' fixed edges in tokens. 20,000 is the shipped gate.
var prefixEdges = []float64{0, 20000, 50000, 100000, 200000, 400000, 800000}

// bandLabels renders the edge list as human labels, with a unit formatter.
func bandLabels(edges []float64, unit func(float64) string) []string {
	out := make([]string, 0, len(edges))
	for i, lo := range edges {
		if i == len(edges)-1 {
			out = append(out, "> "+unit(lo))
			continue
		}
		out = append(out, unit(lo)+"–"+unit(edges[i+1]))
	}
	return out
}

// bandOf places a value in the edge list.
func bandOf(edges []float64, v float64) int {
	for i := len(edges) - 1; i >= 0; i-- {
		if v >= edges[i] {
			return i
		}
	}
	return 0
}

// KeepAliveBehaviour computes the behavioural panels for one window.
//
// coverageSeconds is the policy the gap bands are marked against (K*X + TTL); 0 falls back to
// the shipped default's 860 s.
func (d *DB) KeepAliveBehaviour(f Filter, coverageSeconds float64) (*KeepAliveBehaviour, error) {
	if coverageSeconds <= 0 {
		coverageSeconds = 2*280 + ttlTTL.Seconds()
	}
	out := &KeepAliveBehaviour{CoverageSeconds: coverageSeconds,
		Daily: []DayPoint{}, Gaps: []AccountGaps{}}
	aCond, aArgs := addressable(f)
	cond, args := f.where()

	// 3a: per day, and the SHARE of that day's whole bill, because a count with no
	// denominator cannot be sized.
	all := map[string]struct {
		n   int64
		usd float64
	}{}
	rows, err := d.sql.Query(`SELECT date(r.ts/1000,'unixepoch'), COUNT(*), COALESCE(SUM(r.cost_usd),0)
		FROM requests r WHERE `+cond+` GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day string
		var n int64
		var usd float64
		if err := rows.Scan(&day, &n, &usd); err != nil {
			rows.Close()
			return nil, err
		}
		all[day] = struct {
			n   int64
			usd float64
		}{n, usd}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = d.sql.Query(aCond+`
		SELECT date(ts/1000,'unixepoch'), MIN(ts), COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM addressable GROUP BY 1 ORDER BY 1`, aArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.TS, &p.Misses, &p.USD); err != nil {
			rows.Close()
			return nil, err
		}
		if a, ok := all[p.Day]; ok {
			p.Requests, p.AllUSD = a.n, a.usd
			if a.usd > 0 {
				p.SharePct = 100 * p.USD / a.usd
			}
			if a.n > 0 {
				p.MissRatePc = 100 * float64(p.Misses) / float64(a.n)
			}
		}
		out.Daily = append(out.Daily, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3b/3e: the two band histograms and the 24 hour bins, from one pass over `addressable`.
	// Percentiles come from the same read, so the bands and the p50 beside them cannot
	// disagree.
	gapN := make([]int64, len(gapEdges))
	gapUSD := make([]float64, len(gapEdges))
	preN := make([]int64, len(prefixEdges))
	preUSD := make([]float64, len(prefixEdges))
	hourN := make([]int64, 24)
	hourUSD := make([]float64, 24)
	var gaps, prefixes []float64
	rows, err = d.sql.Query(aCond+`
		SELECT gap_s, COALESCE(prev_prefix,0), cost_usd,
		       CAST(strftime('%H', ts/1000, 'unixepoch') AS INTEGER)
		FROM addressable`, aArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var gap, prefix, usd float64
		var hour int
		if err := rows.Scan(&gap, &prefix, &usd, &hour); err != nil {
			rows.Close()
			return nil, err
		}
		out.Addressable++
		gi := bandOf(gapEdges, gap)
		gapN[gi]++
		gapUSD[gi] += usd
		pi := bandOf(prefixEdges, prefix)
		preN[pi]++
		preUSD[pi] += usd
		if hour >= 0 && hour < 24 {
			hourN[hour]++
			hourUSD[hour] += usd
		}
		gaps = append(gaps, gap)
		prefixes = append(prefixes, prefix)
		if prefix >= 20000 {
			out.AboveTwentyK++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Axis labels are read by a person, so they are ROUNDED. %g on 580/60 renders
	// "9.666666666666666m", which is what the first live render of this panel showed.
	secs := func(v float64) string {
		switch {
		case v >= 3600:
			return trimZero(v/3600) + "h"
		case v >= 60:
			return trimZero(v/60) + "m"
		default:
			return trimZero(v) + "s"
		}
	}
	toks := func(v float64) string {
		if v >= 1000 {
			return trimZero(v/1000) + "k"
		}
		return trimZero(v)
	}
	for i, label := range bandLabels(gapEdges, secs) {
		out.GapBands = append(out.GapBands, Band{Label: label, N: gapN[i], USD: gapUSD[i],
			Beyond: gapEdges[i] >= coverageSeconds})
	}
	for i, label := range bandLabels(prefixEdges, toks) {
		out.PrefixBands = append(out.PrefixBands, Band{Label: label, N: preN[i], USD: preUSD[i]})
	}
	for h := 0; h < 24; h++ {
		out.HourBins = append(out.HourBins, Band{
			Label: fmt.Sprintf("%02d", h), N: hourN[h], USD: hourUSD[h]})
	}
	out.GapP10, out.GapP50, out.GapP90 = pctlF(gaps, 0.10), pctlF(gaps, 0.50), pctlF(gaps, 0.90)
	out.PrefixP10 = int64(pctlF(prefixes, 0.10))
	out.PrefixP50 = int64(pctlF(prefixes, 0.50))
	out.PrefixP90 = int64(pctlF(prefixes, 0.90))

	// The phantoms, named rather than silently dropped: a reader comparing this panel with the
	// cache-miss breakdown on Usage will see two different `ttl_expiry` counts, and the
	// difference has to be explicable.
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond+`
		AND r.cache_miss_reason = 'ttl_expiry' AND r.cache_write = 0`, args...).Scan(
		&out.Phantom); err != nil {
		return nil, err
	}

	// 3d: gaps BETWEEN expiries, per account.
	rows, err = d.sql.Query(aCond+`, e AS (
		SELECT tenant_id, (ts - LAG(ts) OVER (PARTITION BY tenant_id ORDER BY ts)) / 3600000.0 AS h
		FROM addressable)
		SELECT tenant_id, COUNT(h), AVG(h), MAX(h) FROM e WHERE h IS NOT NULL GROUP BY 1
		ORDER BY 2 DESC`, aArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g AccountGaps
		var n int64
		var mean, max sql.NullFloat64
		if err := rows.Scan(&g.Tenant, &n, &mean, &max); err != nil {
			rows.Close()
			return nil, err
		}
		g.N, g.MeanHrs, g.MaxHrs, g.Coverage = n, mean.Float64, max.Float64, n > 0
		out.Gaps = append(out.Gaps, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The median per account, in a second pass. Exact rather than interpolated, for the reason
	// DB.percentile is: sorting a few hundred floats is free and an estimate here would be a
	// number nobody can reproduce.
	for i := range out.Gaps {
		g := &out.Gaps[i]
		var hs []float64
		r2, err := d.sql.Query(aCond+`, e AS (
			SELECT tenant_id, (ts - LAG(ts) OVER (PARTITION BY tenant_id ORDER BY ts)) / 3600000.0 AS h
			FROM addressable)
			SELECT h FROM e WHERE h IS NOT NULL AND tenant_id = ?`,
			append(append([]any(nil), aArgs...), g.Tenant)...)
		if err != nil {
			return nil, err
		}
		for r2.Next() {
			var h float64
			if err := r2.Scan(&h); err != nil {
				r2.Close()
				return nil, err
			}
			hs = append(hs, h)
		}
		r2.Close()
		if err := r2.Err(); err != nil {
			return nil, err
		}
		g.MedHrs = pctlF(hs, 0.50)
	}

	cov, err := d.keepAliveCoverage(f)
	if err != nil {
		return nil, err
	}
	out.KeepAliveCoverage = *cov
	return out, nil
}

// keepAliveCoverage answers "is a zero here a measurement or an absence?" for one window.
func (d *DB) keepAliveCoverage(f Filter) (*KeepAliveCoverage, error) {
	var c KeepAliveCoverage
	cond, args := f.where()
	kaCond, kaArgs := withKeepAlive(f).where()
	var from sql.NullInt64
	if err := d.sql.QueryRow(`SELECT MIN(CASE WHEN r.keepalive = 1 OR r.keepalive_pings > 0
		OR r.keepalive_saved_usd > 0 THEN r.ts END) FROM requests r WHERE `+kaCond,
		kaArgs...).Scan(&from); err != nil {
		return nil, err
	}
	c.RecordedFrom = from.Int64
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond, args...).Scan(
		&c.Requests); err != nil {
		return nil, err
	}
	if c.RecordedFrom > 0 {
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond+` AND r.ts >= ?`,
			append(append([]any(nil), args...), c.RecordedFrom)...).Scan(&c.RecordedRows); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// pctlF is an exact percentile over an in-memory slice, nearest-rank. Sorts a copy.
func pctlF(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p)
	return s[i]
}

// KeepAliveSessionRow is one row of panel 3f / panel 5: a session, what its expiries cost, and
// whether an override is armed on it.
type KeepAliveSessionRow struct {
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	Turns     int64  `json:"turns"`
	Last      int64  `json:"last"`
	Model     string `json:"model"`
	// LastPrefix is the last billed prefix (cache_read + cache_write) on this session — the
	// number the calculator prefills from, because per-ping cost is bimodal and a service-wide
	// average would answer a question nobody asked.
	LastPrefix int64   `json:"last_prefix"`
	Expiries   int64   `json:"expiries"`
	ExpiryUSD  float64 `json:"expiry_usd"`
	Pings      int64   `json:"pings"`
	PingUSD    float64 `json:"ping_usd"`
	SavedUSD   float64 `json:"saved_usd"`
	NetUSD     float64 `json:"net_usd"`
}

// KeepAliveSessions lists the sessions this window's addressable expiries are concentrated in,
// costliest first, with each one's own keep-alive ledger.
//
// The concentration is real and it does NOT transfer forward: the top 8 sessions hold $431 of
// $734.75, and a list fitted on one half of the week earned $5.27 in the next half where the
// plain account-wide rule earned $53.32. The panel says that above the table.
func (d *DB) KeepAliveSessions(f Filter, limit int) ([]*KeepAliveSessionRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	aCond, aArgs := addressable(f)
	rows, err := d.sql.Query(aCond+`
		SELECT session_id, MIN(tenant_id), COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM addressable WHERE session_id <> ''
		GROUP BY session_id ORDER BY 4 DESC LIMIT ?`,
		append(append([]any(nil), aArgs...), limit)...)
	if err != nil {
		return nil, err
	}
	out := []*KeepAliveSessionRow{}
	byID := map[string]*KeepAliveSessionRow{}
	for rows.Next() {
		var s KeepAliveSessionRow
		if err := rows.Scan(&s.SessionID, &s.TenantID, &s.Expiries, &s.ExpiryUSD); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, &s)
		byID[s.SessionID] = &s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// Turns, the last request and the last billed prefix — from AGENT rows, so a ping does not
	// count as a turn or re-date the session.
	cond, args := f.where()
	for _, s := range out {
		if err := d.sql.QueryRow(`SELECT COUNT(*), MAX(r.ts) FROM requests r
			WHERE `+cond+` AND r.session_id = ?`,
			append(append([]any(nil), args...), s.SessionID)...).Scan(&s.Turns, &s.Last); err != nil {
			return nil, err
		}
		var prefix sql.NullInt64
		var model sql.NullString
		if err := d.sql.QueryRow(`SELECT r.cache_read + r.cache_write, r.model FROM requests r
			WHERE `+cond+` AND r.session_id = ? ORDER BY r.ts DESC, r.id DESC LIMIT 1`,
			append(append([]any(nil), args...), s.SessionID)...).Scan(&prefix, &model); err != nil &&
			err != sql.ErrNoRows {
			return nil, err
		}
		s.LastPrefix, s.Model = prefix.Int64, model.String
	}
	// The keep-alive halves, each from its own query. Same two-query pattern as the overview.
	saved, err := d.sumBySession(cond, args, kaSaved("r."), "r.keepalive_saved_usd > 0")
	if err != nil {
		return nil, err
	}
	kaCond, kaArgs := withKeepAlive(f).where()
	spent, err := d.sumBySession(kaCond, kaArgs, "r.cost_usd", "r.keepalive = 1")
	if err != nil {
		return nil, err
	}
	pings, err := d.countBySession(kaCond, kaArgs, "r.keepalive = 1")
	if err != nil {
		return nil, err
	}
	for _, s := range out {
		s.SavedUSD, s.PingUSD, s.Pings = saved[s.SessionID], spent[s.SessionID], pings[s.SessionID]
		s.NetUSD = s.SavedUSD - s.PingUSD
	}
	return out, nil
}

// ttlTTL1h is the provider's extended prompt-cache lifetime, for sessions whose last write
// landed in the one-hour tier.
const ttlTTL1h = time.Hour

// write1hMultiple is the provider's published price for a ONE-HOUR cache write: 2.0x base
// input, against 1.25x for the five-minute tier and 0.1x for a read.
//
// Derived from Input rather than read off the operator's price list, because the list carries
// one `cache_write` rate per model and it is the 5-minute one — which is what the provider
// bills for the tier this service's models actually grant. Any 1-hour figure on this page is
// therefore explicitly a "what it would be" and is labelled as one.
const write1hMultiple = 2.0

// KeepAliveLiveRow is one session that may still hold a provider cache entry RIGHT NOW: which
// lifetime is in force on it, how long that entry has left, and what one ping and one lapse
// would each cost on this session's own prefix at its own model's rates.
//
// The economics are per SESSION and never blended, for the reason the calculator refuses to
// average a prefix: per-ping cost is bimodal (p50 $0.0004, p99 $0.2275), so a service-wide
// average is a number that describes no session on the list.
type KeepAliveLiveRow struct {
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	Model     string `json:"model"`
	Turns     int64  `json:"turns"`
	// LastTS is the last real request's START, which is what the provider measures the lifetime
	// from — not the end of its response. A response that streamed for four minutes has already
	// spent four minutes of its own entry's life.
	LastTS int64 `json:"last_ts"`
	// Prefix is that request's billed prefix (cache_read + cache_write): the size of the entry
	// at risk, in the provider's own units. Never tokens_before, which is message text only and
	// runs a median 3.38x low.
	Prefix int64 `json:"prefix_tokens"`
	// TTLSeconds is the lifetime IN FORCE: 3600 when this session's most recent write landed in
	// the one-hour tier, 300 otherwise. Read off cache_write_1h rather than off configuration,
	// because a `ttl: "1h"` request that the model does not support is granted as a 5-minute
	// entry with a perfectly normal 200 — the tier billed is the only honest source.
	TTLSeconds int64 `json:"ttl_seconds"`
	// RemainingSeconds is what is left of it. Rows at or below zero are not returned: the entry
	// is gone and the page would be stating a lifetime that has already elapsed.
	RemainingSeconds float64 `json:"remaining_seconds"`
	// Pings/PingUSD/SavedUSD are this session's keep-alive history in the window. Whether it is
	// armed RIGHT NOW is not here: that lives in the proxy's control plane, which the dashboard
	// does not read — the page joins the two, from /api/me/keepalive/sessions.
	Pings    int64   `json:"pings"`
	PingUSD  float64 `json:"ping_usd"`
	SavedUSD float64 `json:"saved_usd"`
	// Priced false means this model has no rate on the operator's list, and then EVERY dollar
	// below is omitted rather than defaulted. A blended rate here is the defect class this
	// project has hit five times.
	Priced bool `json:"priced"`
	// PingUSDEach is one ping on this prefix: the prefix at the model's cache-READ rate plus the
	// single output token the ping asks for. MissUSD is what letting the entry lapse costs — the
	// avoidable WRITE PREMIUM, prefix x (cache_write - cache_read), and not the whole re-read,
	// because a resuming request pays to read its prefix either way.
	PingUSDEach float64 `json:"ping_usd_each,omitempty"`
	MissUSD     float64 `json:"miss_usd,omitempty"`
	// BreakevenPings is how many pings one lapse pays for: MissUSD / PingUSDEach, floored. It is
	// THE number that decides whether to arm, and it is nearly prefix-independent — both terms
	// scale with the prefix, so it collapses to a ratio of the model's own rates.
	// BreakevenMinutes is the idle time that many pings bridges at the current policy.
	BreakevenPings   int64   `json:"breakeven_pings,omitempty"`
	BreakevenMinutes float64 `json:"breakeven_minutes,omitempty"`
	// The same pair for a ONE-HOUR entry, which is strictly a "what it would be" on a deployment
	// whose models grant 5 minutes: a 1h write costs 2.0x base against 1.25x, so a lapse is
	// dearer and pays for more pings, and each ping buys an hour instead of five minutes.
	Breakeven1hPings   int64   `json:"breakeven_1h_pings,omitempty"`
	Breakeven1hMinutes float64 `json:"breakeven_1h_minutes,omitempty"`
}

// KeepAliveLive is the live-session answer: the rows, and the clock they were computed against.
type KeepAliveLive struct {
	// Now is the server's clock at read time. On the wire so the page can count down without
	// drifting against a browser clock that may be minutes out — a countdown computed from the
	// client's own clock is how a "2 minutes left" reads on a session that expired ten ago.
	Now int64 `json:"now"`
	// IdleSeconds and MaxPings are the policy the reach figures are computed at.
	IdleSeconds float64 `json:"idle_seconds"`
	MaxPings    int     `json:"max_pings"`
	// SoonSeconds is the threshold the page warns at.
	SoonSeconds float64 `json:"soon_seconds"`
	// Soon and SoonUSD are the expiry warning: how many rows are inside SoonSeconds of lapsing
	// and what they would pay to re-create what they are about to lose. PotentialUSD is the same
	// figure over every live row.
	//
	// Summed HERE and not in the browser. The tab's rule is that the server owns every dollar on
	// the page (see renderKACalcControls: "nothing below multiplies a dollar by anything"), and
	// a total assembled client-side is a figure with no test behind it — which is how a
	// denominator came to be the twenty rows a table happened to be showing.
	Soon         int64   `json:"soon"`
	SoonUSD      float64 `json:"soon_usd"`
	PotentialUSD float64 `json:"potential_usd"`
	// SoonUnpriced counts rows inside the warning whose model has no rate, so the page can say
	// the total excludes them rather than implying it covers everything.
	SoonUnpriced int64              `json:"soon_unpriced"`
	Rows         []KeepAliveLiveRow `json:"rows"`
}

// keepAliveSoon is how much of an entry's life left counts as "about to expire".
//
// One idle interval plus the margin a ping needs to land: below this, the next ping is the last
// one that can still reach the entry, so it is the last moment arming can change the outcome.
const keepAliveSoon = 330 * time.Second

// KeepAliveLive lists the sessions whose provider cache entry has not yet lapsed.
//
// `now` is passed in rather than read, for the same reason the keeper's clock is: a function
// that reads the wall clock cannot be tested against a fixture.
func (d *DB) KeepAliveLive(f Filter, now int64, idleSeconds float64, maxPings int,
	price func(string) (modelinfo.Price, bool)) (*KeepAliveLive, error) {
	if idleSeconds <= 0 {
		idleSeconds = recIdleSeconds
	}
	if maxPings <= 0 {
		maxPings = recMaxPings
	}
	out := &KeepAliveLive{Now: now, IdleSeconds: idleSeconds, MaxPings: maxPings,
		SoonSeconds: keepAliveSoon.Seconds(), Rows: []KeepAliveLiveRow{}}
	cond, args := f.where()
	// One pass. The partition is (tenant, session) exactly as everywhere else on this tab, and
	// the three window MAXes answer "which tier is in force" without a query per row: the entry's
	// lifetime is the one it was WRITTEN at, and a read refreshes it at that same tier, so the
	// tell is whether this session's most recent write was a one-hour write.
	rows, err := d.sql.Query(`WITH t AS (
		SELECT r.session_id AS session_id, r.tenant_id AS tenant_id, r.ts AS ts, r.model AS model,
		       r.cache_read + r.cache_write AS prefix,
		       ROW_NUMBER() OVER w AS rn,
		       COUNT(*) OVER w2 AS turns,
		       MAX(r.ts) OVER w2 AS last_ts,
		       MAX(CASE WHEN r.cache_write > 0 THEN r.ts ELSE 0 END) OVER w2 AS last_write_ts,
		       MAX(CASE WHEN r.cache_write_1h > 0 THEN r.ts ELSE 0 END) OVER w2 AS last_1h_ts
		FROM requests r WHERE `+cond+` AND r.session_id <> ''
		WINDOW w AS (PARTITION BY r.tenant_id, r.session_id ORDER BY r.ts DESC, r.id DESC),
		       w2 AS (PARTITION BY r.tenant_id, r.session_id))
		SELECT session_id, tenant_id, model, turns, ts, prefix,
		       CASE WHEN last_1h_ts > 0 AND last_1h_ts = last_write_ts THEN 1 ELSE 0 END
		FROM t WHERE rn = 1 AND ts >= ?
		ORDER BY prefix DESC, session_id`,
		append(args, now-ttlTTL1h.Milliseconds())...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r KeepAliveLiveRow
		var oneHour int
		if err := rows.Scan(&r.SessionID, &r.TenantID, &r.Model, &r.Turns, &r.LastTS,
			&r.Prefix, &oneHour); err != nil {
			return nil, err
		}
		ttl := ttlTTL
		if oneHour == 1 {
			ttl = ttlTTL1h
		}
		r.TTLSeconds = int64(ttl.Seconds())
		r.RemainingSeconds = float64(r.LastTS+ttl.Milliseconds()-now) / 1000
		if r.RemainingSeconds <= 0 {
			continue // the entry is gone; a lifetime that has elapsed is not a lifetime in force
		}
		if price != nil && r.Model != "" && r.Prefix > 0 {
			if p, ok := price(r.Model); ok && !p.Zero() {
				r.Priced = true
				// The ping as the request path actually sends it: max_tokens 1, so one output
				// token is billed. max_tokens 0 is accepted by this gateway and bills none, but
				// the figure has to match what is sent, not the cheapest shape available.
				r.PingUSDEach = float64(r.Prefix)*p.CacheRead + p.Output
				r.MissUSD = float64(r.Prefix) * (p.CacheWrite - p.CacheRead)
				if r.PingUSDEach > 0 {
					r.BreakevenPings = int64(r.MissUSD / r.PingUSDEach)
					r.BreakevenMinutes = CoverageSeconds(idleSeconds, int(r.BreakevenPings)) / 60
					miss1h := float64(r.Prefix) * (p.Input*write1hMultiple - p.CacheRead)
					r.Breakeven1hPings = int64(miss1h / r.PingUSDEach)
					// An hour-long entry needs a ping only once an hour, so the same count of
					// pings bridges twelve times the wall clock: K x X per span still, but the
					// lifetime the last one refreshes is 3600 s and not 300.
					if r.Breakeven1hPings > 0 {
						r.Breakeven1hMinutes = (float64(r.Breakeven1hPings)*idleSeconds +
							ttlTTL1h.Seconds()) / 60
					}
				}
			}
		}
		out.PotentialUSD += r.MissUSD
		if r.RemainingSeconds <= out.SoonSeconds {
			out.Soon++
			out.SoonUSD += r.MissUSD
			if !r.Priced {
				out.SoonUnpriced++
			}
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Rows) == 0 {
		return out, nil
	}
	// This session's own keep-alive history, from the same two queries the concentration table
	// uses, so the two panels cannot disagree about what a session's pings cost.
	saved, err := d.sumBySession(cond, args, kaSaved("r."), "r.keepalive_saved_usd > 0")
	if err != nil {
		return nil, err
	}
	kaCond, kaArgs := withKeepAlive(f).where()
	spent, err := d.sumBySession(kaCond, kaArgs, "r.cost_usd", "r.keepalive = 1")
	if err != nil {
		return nil, err
	}
	pings, err := d.countBySession(kaCond, kaArgs, "r.keepalive = 1")
	if err != nil {
		return nil, err
	}
	for i := range out.Rows {
		id := out.Rows[i].SessionID
		out.Rows[i].SavedUSD, out.Rows[i].PingUSD = saved[id], spent[id]
		out.Rows[i].Pings = pings[id]
	}
	return out, nil
}

// countBySession counts rows per session under an extra predicate.
func (d *DB) countBySession(cond string, args []any, extra string) (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT r.session_id, COUNT(*) FROM requests r
		WHERE `+cond+` AND `+extra+` AND r.session_id <> '' GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// CoverageSeconds is the window one idle span's pings keep a cache entry alive for.
//
// K*X + TTL, and the trailing TTL is LOAD-BEARING: the last ping is itself a cache READ, and a
// read refreshes the entry for the provider's full lifetime. Writing K*X instead is the single
// error that made an earlier analysis of this mechanism wrong by a factor of 4.4, so
// TestCoverageIncludesTheFinalPingsTTL exists to fail if anyone reintroduces it.
func CoverageSeconds(idleSeconds float64, maxPings int) float64 {
	if idleSeconds <= 0 || maxPings <= 0 {
		return 0
	}
	return float64(maxPings)*idleSeconds + ttlTTL.Seconds()
}

// PingsPerSpan is how many pings one idle span of `gap` seconds attracts under (X, K).
//
// The first ping fires at X, each subsequent one X after the last, and K caps the count. A gap
// no wider than X attracts none.
func PingsPerSpan(gap, idleSeconds float64, maxPings int) int {
	if gap <= idleSeconds || idleSeconds <= 0 || maxPings <= 0 {
		return 0
	}
	n := int(math.Floor((gap-idleSeconds)/idleSeconds)) + 1
	if n > maxPings {
		n = maxPings
	}
	return n
}

// kaGateMinPrefix is the billed-prefix floor a replay gates on: the shipped default, mirroring
// config.DefaultKeepAliveMinPrefix, which this package may not import. Pinned by
// TestTheReplayGateMatchesTheShippedPolicy.
const kaGateMinPrefix = 20000

// pingSpan is one idle span a live keep-alive would send pings in.
type pingSpan struct {
	session string
	// gap is the seconds until the next request in the session. Open says there was none.
	gap  float64
	open bool
}

// pings is how many pings this span attracts under (X, K).
//
// An OPEN span gets the full K. That is the whole correction: a live policy cannot know a
// session has ended, so it sends its K and stops — 7,782 of the 9,234 pings in the adjudicated
// replay were session-final, and a LAG-based span list, which exists only BETWEEN two requests,
// charges for none of them.
func (s pingSpan) pings(idleSeconds float64, maxPings int) int {
	if s.open {
		return maxPings
	}
	return PingsPerSpan(s.gap, idleSeconds, maxPings)
}

// pingSpans is the ONE definition of "the idle spans a live keep-alive would ping in", shared by
// the calculator and by the recommendation's bootstrap so the two cannot disagree about what a
// policy costs.
//
// It replaced a LAG over `requests` that was wrong in two directions: it charged NO pings for a
// session-final request, where a live policy must send K because it cannot know the session ended,
// and it charged pings on the turn-0 and small-prefix spans the shipped gate
// (`turn >= 1 AND prefix >= MinPrefixTokens`) never touches.
//
// The two errors PARTLY CANCEL, and the direction is the opposite of what it looks like. Replayed
// on the 19,805-request snapshot at X=280, K=2: the LAG form counts 1,452, this form counts 1,060,
// and a BLANKET policy would send 9,234. So the old column over-charged the shipped policy by
// 1.37x — correcting it makes NET slightly more positive on that corpus. The "6.4x under-count"
// that is easy to quote is the gap to a blanket policy, which the calculator does not model.
//
// What this buys is therefore not a different headline: it is a column that models the policy it
// names, so it MOVES when the gate or K moves instead of silently ignoring both.
//
// The gate is the request path's, in the same order: proxy/keepalive.go's kaEntry.pingable(). It
// gates at the shipped 20k default and not at the account's own configured floor, because this
// package does not read tenant configuration — an account that tuned its floor sees a replay of
// the default policy, and the panel's copy names 20k so the page stays self-consistent.
func (d *DB) pingSpans(f Filter, minPrefix int64) ([]pingSpan, error) {
	cond, args := f.where()
	// LEAD, not LAG: a span belongs to the request that OPENS it, which is the request the gate
	// is evaluated on, and LEAD is NULL exactly on the session-final row.
	rows, err := d.sql.Query(`WITH s AS (
		SELECT r.session_id AS session_id,
		       ROW_NUMBER() OVER (PARTITION BY r.tenant_id, r.session_id
		                          ORDER BY r.ts, r.id) - 1 AS turn,
		       r.cache_read + r.cache_write AS prefix,
		       (LEAD(r.ts) OVER (PARTITION BY r.tenant_id, r.session_id ORDER BY r.ts, r.id)
		         - r.ts) / 1000.0 AS gap_s
		FROM requests r WHERE `+cond+`)
		SELECT session_id, gap_s FROM s
		WHERE turn >= 1 AND prefix >= ? AND (gap_s IS NULL OR gap_s > 0)`,
		append(args, minPrefix)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pingSpan
	for rows.Next() {
		var sess string
		var gap sql.NullFloat64
		if err := rows.Scan(&sess, &gap); err != nil {
			return nil, err
		}
		out = append(out, pingSpan{session: sess, gap: gap.Float64, open: !gap.Valid})
	}
	return out, rows.Err()
}

// CalcRow is one rung of the K ladder.
type CalcRow struct {
	MaxPings int     `json:"max_pings"`
	Coverage float64 `json:"coverage_seconds"`
	// Convertible is the account's OWN addressable expiries whose idle gap falls inside
	// coverage — a replay of its history, not a forecast, which the panel states in words.
	Convertible    int64   `json:"convertible_misses"`
	ConvertibleUSD float64 `json:"convertible_usd"`
	SharePct       float64 `json:"share_of_addressable_pct"`
	Pings          int64   `json:"pings"`
	// omitempty on the three dollar fields, so an unpriced ladder puts no `0` on the wire at all.
	// "A field that exists gets rendered" is this project's own rule, and a 0 that means "no rate
	// on the operator's list" is the shape every zero-as-a-measurement defect on this dashboard has
	// had. The UI gates on `priced` as well; both, because either alone has failed before.
	PingUSD  float64 `json:"ping_usd,omitempty"`
	SavedUSD float64 `json:"saved_usd,omitempty"`
	NetUSD   float64 `json:"net_usd,omitempty"`
	Current  bool    `json:"current,omitempty"`
}

// KeepAliveCalc is the calculator's whole answer.
type KeepAliveCalc struct {
	IdleSeconds float64 `json:"idle_seconds"`
	Prefix      int64   `json:"prefix_tokens"`
	Model       string  `json:"model"`
	// Priced false means the model has no rate on the operator's list. Then EVERY dollar field
	// is omitted and the panel says "not priced". It does NOT fall back to a blended average:
	// that is the defect class this project has hit five times.
	Priced bool `json:"priced"`
	// PingUSDEach is the cost of one ping on this prefix: prefix at the cache-READ rate plus
	// the single output token. AvoidedUSDEach is what one converted miss is worth: the
	// avoidable WRITE PREMIUM, (cache_write - cache_read) x prefix — not the whole miss.
	PingUSDEach    float64   `json:"ping_usd_each,omitempty"`
	AvoidedUSDEach float64   `json:"avoided_usd_each,omitempty"`
	Addressable    int64     `json:"addressable_misses"`
	AddressableUSD float64   `json:"addressable_usd"`
	Rows           []CalcRow `json:"rows"`
	// PrefixSource says where the prefill came from, so nobody reads a number as measured when
	// it was typed: "session", "account_median" or "given".
	PrefixSource string `json:"prefix_source,omitempty"`
	KeepAliveCoverage
}

// KeepAliveCalc replays the account's own gaps against the K ladder at one idle interval.
//
// Everything is priced from the ROW'S OWN MODEL rates, per the operator's price list. The one
// number that is not the account's own is the prefix, which the caller supplies — because
// per-ping cost is bimodal (p50 $0.0004, p99 $0.2275) and there is no honest average of it.
func (d *DB) KeepAliveCalc(f Filter, idleSeconds float64, prefix int64, model string,
	price func(string) (modelinfo.Price, bool), currentPings int) (*KeepAliveCalc, error) {
	if idleSeconds <= 0 {
		idleSeconds = 280
	}
	out := &KeepAliveCalc{IdleSeconds: idleSeconds, Prefix: prefix, Model: model, Rows: []CalcRow{}}
	var p modelinfo.Price
	if price != nil && model != "" {
		if pr, ok := price(model); ok && !pr.Zero() {
			p, out.Priced = pr, true
		}
	}
	// The account's own spans and its own addressable gaps, one read each.
	aCond, aArgs := addressable(f)
	var gaps []float64
	var usd []float64
	rows, err := d.sql.Query(aCond+` SELECT gap_s, cost_usd FROM addressable`, aArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g, u float64
		if err := rows.Scan(&g, &u); err != nil {
			rows.Close()
			return nil, err
		}
		gaps = append(gaps, g)
		usd = append(usd, u)
		out.Addressable++
		out.AddressableUSD += u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// EVERY idle span the gate admits, not only the ones that expired: the ping cost is paid on
	// all of them, and counting only the spans that paid off is how a calculator flatters its own
	// feature. Session-final spans included — see pingSpans, and see the PINGS column's own note
	// on the panel, which says which pings are counted.
	spans, err := d.pingSpans(f, kaGateMinPrefix)
	if err != nil {
		return nil, err
	}
	if out.Priced {
		out.PingUSDEach = float64(prefix)*p.CacheRead + p.Output
		out.AvoidedUSDEach = float64(prefix) * (p.CacheWrite - p.CacheRead)
	}
	for k := 1; k <= 4; k++ {
		row := CalcRow{MaxPings: k, Coverage: CoverageSeconds(idleSeconds, k),
			Current: k == currentPings}
		for i, g := range gaps {
			if g > idleSeconds && g <= row.Coverage {
				row.Convertible++
				row.ConvertibleUSD += usd[i]
			}
		}
		if out.AddressableUSD > 0 {
			row.SharePct = 100 * row.ConvertibleUSD / out.AddressableUSD
		}
		for _, sp := range spans {
			row.Pings += int64(sp.pings(idleSeconds, k))
		}
		if out.Priced {
			row.PingUSD = float64(row.Pings) * out.PingUSDEach
			row.SavedUSD = float64(row.Convertible) * out.AvoidedUSDEach
			row.NetUSD = row.SavedUSD - row.PingUSD
		}
		out.Rows = append(out.Rows, row)
	}
	cov, err := d.keepAliveCoverage(f)
	if err != nil {
		return nil, err
	}
	out.KeepAliveCoverage = *cov
	return out, nil
}

// AccountMedianPrefix is the account's own median billed prefix at a lapsed entry, and the model
// most of those lapses were on. What the calculator prefills when no session is selected.
//
// The MODEL matters as much as the size: a rate belongs to a model, and without one the panel
// reported "not priced" on a deployment whose price list was perfectly good. The modal model of
// this account's own expiries is an honest answer to "what would this cost me"; a blended
// average across models is not, and is the defect class this project has hit five times.
// Returns 0 and "" when the account has no addressable expiry.
func (d *DB) AccountMedianPrefix(f Filter) (int64, string, error) {
	aCond, aArgs := addressable(f)
	rows, err := d.sql.Query(aCond+` SELECT COALESCE(prev_prefix,0), model FROM addressable`, aArgs...)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var xs []float64
	byModel := map[string]int{}
	for rows.Next() {
		var v float64
		var m sql.NullString
		if err := rows.Scan(&v, &m); err != nil {
			return 0, "", err
		}
		xs = append(xs, v)
		if m.String != "" {
			byModel[m.String]++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	// Ties broken by name, so the same window gives the same answer twice.
	var best string
	for _, m := range sortedStrings(byModel) {
		if best == "" || byModel[m] > byModel[best] {
			best = m
		}
	}
	return int64(pctlF(xs, 0.50)), best, nil
}

// sortedStrings is the keys of a map in a stable order.
func sortedStrings[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LastBilledPrefix is one session's most recent billed prefix and model, from AGENT rows only.
// The number a person is shown before authorizing an override on that session.
func (d *DB) LastBilledPrefix(f Filter, session string) (int64, string, error) {
	cond, args := f.where()
	var prefix sql.NullInt64
	var model sql.NullString
	err := d.sql.QueryRow(`SELECT r.cache_read + r.cache_write, r.model FROM requests r
		WHERE `+cond+` AND r.session_id = ? ORDER BY r.ts DESC, r.id DESC LIMIT 1`,
		append(append([]any(nil), args...), session)...).Scan(&prefix, &model)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return prefix.Int64, model.String, err
}

// The recommendation's admission rule. All three must hold, and condition 3 does the work.
const (
	// recMinMisses is the addressable-expiry floor. Below it a bootstrap over sessions has
	// nothing to resample and its interval is an artefact.
	recMinMisses = 20
	// recMinRequests is the traffic floor.
	recMinRequests = 200
	// recBootstrapDraws and recBootstrapAlpha are the interval: 2,000 resamples over SESSIONS
	// (not over requests — the unit of correlation is the session), 90% two-sided.
	recBootstrapDraws = 2000
	recBootstrapAlpha = 0.05
	// recRecoverableShare is the documented share of an addressable miss's billed cost that is
	// the avoidable WRITE PREMIUM. Measured at 78.4% on the production corpus. Used only here,
	// where a per-row model rate is not available for every row of a resample.
	recRecoverableShare = 0.784
)

// KeepAliveRecommendation is either a recommendation with its interval and its n, or a refusal
// with the count that caused it. Never both, and there is no point estimate on the wire AT ALL.
//
// The missing field is deliberate and it is the whole design of this payload. Every account in
// the production corpus has a 90% interval whose relative half-width is at least 62%, and 5 of
// 12 cross zero — the account with 89 addressable expiries cannot pin its own figure closer
// than [+$9, +$48]. A `point_estimate` field would be rendered as a number by somebody, some
// day, so it does not exist.
type KeepAliveRecommendation struct {
	// Refused is the reason, when there is one. Its presence is the branch.
	Refused string `json:"refused,omitempty"`
	// IdleSeconds and MaxPings are the recommendation. K = 1 is NEVER returned, and the reason
	// is REACH, not a loss: coverage is K*X + TTL, so one ping reaches 580 s against two pings'
	// 860 s. The "4.7 minutes / -$71 service-wide" this comment used to give is the K*X coverage
	// error CoverageSeconds exists to refuse — under correct coverage K=1 is +$101.56 in the
	// adjudicated sweep and K=2 is +$170.08, so K=2 dominates on size and not on sign.
	IdleSeconds int `json:"idle_seconds,omitempty"`
	MaxPings    int `json:"max_pings,omitempty"`
	// LoUSD/HiUSD is the 90% interval over a window like this one. A RANGE, always, and the UI
	// renders it as "$lo – $hi" and never as a hero figure.
	LoUSD float64 `json:"lo_usd,omitempty"`
	HiUSD float64 `json:"hi_usd,omitempty"`
	// N is the addressable expiries the interval rests on and Sessions how many sessions they
	// came from. Shown beside the range, because a range without its n is not honest either.
	N        int64 `json:"n"`
	Sessions int64 `json:"sessions"`
	Requests int64 `json:"requests"`
	// AltMaxPings is offered only where the account's own convertible count rises by more than
	// its bootstrap's own noise between K=2 and K=3. On the production corpus that is nobody,
	// and the panel says K=2 and K=3 are a measured tie.
	AltMaxPings int `json:"alt_max_pings,omitempty"`
	// ServiceLoUSD/ServiceHiUSD is the service-wide interval, for scale in a refusal.
	ServiceLoUSD float64 `json:"service_lo_usd,omitempty"`
	ServiceHiUSD float64 `json:"service_hi_usd,omitempty"`
	KeepAliveCoverage
}

// The shipped policy, which is also what is recommended when the rule admits an account.
const (
	recIdleSeconds = 280
	recMaxPings    = 2
)

// The service-wide interval, for scale inside a refusal. Measured by the adjudicated bootstrap
// over 357 addressable misses across the 4.47-day production window; quoted, not recomputed,
// because it is a property of that measurement and not of the caller's filter.
const (
	serviceLoUSD = 95.0
	serviceHiUSD = 237.0
)

// KeepAliveRecommend answers "what should I set?" — or refuses.
func (d *DB) KeepAliveRecommend(f Filter) (*KeepAliveRecommendation, error) {
	out := &KeepAliveRecommendation{ServiceLoUSD: serviceLoUSD, ServiceHiUSD: serviceHiUSD}
	cov, err := d.keepAliveCoverage(f)
	if err != nil {
		return nil, err
	}
	out.KeepAliveCoverage = *cov
	out.Requests = cov.Requests

	// The account's own addressable expiries, grouped by session — the resampling unit.
	aCond, aArgs := addressable(f)
	rows, err := d.sql.Query(aCond+`
		SELECT session_id, gap_s, cost_usd FROM addressable`, aArgs...)
	if err != nil {
		return nil, err
	}
	type miss struct {
		gap, usd float64
	}
	bySession := map[string][]miss{}
	for rows.Next() {
		var s string
		var m miss
		if err := rows.Scan(&s, &m.gap, &m.usd); err != nil {
			rows.Close()
			return nil, err
		}
		bySession[s] = append(bySession[s], m)
		out.N++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Sessions = int64(len(bySession))

	if out.N < recMinMisses {
		out.Refused = fmt.Sprintf("you have %d cache expiries large enough to act on in this "+
			"window; below %d we cannot tell a real saving from noise", out.N, recMinMisses)
		return out, nil
	}
	if out.Requests < recMinRequests {
		out.Refused = fmt.Sprintf("this window holds %d of your requests; below %d there is not "+
			"enough history for an interval to mean anything", out.Requests, recMinRequests)
		return out, nil
	}

	// The per-session ping cost of the SAME policy, so the resample scores a net and not a
	// saving. Ping cost is derived from the account's own median back-derived input rate,
	// applied to the prefix that lapsed — the same simplification the adjudicated per-account
	// bootstrap used, and it measures the SPREAD rather than competing with the shipped
	// replay's own figure.
	pingUSD, err := d.medianPingUSD(f)
	if err != nil {
		return nil, err
	}
	spans, err := d.pingSpans(f, kaGateMinPrefix)
	if err != nil {
		return nil, err
	}
	bySpan := map[string][]pingSpan{}
	for _, sp := range spans {
		bySpan[sp.session] = append(bySpan[sp.session], sp)
	}
	// The resampling unit is EVERY session in the window, not only the ones that expired.
	//
	// Resampling only the sessions with an addressable expiry drew the saving side and the cost
	// side from different populations: it charged ping cost over 53 of 3,891 sessions and omitted
	// 42.4% of even the intra-session pings, on top of every session-final one. Both omissions
	// push the interval UP — the direction that makes the rule fire when it should refuse — and
	// condition 3 is the load-bearing test that decides whether this is recommended at all.
	// Resampling over sessions with zero expiries is exactly how a bootstrap sees the cost side.
	all := map[string]bool{}
	for s := range bySession {
		all[s] = true
	}
	for s := range bySpan {
		all[s] = true
	}
	keys := make([]string, 0, len(all))
	for s := range all {
		keys = append(keys, s)
	}
	sort.Strings(keys) // deterministic draw order for a given seed
	net := func(s string, k int) float64 {
		cover := CoverageSeconds(recIdleSeconds, k)
		var v float64
		for _, m := range bySession[s] {
			if m.gap > recIdleSeconds && m.gap <= cover {
				v += m.usd * recRecoverableShare
			}
		}
		for _, sp := range bySpan[s] {
			v -= float64(sp.pings(recIdleSeconds, k)) * pingUSD
		}
		return v
	}
	lo, hi := bootstrapCI(keys, func(s string) float64 { return net(s, recMaxPings) })
	// Condition 3, and it is a ONE-SIDED test. An interval that spans zero cannot tell the
	// effect from nothing; an interval entirely BELOW zero is worse than inconclusive — this
	// account's own gaps say the mechanism would cost it money — and both are refusals. The
	// two-sided reading of "excludes zero" would have recommended a policy whose own 90%
	// interval was -$39 to -$5, which is what the first live look at this route produced: an
	// account whose median idle gap is 57 minutes has almost nothing inside any coverage worth
	// having, so it pays for pings and converts nothing.
	if lo <= 0 {
		if hi < 0 {
			out.Refused = fmt.Sprintf("your own history says this would COST you money: over %d "+
				"cache expiries in %d sessions the 90%% interval is $%.2f to $%.2f, entirely "+
				"below zero. Your idle gaps are mostly too long for any setting worth having to "+
				"reach", out.N, out.Sessions, lo, hi)
		} else {
			out.Refused = fmt.Sprintf("your own history cannot tell this apart from zero: over %d "+
				"cache expiries in %d sessions the 90%% interval is $%.2f to $%.2f, which "+
				"includes no change at all", out.N, out.Sessions, lo, hi)
		}
		return out, nil
	}
	out.IdleSeconds, out.MaxPings = recIdleSeconds, recMaxPings
	out.LoUSD, out.HiUSD = lo, hi
	// K=3 is offered only where it beats K=2 by more than this account's OWN noise — its
	// interval has to clear K=2's entirely. Anything weaker than that is the comparison this
	// project has got wrong five times: two numbers whose intervals overlap are a tie, and on
	// the production corpus K=2 and K=3 ARE a tie for every account. The panel says so.
	if lo3, _ := bootstrapCI(keys, func(s string) float64 { return net(s, 3) }); lo3 > hi {
		out.AltMaxPings = 3
	}
	return out, nil
}

// bootstrapCI resamples the units WITH REPLACEMENT and returns the 90% interval of the total.
//
// The unit is the SESSION, not the request: expiries inside one session are not independent
// draws — they share a prefix, a working pattern and an agent — so resampling requests would
// report an interval several times too narrow. That is the specific error this project has been
// bitten by five times, at a smaller scale each time.
//
// Deterministic: a fixed seed, so the same window gives the same interval twice. An interval
// that moves when the page is refreshed is one nobody can act on.
func bootstrapCI(units []string, value func(string) float64) (lo, hi float64) {
	n := len(units)
	if n == 0 {
		return 0, 0
	}
	vals := make([]float64, n)
	for i, u := range units {
		vals[i] = value(u)
	}
	rng := rand.New(rand.NewSource(1))
	totals := make([]float64, recBootstrapDraws)
	for d := 0; d < recBootstrapDraws; d++ {
		var sum float64
		for i := 0; i < n; i++ {
			sum += vals[rng.Intn(n)]
		}
		totals[d] = sum
	}
	sort.Float64s(totals)
	at := func(p float64) float64 {
		i := int(float64(len(totals)-1) * p)
		return totals[i]
	}
	return at(recBootstrapAlpha), at(1 - recBootstrapAlpha)
}

// medianPingUSD is the account's own median cost of one ping on the prefix that lapses,
// back-derived from what its requests actually paid. Used only by the bootstrap, which needs a
// per-session cost and cannot carry a per-row model rate through a resample.
func (d *DB) medianPingUSD(f Filter) (float64, error) {
	aCond, aArgs := addressable(f)
	// cost_usd on an addressable miss is dominated by the re-creation of prev_prefix at 1.25x
	// base input, so cost/prefix/1.25 recovers the base rate and 0.1x of it is the read.
	rows, err := d.sql.Query(aCond+`
		SELECT cost_usd, COALESCE(prev_prefix,0) FROM addressable WHERE prev_prefix > 0`, aArgs...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var xs []float64
	for rows.Next() {
		var usd, prefix float64
		if err := rows.Scan(&usd, &prefix); err != nil {
			return 0, err
		}
		if prefix > 0 && usd > 0 {
			xs = append(xs, usd/1.25*0.1)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return pctlF(xs, 0.50), nil
}

// trimZero renders a number to at most one decimal place and drops a trailing ".0", so a band
// edge reads as "9.7m" rather than "9.666666666666666m" and as "5m" rather than "5.0m".
func trimZero(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
