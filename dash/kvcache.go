package dash

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The KV-cache tab's read side: turn stored request rows into the analysis dataset package
// kvcache works on, and aggregate that dataset for the page.
//
// # Where the arithmetic is, and where it is not
//
// Nothing here prices anything or decides anything. The dataset is DERIVED here because the
// derivation is a SQL window function over the store; every dollar and every policy is in
// package kvcache, which the request path will one day share. The browser gets numbers, enum
// labels and ids, and does no arithmetic at all — the same split the keep-alive tab keeps, for
// the same reason: the browser has twice duplicated a table the server owns and drifted from it.
//
// # The one thing this file gets right that a naive version would not
//
// A filter is applied in TWO PLACES, deliberately, and which predicate goes where changes the
// answer:
//
//   - The predicates that select WHICH CONVERSATIONS are in scope — the tenant, the time
//     window, one session, the exclusion of ping rows — run INSIDE the window function. They
//     remove whole conversations or bound the window, and neither can corrupt a remaining
//     conversation's successor.
//   - Every predicate that selects WHICH REQUESTS TO SHOW — model, provider, agent, TTL tier,
//     time-of-day, has-a-successor — runs OUTSIDE it, over the already-derived rows. Running
//     `model = X` inside the partition would make a request's successor the next request ON
//     THAT MODEL, which is not the next request in the conversation. On the production corpus
//     that is not a corner case: 121 of 1,772 conversations use more than one model and they
//     hold 12,035 of 14,407 requests, so the naive version overstates the idle time of five
//     sixths of the traffic.
//
// The outer predicate is the shared Filter.where() applied to the CTE aliased as `r`, so every
// dimension the rest of the dashboard supports works here for free AND the tenant predicate is
// re-applied over the derived rows — the same guard twice, which is the right number of times
// for the one that keeps accounts apart.

// kvCacheMaxRows bounds one analysis read.
//
// The point of this dataset is that percentiles, a survival curve and a chronological replay
// all need the ROWS rather than a pre-aggregate, so there is a real ceiling on how much history
// one request can answer over — and it is stated rather than discovered. At the production
// corpus's density (14,407 rows over 57 hours) this is roughly a year of one deployment's
// traffic. Past it the read keeps the NEWEST rows and every payload carries scanned, total and
// truncated: a silent cap reads as "this is your whole history", which is the one thing a
// savings figure must never imply.
// A var rather than a const so a test can lower it: the truncation branch decides what `total`
// means and whether the extra row is trimmed, and a branch that only executes above 200,000 rows
// is a branch no test reaches.
var kvCacheMaxRows = 200_000

// kvCacheMaxConcurrent bounds how many analysis reads run at once, which is the half of the
// ceiling kvCacheMaxRows does not cover.
//
// kvCacheMaxRows bounds ONE request: ~135 MB of live heap and several seconds at the cap. The
// store's pool is bounded (dash/store.go's SetMaxOpenConns), but that caps CONNECTION count, not
// per-request memory — a kvcache analysis is heavy enough that even a handful of them running at
// once, well under the pool's own cap, is what OOMs: eight concurrent analyses measured at 1.65 GB
// resident and 24 s each, which OOMs a 2 GB container. A dashboard reader holding the refresh key
// is enough, and on a single-tenant deployment no credential is needed to do it.
//
// Two, deliberately, because the number is a memory budget rather than a throughput choice:
// 2 x 135 MB is a bound worth stating, and almost every real window is far below the cap so the
// queue is rarely reached. A waiter whose request has been abandoned leaves the queue instead of
// entering it, so the slot goes to somebody still listening.
const kvCacheMaxConcurrent = 2

// kvCacheSlots is that bound. Package-level because the resource being protected is the process's
// memory, not any one account's quota.
var kvCacheSlots = make(chan struct{}, kvCacheMaxConcurrent)

