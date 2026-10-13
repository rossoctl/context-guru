package dash

import (
	"database/sql"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// OverviewLean answers exactly what /api/stats?lean=1 needs (dash/api.go's statsLean branch),
// and nothing else — see #502. Overview() runs sixteen queries; lean read a few of their
// outputs. The plan this follows (issue #502's "Plan (decided)" comment) is a rule, not a
// per-query judgement call: "/api/stats?lean=1 is the bounded path. Every query on it must
// have a cost that does not grow with the session's size squared, nor with the whole table."
//
// What lean actually reads, confirmed against context-guru-plugin/scripts/statusline.py (the
// status line) and scripts/cg.py (the plugin's cost commands): total_saved_usd,
// keepalive_net_usd, keepalive_misses_avoided, upstream_ms_avg, cost_usd, baseline_cost_usd,
// cache_saved_usd, and the waterfall rows keyed "compaction", "cachesplit_saved",
// "keepalive_saved", "keepalive_ping", "decl_filter_saved", "self_removed", "cg_llm". Every
// one of those is a plain SUM/AVG of a `requests` column (or, for the keep-alive COST half, a
// SEPARATELY scoped SUM/COUNT) — a single pass over an INDEXED range, bounded by the session
// or by the time window, same as Overview()'s own main aggregate.
//
// What lean never reads: ReplayProjectedTokens, ReplayRealizedPct, CompactionResets, the
// estimator-divergence pair, the TTL/accounting/breakpoint breakdown, the two p95 latencies,
// InvalidConfigRequests/Recent. Those are the seven goroutines Overview() runs beyond the
// main aggregate — the replay ceiling and CompactionResets among them being exactly the
// per-session window queries #492 and #502 measured as slow or quadratic. OverviewLean never
// runs any of them.
//
// Two fields ARE priced at read time rather than stored on the row — the pre-instrumentation
// cachesplit credit (CachesplitHistoricalUSD) and the declaration-filter credit
// (DeclFilterSavings) — and both need a query that is bounded ONLY when scoped to one
// session: CachesplitHistoricalUSD's ranking has to see a session's whole history to know
// which row was its first (dash/cachehistory.go's own comment), and DeclFilterSavings' SELECT
// is a plain filtered scan that is bounded by the SAME index as the main aggregate for a
// session or a time window, but not for `since=0`. So OverviewLean includes both ONLY when
// f.Session is set; for a day window or the lifetime read, `total_saved_usd` is short by
// those two credits. Both are small and shrink over time (the first is a historical
// population that only gets smaller; the second needs a model's price, which the caller
// cannot always supply either) — a disclosed, bounded trade accepted in the plan comment's
// "if cg.py needs a field that is moved out, switch that call to the full /api/stats": cg.py's
// own `stats()` helper now calls the full endpoint, so its numbers are exact; only the status
// line's day/lifetime reads (which can never call anything but lean) carry the small
// understatement, and its SESSION read — the one that renders on every tick — does not.
func (d *DB) OverviewLean(f Filter, pricer modelinfo.Pricer, priceFn func(string) (modelinfo.Price, bool)) (*Overview, error) {
	var o *Overview
	var err error
	if f.Session == "" && f.Since == 0 && f.Until == 0 {
		o, err = d.overviewLeanLifetime(f)
	} else {
		o, err = d.overviewLeanWindow(f)
	}
	if err != nil {
		return nil, err
	}
	if f.Session != "" && pricer != nil {
		if h, err := d.CachesplitHistoricalUSD(f, pricer); err == nil {
			o.CachesplitHistorical = &h
			o.TotalSavedUSD += h.USD
		}
		if fs, err := d.DeclFilterSavings(f, priceFn); err == nil {
			o.TotalSavedUSD += fs.USD
		}
	}
	o.Waterfall = o.waterfall()
	return o, nil
}

// overviewLeanWindow answers a session or a concrete time window (`since` other than 0, or
// `session` set, or both) from `requests` directly — bounded the same way Overview()'s own
// main aggregate is: `f.where()` turns a session into `r.session_id = ?` (idx_requests_session)
// and a time window into `r.ts >= ?` (idx_requests_tenant, tenant_id-leading), so this is one
// indexed range read, never a scan of a session squared or of the whole table.
func (d *DB) overviewLeanWindow(f Filter) (*Overview, error) {
	o := &Overview{Since: f.Since, Until: f.Until}
	q, args := leanWindowMainQuery(f)
	var upAvg sql.NullFloat64
	if err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(
		&o.Requests, &o.TokensBefore, &o.TokensAfter, &o.SavedUnique,
		&o.CostUSD, &o.BaselineCostUSD, &o.CGLLMCostUSD, &o.CacheSavedUSD, &o.CachesplitSavedUSD,
		&o.KeepAliveSavedUSD, &o.KeepAliveMissesAvoided, &upAvg, &o.KeepAlivePingUSD); err != nil {
		return nil, err
	}
	o.UpstreamMsAvg = upAvg.Float64
	o.NetSavedUSD, o.KeepAliveNetUSD, o.TotalSavedUSD = savingsArithmetic(
		o.CostUSD, o.BaselineCostUSD, o.CGLLMCostUSD, o.CachesplitSavedUSD, o.KeepAliveSavedUSD, o.KeepAlivePingUSD)
	o.TotalReducedUSD = o.TotalSavedUSD
	return o, nil
}

// leanWindowMainQuery builds the SQL and args for overviewLeanWindow's one aggregate — pulled
// out so #502's EXPLAIN-plan test (TestOverviewLeanQueriesAreIndexed) runs against the EXACT
// statement the server executes, not a hand-copied duplicate that could drift from it.
//
// One scan, not two: this used to run a second, separately-scoped query for the keep-alive
// COST half (ping rows themselves, which the agent-only `cond` below excludes from everything
// else) — correct, but measured on a 20,000-request session as two indexed scans of the same
// range, ~38ms and ~18ms, 56ms total, over #502's own 50ms budget. Scoping by
// `withKeepAlive(f)` instead (every row in range, agent and ping alike) and folding the
// agent/ping split into CASE expressions answers both in the ONE scan this range needs either
// way — measured, ~45ms for the combined query against ~56ms for the two separately.
func leanWindowMainQuery(f Filter) (string, []any) {
	cond, args := withKeepAlive(f).where()
	const agent = "r.keepalive = 0"
	return `SELECT
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.tokens_before ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.tokens_after ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.saved_unique ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cost_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.baseline_cost_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cg_llm_cost_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cache_saved_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN r.cachesplit_saved_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` THEN ` + kaSaved("r.") + ` ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + agent + ` AND ` + kaSaved("r.") + ` > 0 THEN 1 ELSE 0 END),0),
			AVG(CASE WHEN ` + agent + ` THEN r.upstream_ms END),
			COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN r.cost_usd ELSE 0 END),0)
		FROM requests r WHERE ` + cond, args
}

// overviewLeanLifetime answers `since=0` (no session, no window: the status line's "life" job)
// from daily_rollup instead of `requests` — bounded by the number of DAYS the deployment has
// existed, not by how many requests it has served. See dash/schema.go's comment on the table
// for how it stays current, and dash/dailyrollupbackfill.go for how it covers history written
// before it existed.
func (d *DB) overviewLeanLifetime(f Filter) (*Overview, error) {
	o := &Overview{}
	q, args := leanLifetimeQuery(f)
	var upSum sql.NullFloat64
	var upCount sql.NullInt64
	err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(
		&o.Requests, &o.TokensBefore, &o.TokensAfter, &o.SavedUnique,
		&o.CostUSD, &o.BaselineCostUSD, &o.CGLLMCostUSD, &o.CacheSavedUSD, &o.CachesplitSavedUSD,
		&o.KeepAliveSavedUSD, &o.KeepAliveMissesAvoided, &o.KeepAlivePingUSD, &upSum, &upCount)
	if err != nil {
		return nil, err
	}
	if upCount.Valid && upCount.Int64 > 0 {
		o.UpstreamMsAvg = upSum.Float64 / float64(upCount.Int64)
	}
	o.NetSavedUSD, o.KeepAliveNetUSD, o.TotalSavedUSD = savingsArithmetic(
		o.CostUSD, o.BaselineCostUSD, o.CGLLMCostUSD, o.CachesplitSavedUSD, o.KeepAliveSavedUSD, o.KeepAlivePingUSD)
	o.TotalReducedUSD = o.TotalSavedUSD
	return o, nil
}

// leanLifetimeQuery builds the SQL and args for overviewLeanLifetime — same reason as
// leanWindowMainQuery above. Bounded by the number of (tenant, day) rows in daily_rollup, not
// by the number of requests the tenant has ever sent.
func leanLifetimeQuery(f Filter) (string, []any) {
	cond, args := "1=1", []any{}
	if !f.TenantAll {
		cond, args = "tenant_id = ?", []any{f.Tenant}
	}
	return `SELECT
			COALESCE(SUM(requests),0), COALESCE(SUM(tokens_before),0), COALESCE(SUM(tokens_after),0),
			COALESCE(SUM(saved_unique),0), COALESCE(SUM(cost_usd),0), COALESCE(SUM(baseline_cost_usd),0),
			COALESCE(SUM(cg_llm_cost_usd),0), COALESCE(SUM(cache_saved_usd),0), COALESCE(SUM(cachesplit_saved_usd),0),
			COALESCE(SUM(keepalive_saved_usd),0), COALESCE(SUM(keepalive_misses_avoided),0),
			COALESCE(SUM(keepalive_ping_usd),0), SUM(upstream_ms_sum), SUM(upstream_ms_count)
		FROM daily_rollup WHERE ` + cond, args
}
