package dash

import (
	"database/sql"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// OverviewLean answers /api/stats?lean=1 (dash/api.go's statsLean branch) — see #502.
//
// For a SESSION or a concrete time window (anything but true lifetime), it runs 1 bounded
// aggregate (overviewLeanWindow) instead of Overview()'s 16 queries, because lean reads only a
// handful of their outputs: confirmed against context-guru-plugin/scripts/statusline.py and
// scripts/cg.py, lean needs total_saved_usd, keepalive_net_usd, keepalive_misses_avoided,
// upstream_ms_avg, cg_latency_ms_avg, cost_usd, baseline_cost_usd, cache_saved_usd, and the
// waterfall rows keyed "compaction", "cachesplit_saved", "keepalive_saved", "keepalive_ping",
// "decl_filter_saved", "self_removed", "cg_llm" — every one a plain SUM/AVG of a `requests`
// column, bounded by the session or the time window, same index Overview()'s own main
// aggregate already uses. The 7 goroutines Overview() runs beyond that aggregate (the replay
// ceiling, its keep-alive correction, CompactionResets, the TTL/accounting breakdown, 2
// invalid-config counts, the estimator-divergence query, the p95-latency query) feed none of
// those fields, and the replay ceiling and CompactionResets among them are exactly the
// per-session window queries #492 measured as slow. OverviewLean never runs any of them.
//
// TRUE lifetime (`since=0`, no session, no window) is not answered here: dash/api.go sends a lean
// call with neither bound to the full handler, which is exact. The status line stopped reading
// lifetime (#510), so lean is only ever a bounded read.
//
// The 2 read-time-priced credits (CachesplitHistoricalUSD, DeclFilterSavings) are included for
// SESSION and DAY scope, each wherever its OWN bound holds:
//
//   - DeclFilterSavings' SELECT is a plain filtered scan, bounded by the same index as the main
//     aggregate for a session OR a time window — included for both.
//   - CachesplitHistoricalUSD's ranking has to see a session's WHOLE history to know which row
//     was its first (dash/cachehistory.go's own comment) — bounded for a session only, so a
//     day-window call does not get it either.
func (d *DB) OverviewLean(f Filter, pricer modelinfo.Pricer, priceFn func(string) (modelinfo.Price, bool)) (*Overview, error) {
	o, err := d.overviewLeanWindow(f)
	if err != nil {
		return nil, err
	}
	if priceFn != nil {
		if fs, err := d.DeclFilterSavings(f, priceFn); err == nil {
			o.TotalSavedUSD += fs.USD
		}
	}
	if f.Session != "" && pricer != nil {
		if h, err := d.CachesplitHistoricalUSD(f, pricer); err == nil {
			o.CachesplitHistorical = &h
			o.TotalSavedUSD += h.USD
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
	var upAvg, cgAvg sql.NullFloat64
	if err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(
		&o.Requests, &o.TokensBefore, &o.TokensAfter, &o.SavedUnique,
		&o.CostUSD, &o.BaselineCostUSD, &o.CGLLMCostUSD, &o.CacheSavedUSD, &o.CachesplitSavedUSD,
		&o.KeepAliveSavedUSD, &o.KeepAliveMissesAvoided, &upAvg, &cgAvg, &o.KeepAlivePingUSD, &o.KeepAlivePings); err != nil {
		return nil, err
	}
	o.UpstreamMsAvg, o.CGLatencyMsAvg = upAvg.Float64, cgAvg.Float64
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
//
// #502 review (M4): cg_latency_ms_avg stays in this SELECT because the released v0.4.2 status
// line (context-guru-plugin/scripts/statusline.py, before #478) reads it and renders
// "proxy: 0ms" if it is ever absent — a field dropped here that an OLDER, already-installed
// client still expects is a regression for that client, not a cleanup.
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
			AVG(CASE WHEN ` + agent + ` THEN r.cg_latency_ms END),
			COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN r.cost_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN 1 ELSE 0 END),0)
		FROM requests r WHERE ` + cond, args
}