// acquireKVCache waits for a slot, or gives up if the caller has already gone.
func acquireKVCache(ctx context.Context) error {
	select {
	case kvCacheSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseKVCache returns a slot.
func releaseKVCache() { <-kvCacheSlots }

// idleEdges are the idle-gap histogram's fixed edges, in seconds; the last band is unbounded.
//
// Fine at the bottom and coarse at the top, because that is where the data is: the production
// corpus's median gap is 14.9 s and its 90th percentile is 106 s, so a first bucket of
// "0–5 minutes" would put 95% of the traffic in one bar and say nothing. The 300 s and 3600 s
// edges are LOAD-BEARING — they are the two provider tiers, so a band boundary has to fall
// exactly on each or the page's own percentages would not line up with its own chart.
// TestIdleHistogramEdgesLineUpWithTheHorizons is that assertion.
var idleEdges = []float64{0, 10, 30, 60, kvcache.Horizon5m.Seconds(), 900,
	kvcache.Horizon1h.Seconds(), 21600}

// survivalLadder are the elapsed times the survival view reports, in seconds. Both provider
// horizons are rungs BY CONSTRUCTION rather than by interpolation between neighbours, because
// those two are the numbers a TTL is actually chosen against.
var survivalLadder = []float64{5, 10, 30, 60, 120, kvcache.Horizon5m.Seconds(), 600, 1800,
	kvcache.Horizon1h.Seconds(), 7200, 21600, 86400}

// KVCacheOptions are the narrowings that exist only on this page, plus the table's paging.
//
// They are separate from Filter because all three narrowings are DERIVED — a reconstructed
// tier, a time-of-day band, and whether a successor exists at all — and none of them is a
// column any other view could filter on.
type KVCacheOptions struct {
	Limit  int
	Offset int
	// Sort is a key of kvCacheSortKeys; Dir is "asc" or "desc".
	Sort string
	Dir  string
	// Bucket is a kvcache.Bucket name, or "" for every band. UTC.
	Bucket string
	// TTL is "none", string(kvcache.TTL5m), string(kvcache.TTL1h), or "" for every tier. It
	// selects on the RECONSTRUCTED tier, not on the raw column — see observedTTL.
	TTL string
	// HasNext is "yes" (requests followed by another in the same conversation), "no" (the last
	// request of a conversation), or "" for both.
	//
	// Its own dimension rather than a sentinel on some other field, because "the requests
	// nobody came back to" is a population an operator asks about directly — and it is the
	// population every average on this page has to exclude.
	HasNext string
}

// kvCacheSortKeys maps a sort key to its ORDER BY body, with one %s for the direction.
//
// A closed map, and kvCacheOptionsFrom rejects anything not in it, because the value arrives
// from a query string and lands in SQL.
//
// `idle` is the one entry that is not a bare column, and the reason is honesty rather than
// SQL: a request with no successor has NO idle time, so it must sort LAST in BOTH directions.
// A plain ORDER BY on a nullable column puts nulls at one end ascending and the other end
// descending, which in the ascending case parades every final request at the top of the table
// as though it had returned instantly. The null-ness is therefore the PRIMARY key of the sort,
// always ascending, and the value only breaks ties within it.
var kvCacheSortKeys = map[string]string{
	"ts":           "r.ts %[1]s, r.id %[1]s",
	"user":         "r.tenant_id %s, r.ts DESC",
	"conversation": "r.session_id %s, r.ts DESC",
	"model":        "r.model %s, r.ts DESC",
	"input":        "r.fresh_input %s",
	"output":       "r.output_tokens %s",
	"read":         "r.cache_read %s",
	"write":        "r.cache_write %s",
	"cached":       "(r.cache_read + r.cache_write) %s",
	"ttl":          "r.cache_ttl %s, r.ts DESC",
	"hit":          "(r.cache_miss_reason = 'hit') %s, r.cache_miss_reason ASC",
	"idle":         "(r.next_ts IS NULL) ASC, (r.next_ts - r.ts) %s",
	"cost":         "r.cost_usd %s",
}

// observedTTL reconstructs a request's prompt-cache tier and says HOW WELL IT IS KNOWN.
//
// Three states, never two, and the third is the whole reason this function exists. `cache_ttl`
// arrived as an ADDITIVE column with DEFAULT ”, so a blank on an old row means NOT RECORDED —
// and a request that genuinely carried no cache_control also reads blank. Two pieces of
// evidence separate them:
//
//   - the provider billed part of the write at the one-hour tier (cache_write_1h > 0), which is
//     the only proof a requested 1h was honoured rather than silently downgraded. That is an
//     OBSERVED tier: not on the row, but deducible from what was billed.
//   - nothing was billed at any cache tier at all, which is a request that really did cache
//     nothing. Also observed.
//
// What is left cached something at a tier nobody wrote down. It is reported as UNKNOWN and
// taken as the provider's five-minute default so a simulation has something to replay — and the
// count is on the coverage panel, and the observed-policy arm reports how much of itself rested
// on it. An unrecognised recorded value ('ephemeral_10m' from a future build) is treated the
// same way: not trusted, never coerced into the tier it superficially resembles.
func observedTTL(recorded string, write1h, cached int64) (kvcache.TTL, string) {
	if t := kvcache.TTL(recorded); t == kvcache.TTL5m || t == kvcache.TTL1h {
		return t, kvcache.TTLSourceConfigured
	}
	if write1h > 0 {
		return kvcache.TTL1h, kvcache.TTLSourceObserved
	}
	if cached > 0 {
		return kvcache.TTL5m, kvcache.TTLSourceUnknown
	}
	return kvcache.TTLNone, kvcache.TTLSourceObserved
}

// ttlPredicate is observedTTL as SQL, so the tier filter narrows in the database instead of
// dragging every row into Go to be thrown away.
//
// It sits directly beneath observedTTL because the two are one definition in two languages, and
// TestTheTierFilterAgreesWithTheTierReconstruction asserts they agree over every combination
// rather than leaving the reader to compare them by eye.
func ttlPredicate(tier string) string {
	const notRecorded = `r.cache_ttl <> 'ephemeral_5m' AND r.cache_ttl <> 'ephemeral_1h'`
	switch tier {
	case string(kvcache.TTL5m):
		// The row's OWN tier only. A request that recorded nothing is replayed AS IF it were
		// five minutes, but it is not evidence of a five-minute policy, so it is not in this
		// group — see TTLUnrecorded.
		return `r.cache_ttl = 'ephemeral_5m'`
	case string(kvcache.TTL1h):
		return `(r.cache_ttl = 'ephemeral_1h' OR (` + notRecorded + ` AND r.cache_write_1h > 0))`
	case "none":
		return `(` + notRecorded + ` AND r.cache_write_1h = 0 AND (r.cache_read + r.cache_write) = 0)`
	case TTLUnrecorded:
		return `(` + notRecorded + ` AND r.cache_write_1h = 0 AND (r.cache_read + r.cache_write) > 0)`
	}
	return ""
}

// TTLUnrecorded is the filter value and the group key for requests that cached something at a
// tier NOBODY WROTE DOWN.
//
// It is a fourth group rather than a footnote on the five-minute one, and that is the whole
// honesty rule of this page applied to its own grouped table. Those rows are REPLAYED as five
// minutes so a simulation has something to run, but they are not evidence of a five-minute
// policy — and folding them in said "3,106 of your requests used the 5-minute tier" about a
// window where 295 of them recorded no tier at all, under a heading that called the group
// "configured". The coverage banner above it was reporting the same 295 as not recorded, so the
// page contradicted itself in two panels.
const TTLUnrecorded = "unrecorded"

// kvCacheScope splits one Filter into the two predicates described at the top of this file.
//
// The inner filter keeps ONLY the conversation-defining dimensions, copied field by field so
// that a dimension added to Filter later lands OUTSIDE by default. That is the safe direction: a
// new predicate applied outside narrows the rows shown, while the same predicate applied inside
// would silently change every idle time on the page.
func kvCacheScope(f Filter) Filter {
	return Filter{
		Since: f.Since, Until: f.Until,
		// Both, verbatim: TenantAll is what makes "" mean "every account" rather than "the
		// single-tenant one", so carrying one without the other would widen the scope of the
		// read that decides which conversations exist.
		Tenant: f.Tenant, TenantAll: f.TenantAll,
		Session:       f.Session,
		WithKeepAlive: f.WithKeepAlive,
	}
}

// kvCacheCTE is the window function. The LEAD is partitioned by (tenant_id, session_id, model)
// and ordered by (ts, id).
//
// All THREE columns, and the model is the one that is easy to leave out. It has to be here
// because the partition must be exactly kvcache.Conversation — a cache entry does not transfer
// between models, so an opus request cannot read a sonnet request's entry, and linking the two as
// successor grants the second a hit at 0.1x on something it could never have matched. The bias is
// one-directional: every arm looks cheaper and hits more often than it can. On this deployment's
// corpus that is 124 extra trajectories (1,896 keyed with the model against 1,772 without) and it
// reaches the 12,035 requests that sit in a session using more than one model.
//
// The tenant is here because a session id is CLIENT-SUPPLIED, so two accounts can present the
// same one; on the session alone this derives a gap across an account boundary, which is both a
// wrong measurement and a cross-account read. And the id breaks the ORDER BY tie because 9 of the
// corpus's 12,635 consecutive pairs share a millisecond — without it the successor of such a row
// is whichever the planner happened to emit, so the same window would derive two datasets.
//
// This partition and kvcache.Conversation are one definition in two languages;
// TestTheSQLPartitionIsExactlyTheConversationKey is the assertion that they agree.
const kvCacheCTE = `WITH s AS (SELECT r.id, r.ts, r.tenant_id, r.session_id, r.model,
		r.provider, r.agent, r.preset, r.mode, r.token_accounting, r.reasoning_effort,
		r.thinking_mode, r.stop_reason, r.uncompressed_reason, r.cache_ttl, r.keepalive,
		r.fresh_input, r.output_tokens, r.cache_read, r.cache_write, r.cache_write_1h,
		r.cache_miss_reason, r.cost_usd, r.upstream_ms,
		LEAD(r.ts) OVER w AS next_ts, LEAD(r.id) OVER w AS next_id
	FROM requests r WHERE %s
	WINDOW w AS (PARTITION BY r.tenant_id, r.session_id, r.model ORDER BY r.ts, r.id))`

// kvCacheQuery renders the CTE plus the outer predicate for one filter, one set of options and
// one projection.
//
// The projection is a PARAMETER rather than a %s left in the string for a later Sprintf, and
// that is not style: the time-of-day predicate contains a strftime('%H'), and a second Sprintf
// over the assembled query consumed it as a verb — turning every bucket filter into a
// comparison against the literal "%!H", which matched nothing and looked exactly like a quiet
// afternoon. There is one format step here and it happens before any SQL text is appended.
func kvCacheQuery(f Filter, o KVCacheOptions, projection string) (string, []any) {
	inCond, inArgs := kvCacheScope(f).where()
	outCond, outArgs := f.where()
	conds, derivedArgs := kvCacheDerivedPreds(o)
	q := strings.Replace(kvCacheCTE, "%s", inCond, 1) +
		" SELECT " + projection + " FROM s r WHERE " + outCond
	for _, c := range conds {
		q += " AND " + c
	}
	args := append(append([]any(nil), inArgs...), outArgs...)
	return q, append(args, derivedArgs...)
}

// kvCacheDerivedPreds renders the three page-only narrowings.
//
// The hour range is BOUND rather than interpolated. It comes from a closed table and could not
// carry an injection, but a bound parameter cannot be eaten by a format verb either, which is
// the failure this shape prevents.
func kvCacheDerivedPreds(o KVCacheOptions) ([]string, []any) {
	var out []string
	var args []any
	if lo, hi, ok := bucketHours(kvcache.Bucket(o.Bucket)); ok {
		out = append(out, "CAST(strftime('%H', r.ts/1000, 'unixepoch') AS INTEGER) BETWEEN ? AND ?")
		args = append(args, lo, hi)
	}
	if p := ttlPredicate(o.TTL); p != "" {
		out = append(out, p)
	}
	switch o.HasNext {
	case "yes":
		out = append(out, "r.next_ts IS NOT NULL")
	case "no":
		out = append(out, "r.next_ts IS NULL")
	}
	return out, args
}

// bucketHours is one time-of-day band's UTC hour range, inclusive.
func bucketHours(b kvcache.Bucket) (lo, hi int, ok bool) {
	switch b {
	case kvcache.BucketNight:
		return 0, 5, true
	case kvcache.BucketMorning:
		return 6, 11, true
	case kvcache.BucketAfternoon:
		return 12, 17, true
	case kvcache.BucketEvening:
		return 18, 23, true
	}
	return 0, 0, false
}

// kvCacheCols is the row projection, in scan order.
const kvCacheCols = `r.id, r.ts, r.tenant_id, r.session_id, r.model, r.provider, r.agent,
	r.cache_ttl, r.fresh_input, r.output_tokens, r.cache_read, r.cache_write, r.cache_write_1h,
	r.cache_miss_reason, r.token_accounting, r.cost_usd, r.upstream_ms, r.keepalive,
	r.stop_reason, r.next_ts, r.next_id`

// scanKVCacheRequest reads one row and fills everything derived from it.
//
// The idle fields come from the SQL LEAD rather than from kvcache.Derive, and that is not a
// duplication of the derivation — it is the only correct source here. Derive works from a row's
// NEIGHBOUR in the slice, so over a FILTERED or CAPPED read it would treat whichever row
// happened to be next as the successor, and treat the oldest kept row's real predecessor as
// absent. The window function saw the whole conversation.
func scanKVCacheRequest(rows interface{ Scan(...any) error }) (*kvcache.Request, error) {
	var r kvcache.Request
	var cacheTTL, missReason, accounting string
	var nextTS, nextID sql.NullInt64
	var keepAlive int
	if err := rows.Scan(&r.ID, &r.TS, &r.User, &r.ConversationID, &r.Model, &r.Provider,
		&r.Agent, &cacheTTL, &r.InputTokens, &r.OutputTokens, &r.CacheRead, &r.CacheWrite,
		&r.CacheWrite1h, &missReason, &accounting, &r.CostUSD, &r.UpstreamMs, &keepAlive,
		&r.StopReason, &nextTS, &nextID); err != nil {
		return nil, err
	}
	r.KeepAlive = keepAlive != 0
	r.MissReason = missReason
	r.Hit = missReason == CacheHit
	r.CachedContext = r.CacheRead + r.CacheWrite
	r.HourUTC = time.UnixMilli(r.TS).UTC().Hour()
	r.Bucket = kvcache.BucketOf(r.HourUTC)
	r.TTL, r.TTLSource = observedTTL(cacheTTL, r.CacheWrite1h, r.CachedContext)
	// An incomplete-accounting row has NO cost. cost_usd is 0 on it and 0 is also a legitimate
	// cost, so the accounting column is the only thing that can tell the two apart.
	r.CostKnown = accounting == AccountingComplete
	if !r.CostKnown {
		r.CostUSD = 0
	}
	if nextTS.Valid {
		idle := nextTS.Int64 - r.TS
		if idle < 0 {
			idle = 0 // impossible after the CTE's ORDER BY; clamped rather than trusted
		}
		v := idle
		r.HasNext, r.NextTS, r.NextID, r.IdleMs = true, nextTS.Int64, nextID.Int64, &v
		r.Within5m = time.Duration(idle)*time.Millisecond <= kvcache.Horizon5m
		r.Within1h = time.Duration(idle)*time.Millisecond <= kvcache.Horizon1h
	}
	return &r, nil
}

// KVCacheDataset reads the derived dataset for one filter, CHRONOLOGICALLY, and reports how
// many rows matched.
//
// Chronological regardless of the table's own sort, because the replay walks it in wall-clock
// order and the table's sort is a view of the same rows. Over the cap the NEWEST rows are kept:
// an analysis of the recent past is useful, and one that silently stopped at some point in the
// middle of the window is not.
func (d *DB) KVCacheDataset(f Filter, o KVCacheOptions) ([]*kvcache.Request, int64, error) {
	// One row past the cap, so the read itself says whether it was truncated. The COUNT below is
	// a SECOND complete pass over the same window function, and it is only needed when the answer
	// is "yes" — on every untruncated window, which is nearly all of them, the count is exactly
	// the number of rows returned. It was costing 0.76 s of a 7 s request to learn something the
	// read already knew.
	q, args := kvCacheQuery(f, o, kvCacheCols)
	q += " ORDER BY r.ts DESC, r.id DESC LIMIT ?"
	rows, err := d.sql.QueryContext(d.readCtx(), q, append(args, kvCacheMaxRows+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*kvcache.Request{}
	for rows.Next() {
		r, err := scanKVCacheRequest(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total := int64(len(out))
	if len(out) > kvCacheMaxRows {
		// Truncated, so the true total needs the count, and the extra row must not be returned.
		out = out[:kvCacheMaxRows]
		if total, err = d.kvCacheCount(f, o); err != nil {
			return nil, 0, err
		}
	}
	// Read newest-first so the cap keeps the newest; handed back oldest-first so the replay can
	// walk it.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, total, nil
}

// kvCacheCount is how many rows match, before the cap and before paging.
func (d *DB) kvCacheCount(f Filter, o KVCacheOptions) (int64, error) {
	q, args := kvCacheQuery(f, o, "COUNT(*)")
	var n int64
	err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// KVCacheRow is one dataset row plus the two ways back out of the analysis.
type KVCacheRow struct {
	*kvcache.Request
	// RequestURL and ConversationURL are the links to the existing request drawer and session
	// diff. Built here rather than in the page because a session id is CLIENT-SUPPLIED text —
	// it can contain a slash, a space, a hash — and a link assembled by string concatenation in
	// JavaScript is where that becomes a broken URL or worse.
	RequestURL      string `json:"request_url"`
	ConversationURL string `json:"conversation_url"`
}

// KVCacheRowPage is one page of the detail table.
type KVCacheRowPage struct {
	Rows []*KVCacheRow `json:"rows"`
	// Total is how many rows match the filter, so the pager can say "51–100 of 14,407".
	Total  int64 `json:"total"`
	Offset int   `json:"offset"`
	Limit  int   `json:"limit"`
	// Truncated says the ANALYSIS over this filter is capped, so the table's own total and the
	// figures above it describe different row counts. On the wire because the pager prints it.
	Truncated bool `json:"truncated"`
}

// KVCacheRows reads one page of the sortable detail table.
//
// Offset paging, not keyset, because this table is SORTABLE on thirteen columns: a keyset
// cursor is a value in the sort order, so every re-sort would invalidate it. The page is
// bounded and the derivation is unaffected by the boundary — the window function runs over the
// whole scoped window inside the CTE and only the outer SELECT is paged, so a row on page 7
// carries the same successor it would on page 1.
func (d *DB) KVCacheRows(f Filter, o KVCacheOptions) (*KVCacheRowPage, error) {
	if o.Limit <= 0 || o.Limit > 500 {
		o.Limit = 50
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	total, err := d.kvCacheCount(f, o)
	if err != nil {
		return nil, err
	}
	dir := "DESC"
	if o.Dir == "asc" {
		dir = "ASC"
	}
	order, ok := kvCacheSortKeys[o.Sort]
	if !ok {
		order = kvCacheSortKeys["ts"]
	}
	q, args := kvCacheQuery(f, o, kvCacheCols)
	q += " ORDER BY " + fmt.Sprintf(order, dir) + " LIMIT ? OFFSET ?"
	rows, err := d.sql.QueryContext(d.readCtx(), q, append(args, o.Limit, o.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &KVCacheRowPage{Rows: []*KVCacheRow{}, Total: total, Offset: o.Offset, Limit: o.Limit,
		Truncated: total > int64(kvCacheMaxRows)}
	for rows.Next() {
		r, err := scanKVCacheRequest(rows)
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, &KVCacheRow{Request: r,
			RequestURL:      fmt.Sprintf("#requests?req=%d", r.ID),
			ConversationURL: "#sessions?diff=" + url.PathEscape(r.ConversationID)})
	}
	return out, rows.Err()
}

// modelsOf is the distinct models in a dataset, sorted, for a price list.
func modelsOf(rows []*kvcache.Request) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range rows {
		if r.Model == "" || seen[r.Model] {
			continue
		}
		seen[r.Model] = true
		out = append(out, r.Model)
	}
	sort.Strings(out)
	return out
}

// ── the aggregates ─────────────────────────────────────────────────────────

// KVCacheCoverage answers "is a zero here a measurement or an absence?".
//
// Every field counts rows that could NOT answer something, and they are on the wire because
// three of them are routinely nonzero on real history: a snapshot written before the cache_ttl
// column records no tier, a row whose token accounting was incomplete has no cost, and a
// conversation with one request in the window contributes a final request and no gap at all. A
// page that rendered those as 5m, $0.00 and 0 s would fabricate three measurements at once.
type KVCacheCoverage struct {
	// The three tier states, counted apart rather than blended. See observedTTL.
	TTLConfigured int64 `json:"ttl_configured"`
	TTLObserved   int64 `json:"ttl_observed"`
	TTLUnknown    int64 `json:"ttl_unknown"`
	// CostUnknown is rows whose accounting was incomplete: counted everywhere, valued nowhere.
	CostUnknown int64 `json:"cost_unknown"`
	// SingleRequestConversations is conversations with exactly one request in this window. On
	// the production corpus that is 1,551 of 1,772, so a page that did not say so would look
	// like it had thrown most of the traffic away.
	SingleRequestConversations int64 `json:"single_request_conversations"`
	// KeepAliveRows is the ping rows this dataset EXCLUDED. A ping is a request context-guru
	// sent while nobody was at the keyboard; counted, it would split one real idle gap into two
	// short ones and make every reuse probability wrong in the flattering direction.
	KeepAliveRows int64 `json:"keepalive_rows"`
}

// KVCacheCards is the summary band.
//
// Every idle figure's denominator is WithNext, never Requests: a conversation's last request has
// no gap, and dividing by Requests would count it as a zero-second return.
type KVCacheCards struct {
	Requests      int64   `json:"requests"`
	Scanned       int64   `json:"scanned"`
	Conversations int64   `json:"conversations"`
	Users         int64   `json:"users"`
	Models        int64   `json:"models"`
	WithNext      int64   `json:"with_next"`
	FinalRequests int64   `json:"final_requests"`
	MedianIdleMs  float64 `json:"median_idle_ms"`
	MeanIdleMs    float64 `json:"mean_idle_ms"`
	P90IdleMs     float64 `json:"p90_idle_ms"`
	Within5m      int64   `json:"within_5m"`
	Within1h      int64   `json:"within_1h"`
	Within5mPct   float64 `json:"within_5m_pct"`
	Within1hPct   float64 `json:"within_1h_pct"`
	Hits          int64   `json:"hits"`
	// HitRatePct is the share the PROVIDER served from cache, as recorded. An observation of
	// what happened, never a simulated figure — the simulated ones live on kvcache.Result.
	HitRatePct  float64 `json:"hit_rate_pct"`
	CostUSD     float64 `json:"cost_usd"`
	CostUnknown int64   `json:"cost_unknown"`
	// CachedContextP50 is the median billed prefix in tokens, over the requests that CACHED
	// SOMETHING — rows with a zero prefix are excluded.
	//
	// The population is the whole point. This figure is what every cache-write, cache-read and
	// keep-alive dollar in the pricing panel is multiplied by, and a request that cached nothing
	// has no prefix to price. Including them does not add noise, it biases: 2,120 of the
	// production corpus's 14,407 rows have a zero prefix, which pulled the median from 147,550
	// down to 124,845 — 18% low on every cost derived from it. A tenant whose traffic is mostly
	// uncached got a median of ZERO, and then a whole table of $0.00 rates rendered as though
	// they were known, which is the failure Result.Valued exists to prevent one layer over.
	//
	// PrefixKnown on the pricing view is the flag that says whether this could be computed at
	// all; see TestTheMedianPrefixPricesOnlyRowsThatCached.
	CachedContextP50 int64 `json:"cached_context_p50"`
}

// KVCacheGroup is the OBSERVED dataset restricted to one user, model, tier or time-of-day band.
//
// Not kvcache.Group, which is a SIMULATED result. The two deliberately do not share a type: with
// the same field names a reader would eventually compare a measurement with a projection.
type KVCacheGroup struct {
	Key           string  `json:"key"`
	Requests      int64   `json:"requests"`
	Conversations int64   `json:"conversations"`
	WithNext      int64   `json:"with_next"`
	FinalRequests int64   `json:"final_requests"`
	MedianIdleMs  float64 `json:"median_idle_ms"`
	MeanIdleMs    float64 `json:"mean_idle_ms"`
	Within5mPct   float64 `json:"within_5m_pct"`
	Within1hPct   float64 `json:"within_1h_pct"`
	// Hits is the numerator of HitRatePct, and it is on the wire for two reasons. A rate without
	// its own count cannot be checked — a share accidentally expressed as a fraction stays inside
	// [0,100] and no range assertion can see it, so only recomputing it from its numerator and
	// denominator catches that — and the page can say "2,599 of 3,391" instead of a bare
	// percentage, which is the standard the rest of this dashboard already keeps.
	Hits        int64   `json:"hits"`
	HitRatePct  float64 `json:"hit_rate_pct"`
	CostUSD     float64 `json:"cost_usd"`
	CostUnknown int64   `json:"cost_unknown"`
	// Source is set only on the TTL groups, naming how that tier was known.
	Source string `json:"source,omitempty"`
}

// SurvivalPoint is one rung of "has the conversation come back yet".
//
// The survival view answers the question a TTL is chosen against, which a histogram does not: a
// histogram says how many gaps were four to five minutes long, and a policy needs the
// CUMULATIVE share still inside a horizon. Both are on the page — the histogram shows the shape,
// this shows the decision.
type SurvivalPoint struct {
	Seconds float64 `json:"seconds"`
	Label   string  `json:"label"`
	// Arrived is how many gaps had closed by then, out of N — which is the count of requests
	// WITH a successor, repeated on every point so a reader never has to hunt for the
	// denominator.
	Arrived    int64   `json:"arrived"`
	ArrivedPct float64 `json:"arrived_pct"`
	N          int64   `json:"n"`
	// TTL marks the rungs that are a provider tier rather than an arbitrary ladder step, so the
	// chart can draw them as the thresholds they are.
	TTL string `json:"ttl,omitempty"`
}

// KVCacheAnalysis is the whole analysis read: one request, one consistent set of denominators.
//
// Deliberately ONE payload rather than a panel per endpoint. Every figure shares the dataset it
// came from, and the failure that avoids is two tiles on one page reporting different totals
// because two queries disagreed about which rows were in scope — which this repo has shipped.
type KVCacheAnalysis struct {
	Cards     KVCacheCards    `json:"cards"`
	Coverage  KVCacheCoverage `json:"coverage"`
	IdleBands []Band          `json:"idle_bands"`
	Survival  []SurvivalPoint `json:"survival"`
	HourBins  []Band          `json:"hour_bins"`
	ByTTL     []KVCacheGroup  `json:"by_ttl"`
	ByBucket  []KVCacheGroup  `json:"by_bucket"`
	ByUser    []KVCacheGroup  `json:"by_user"`
	ByModel   []KVCacheGroup  `json:"by_model"`
	// Assumptions is the server's own statement of every formula and caveat, so the page prints
	// the arithmetic rather than restating it in a template nothing tests.
	Assumptions KVCacheAssumptions `json:"assumptions"`
	Pricing     *kvcache.PriceList `json:"pricing"`
	// FirstTS and LastTS are the span the rows actually cover, so a quiet window is
	// distinguishable from a narrow one.
	FirstTS int64 `json:"first_ts"`
	LastTS  int64 `json:"last_ts"`

	Scanned   int64 `json:"scanned"`
	Total     int64 `json:"total"`
	Truncated bool  `json:"truncated"`
}

// KVCacheAnalyze reads the dataset and aggregates it.
func (d *DB) KVCacheAnalyze(f Filter, o KVCacheOptions, p modelinfo.Pricer,
	cfg KVCacheSimConfig) (*KVCacheAnalysis, error) {
	rows, total, err := d.KVCacheDataset(f, o)
	if err != nil {
		return nil, err
	}
	out := analyseKVCache(rows)
	out.Total, out.Scanned = total, int64(len(rows))
	out.Truncated = out.Scanned < total
	out.Assumptions = kvCacheAssumptions(cfg)
	out.Pricing = kvcache.NewPriceList(context.Background(), modelsOf(rows), p,
		cfg.Multipliers, cfg.Overrides)
	// Destructured, not passed as a tuple: QueryRow takes (string, ...any), so handing it a
	// two-value call passes the whole []any as ONE argument and the driver rejects it.
	kaQ, kaArgs := kvCacheKeepAliveCount(f)
	if err := d.sql.QueryRowContext(d.readCtx(), kaQ, kaArgs...).Scan(&out.Coverage.KeepAliveRows); err != nil &&
		err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// kvCacheKeepAliveCount counts the ping rows the dataset excluded. Its own small query rather
// than a CASE inside an aggregate, for the reason keepalive.go states: one predicate, one
// meaning.
func kvCacheKeepAliveCount(f Filter) (string, []any) {
	inner := kvCacheScope(f)
	inner.WithKeepAlive = true
	cond, args := inner.where()
	return `SELECT COUNT(*) FROM requests r WHERE ` + cond + ` AND r.keepalive = 1`, args
}

// analyseKVCache is the aggregation, over rows and nothing else.
//
// Split out from KVCacheAnalyze so it is testable without a database, and so there is exactly
// one pass: every card, band, rung and group below is filled from the same loop, which is what
// makes it impossible for two of them to disagree about the denominator.
func analyseKVCache(rows []*kvcache.Request) *KVCacheAnalysis {
	out := &KVCacheAnalysis{IdleBands: []Band{}, Survival: []SurvivalPoint{}, HourBins: []Band{},
		ByTTL: []KVCacheGroup{}, ByBucket: []KVCacheGroup{}, ByUser: []KVCacheGroup{},
		ByModel: []KVCacheGroup{}}
	whole := newKVCacheAcc()
	byTTL, byBucket, byUser, byModel := kvAccMap(), kvAccMap(), kvAccMap(), kvAccMap()
	bandN := make([]int64, len(idleEdges))
	bandUSD := make([]float64, len(idleEdges))
	hourN := make([]int64, 24)
	hourUSD := make([]float64, 24)
	var prefixes, gaps []float64
	users, models := map[string]bool{}, map[string]bool{}
	turns := map[kvcache.Conversation]int{}

	for _, r := range rows {
		if out.FirstTS == 0 || r.TS < out.FirstTS {
			out.FirstTS = r.TS
		}
		if r.TS > out.LastTS {
			out.LastTS = r.TS
		}
		users[r.User] = true
		models[r.Model] = true
		turns[r.Key()]++
		whole.add(r)
		kvAccFor(byTTL, ttlGroupKey(r)).addTier(r)
		kvAccFor(byBucket, string(r.Bucket)).add(r)
		kvAccFor(byUser, r.User).add(r)
		kvAccFor(byModel, r.Model).add(r)
		// Only rows that cached something contribute: see CachedContextP50.
		if r.CachedContext > 0 {
			prefixes = append(prefixes, float64(r.CachedContext))
		}
		if r.HourUTC >= 0 && r.HourUTC < 24 {
			hourN[r.HourUTC]++
			hourUSD[r.HourUTC] += r.CostUSD
		}
		// Only rows that HAVE a successor reach the histogram and the ladder. A final request
		// in the first band would be reported as an instant return.
		if idle, ok := r.Idle(); ok {
			gaps = append(gaps, idle.Seconds())
			i := bandOf(idleEdges, idle.Seconds())
			bandN[i]++
			bandUSD[i] += r.CostUSD
		}
	}

	out.Cards = whole.cards(len(users), len(models), prefixes)
	out.Coverage = whole.coverage(turns)
	for i, label := range bandLabels(idleEdges, kvCacheSecs) {
		out.IdleBands = append(out.IdleBands, Band{Label: label, N: bandN[i], USD: bandUSD[i],
			// Beyond de-emphasises the bands the provider's DEFAULT five-minute lifetime cannot
			// reach. The same meaning the keep-alive tab's Band gives it: outside the coverage
			// the current policy provides.
			Beyond: idleEdges[i] >= kvcache.Horizon5m.Seconds()})
	}
	for h := 0; h < 24; h++ {
		out.HourBins = append(out.HourBins, Band{Label: fmt.Sprintf("%02d", h), N: hourN[h],
			USD: hourUSD[h]})
	}
	out.Survival = survivalOf(gaps)
	out.ByTTL = finishKVAccs(byTTL, ttlOrder)
	out.ByBucket = finishKVAccs(byBucket, bucketOrder())
	out.ByUser = finishKVAccs(byUser, nil)
	out.ByModel = finishKVAccs(byModel, nil)
	return out
}

// survivalOf is the empirical CDF of the idle gap over the ladder.
func survivalOf(gaps []float64) []SurvivalPoint {
	out := make([]SurvivalPoint, 0, len(survivalLadder))
	n := int64(len(gaps))
	for _, sec := range survivalLadder {
		p := SurvivalPoint{Seconds: sec, Label: kvCacheSecs(sec), N: n}
		for _, g := range gaps {
			if g <= sec {
				p.Arrived++
			}
		}
		if n > 0 {
			p.ArrivedPct = 100 * float64(p.Arrived) / float64(n)
		}
		switch sec {
		case kvcache.Horizon5m.Seconds():
			p.TTL = kvcache.TTL5m.Label()
		case kvcache.Horizon1h.Seconds():
			p.TTL = kvcache.TTL1h.Label()
		}
		out = append(out, p)
	}
	return out
}

// kvCacheAcc accumulates one group. The reuse fields hold COUNTS until finish(), which is the
// one place they become percentages.
type kvCacheAcc struct {
	g       KVCacheGroup
	convs   map[kvcache.Conversation]bool
	gaps    []float64
	hits    int64
	within5 int64
	within1 int64
	sources map[string]int64
	tiers   map[string]int64
}

func newKVCacheAcc() *kvCacheAcc {
	return &kvCacheAcc{convs: map[kvcache.Conversation]bool{}, sources: map[string]int64{},
		tiers: map[string]int64{}}
}

func kvAccMap() map[string]*kvCacheAcc { return map[string]*kvCacheAcc{} }

func kvAccFor(m map[string]*kvCacheAcc, key string) *kvCacheAcc {
	if a := m[key]; a != nil {
		return a
	}
	a := newKVCacheAcc()
	a.g.Key = key
	m[key] = a
	return a
}

func (a *kvCacheAcc) add(r *kvcache.Request) {
	a.g.Requests++
	a.convs[r.Key()] = true
	a.sources[r.TTLSource]++
	a.tiers[string(r.TTL)]++
	if r.Hit {
		a.hits++
	}
	if r.CostKnown {
		a.g.CostUSD += r.CostUSD
	} else {
		a.g.CostUnknown++
	}
	if idle, ok := r.Idle(); ok {
		a.g.WithNext++
		a.gaps = append(a.gaps, idle.Seconds())
		if r.Within5m {
			a.within5++
		}
		if r.Within1h {
			a.within1++
		}
		return
	}
	a.g.FinalRequests++
}

// addTier is add plus nothing extra; the tier group's Source comes from the same tally add()
// already keeps. A separate name because a reader of the TTL grouping should see that the
// source is deliberate there and absent elsewhere.
func (a *kvCacheAcc) addTier(r *kvcache.Request) { a.add(r) }

func (a *kvCacheAcc) finish() KVCacheGroup {
	g := a.g
	g.Conversations = int64(len(a.convs))
	if g.WithNext > 0 {
		g.Within5mPct = 100 * float64(a.within5) / float64(g.WithNext)
		g.Within1hPct = 100 * float64(a.within1) / float64(g.WithNext)
		g.MedianIdleMs = pctlF(a.gaps, 0.50) * 1000
		var sum float64
		for _, s := range a.gaps {
			sum += s
		}
		g.MeanIdleMs = sum / float64(g.WithNext) * 1000
	}
	g.Hits = a.hits
	if g.Requests > 0 {
		g.HitRatePct = 100 * float64(a.hits) / float64(g.Requests)
	}
	// The dominant way this group's tier was known. Ties broken by name so the same window
	// gives the same answer twice.
	var best string
	for _, s := range sortedStrings(a.sources) {
		if best == "" || a.sources[s] > a.sources[best] {
			best = s
		}
	}
	g.Source = best
	return g
}

func (a *kvCacheAcc) cards(users, models int, prefixes []float64) KVCacheCards {
	g := a.finish()
	return KVCacheCards{
		Requests: g.Requests, Scanned: g.Requests, Conversations: g.Conversations,
		Users: int64(users), Models: int64(models),
		WithNext: g.WithNext, FinalRequests: g.FinalRequests,
		MedianIdleMs: g.MedianIdleMs, MeanIdleMs: g.MeanIdleMs,
		P90IdleMs: pctlF(a.gaps, 0.90) * 1000,
		Within5m:  a.within5, Within1h: a.within1,
		Within5mPct: g.Within5mPct, Within1hPct: g.Within1hPct,
		Hits: a.hits, HitRatePct: g.HitRatePct,
		CostUSD: g.CostUSD, CostUnknown: g.CostUnknown,
		CachedContextP50: int64(pctlF(prefixes, 0.50)),
	}
}

// coverage counts the three tier states and the single-request conversations.
func (a *kvCacheAcc) coverage(turns map[kvcache.Conversation]int) KVCacheCoverage {
	c := KVCacheCoverage{
		TTLConfigured: a.sources[kvcache.TTLSourceConfigured],
		TTLObserved:   a.sources[kvcache.TTLSourceObserved],
		TTLUnknown:    a.sources[kvcache.TTLSourceUnknown],
		CostUnknown:   a.g.CostUnknown,
	}
	for _, n := range turns {
		if n == 1 {
			c.SingleRequestConversations++
		}
	}
	return c
}

// ttlGroupKey is the group a row belongs to in the by-TTL table: its tier where the tier is
// KNOWN, and TTLUnrecorded where it is not. Doubles as the value that filters on that group, so
// clicking a row in the table narrows to exactly the rows behind it.
func ttlGroupKey(r *kvcache.Request) string {
	if r.TTLSource == kvcache.TTLSourceUnknown {
		return TTLUnrecorded
	}
	if r.TTL == kvcache.TTLNone {
		return "none"
	}
	return string(r.TTL)
}

// ttlOrder and bucketOrder are the presentation orders for the two groupings whose order
// carries meaning. A bar chart of these must not sort by size: tier is a scale and a
// time-of-day band is a clock. TTLUnrecorded is LAST, because it is an absence rather than a
// tier and must not sit between two real ones.
var ttlOrder = []string{string(kvcache.TTL5m), string(kvcache.TTL1h), "none", TTLUnrecorded}

func bucketOrder() []string {
	out := make([]string, 0, len(kvcache.Buckets))
	for _, b := range kvcache.Buckets {
		out = append(out, string(b))
	}
	return out
}

// finishKVAccs sorts a group map. `order` fixes the sequence where it carries meaning; nil
// sorts by request count descending, with the key as a stable tie-break. Anything the fixed
// order does not name still appears, or a tier from a future build would vanish from a page
// whose totals include it.
func finishKVAccs(m map[string]*kvCacheAcc, order []string) []KVCacheGroup {
	out := make([]KVCacheGroup, 0, len(m))
	named := map[string]bool{}
	for _, k := range order {
		named[k] = true
		if a := m[k]; a != nil {
			out = append(out, a.finish())
		}
	}
	rest := make([]KVCacheGroup, 0, len(m))
	for _, k := range sortedStrings(m) {
		if !named[k] {
			rest = append(rest, m[k].finish())
		}
	}
	if order == nil {
		sort.SliceStable(rest, func(i, j int) bool { return rest[i].Requests > rest[j].Requests })
	}
	return append(out, rest...)
}

// kvCacheSecs renders a duration in seconds the way a person reads it — the same rounding the
// keep-alive tab's band labels use, so two charts on one dashboard do not spell 9.7 minutes two
// different ways.
func kvCacheSecs(v float64) string {
	switch {
	case v >= 86400:
		return trimZero(v/86400) + "d"
	case v >= 3600:
		return trimZero(v/3600) + "h"
	case v >= 60:
		return trimZero(v/60) + "m"
	default:
		return trimZero(v) + "s"
	}
}

// ── the assumptions payload ────────────────────────────────────────────────

// KVCacheFormula is one cost formula: what it is called, the expression, and the sentence that
// says what it means and where it can mislead.
//
// A struct rather than prose in the UI for one reason: a formula typed into a JavaScript
// template is a SECOND definition of the arithmetic, and nothing tests it against the first.
// These travel with the data and are asserted against the functions that implement them.
type KVCacheFormula struct {
	Name string `json:"name"`
	// Formula and Note are spelled the way the page reads them. Renaming either is a silent
	// break: the expression renders into an empty <code> element, so the panel lists eleven
	// formula NAMES with eleven blank boxes under them and looks like a styling glitch rather
	// than a contract mismatch. TestThePageReadsTheFormulaFieldsTheServerSends is the guard.
	Formula string `json:"formula"`
	Note    string `json:"note"`
}

// KVCacheSchedule is the keep-alive schedule a simulation ran with, in seconds.
//
// Two intervals, because the interval is the whole cost difference between the two tiers: one
// ping costs the same either way (it is a cache read), but a five-minute entry has to be touched
// roughly twelve times as often as a one-hour one to be held for the same span.
type KVCacheSchedule struct {
	IdleSeconds   float64 `json:"idle_seconds"`
	IdleSeconds1h float64 `json:"idle_seconds_1h"`
	MaxPings      int     `json:"max_pings"`
}

// KVCacheAssumptions is the server's own statement of what every figure on the page rests on:
// the units, the two horizons, the pricing multiples, the provider cache semantics, the
// keep-alive schedule, the cost formulas and the caveats.
//
// It is on the wire so the page PRINTS it rather than restating it. Every number here is one the
// simulation actually used, echoed back — so a reader checking a figure against a formula is
// checking it against the arithmetic that produced it.
type KVCacheAssumptions struct {
	// TimeZone is UTC, always, and it is a field rather than a constant in the page because the
	// claim has to travel with the data: the store carries no per-user timezone, so a local one
	// would be invented.
	TimeZone string `json:"time_zone"`
	TimeUnit string `json:"time_unit"`

	Horizon5mSeconds float64 `json:"horizon_5m_seconds"`
	Horizon1hSeconds float64 `json:"horizon_1h_seconds"`

	Multipliers kvcache.Multipliers `json:"multipliers"`
	Semantics   kvcache.Semantics   `json:"semantics"`
	Schedule    KVCacheSchedule     `json:"schedule"`

	Formulas []KVCacheFormula `json:"formulas"`
	Notes    []string         `json:"notes"`
}
