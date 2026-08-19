package dash

import "path/filepath"

// Accessors the Prometheus exporter needs. They live here rather than in the exporter
// because they read this package's private state, and exposing the store itself just
// so something else can measure it would be a wider seam than the numbers deserve.

// DBSizeBytes is the local database's size, write-ahead log included. 0 on error —
// a metrics endpoint must not fail because one gauge could not be read.
func (r *Recorder) DBSizeBytes() int64 {
	if r == nil || r.db == nil {
		return 0
	}
	n, err := r.db.sizeBytes()
	if err != nil {
		return 0
	}
	return n
}

// DiskUsedFraction is how full the filesystem holding the database is. The second
// return distinguishes "0% used" from "could not measure", which matters for an alert
// rule: a gauge that reads zero when the probe fails would suppress the alert exactly
// when the box is in trouble.
func (r *Recorder) DiskUsedFraction() (float64, bool) {
	if r == nil || r.db == nil {
		return 0, false
	}
	p := r.db.Path()
	if p == "" || p == ":memory:" {
		return 0, false
	}
	return r.probeDisk(filepath.Dir(p))
}

// ArchivedSessionCount is how many sessions are in cold storage.
func (r *Recorder) ArchivedSessionCount() int64 {
	if r == nil || r.db == nil {
		return 0
	}
	var n int64
	_ = r.db.sql.QueryRow(`SELECT COUNT(*) FROM archived_sessions`).Scan(&n)
	return n
}

// ArchivedBytes is how much is stored in cold storage, both kinds of object counted.
func (r *Recorder) ArchivedBytes() int64 {
	if r == nil || r.db == nil {
		return 0
	}
	var n *int64
	_ = r.db.sql.QueryRow(
		`SELECT SUM(content_bytes + full_bytes) FROM archived_sessions`).Scan(&n)
	if n == nil {
		return 0
	}
	return *n
}

// TenantMetricRow mirrors proxy.TenantMetricRow. Duplicated rather than imported
// because dash must not depend on proxy (proxy depends on dash), and a shared types
// package for one struct would be more indirection than the copy costs.
type TenantMetricRow struct {
	TenantID      string
	Label         string
	Requests      int64
	TokensBefore  int64
	TokensAfter   int64
	SavedUnique   int64
	CacheRead     int64
	CacheWrite    int64
	FreshInput    int64
	OutputTokens  int64
	CostUSD       float64
	BaselineUSD   float64
	CGLLMCostUSD  float64
	CacheSavedUSD float64
	// CachesplitSavedUSD is the prefix-cache saving that is ours (see Overview).
	CachesplitSavedUSD float64
	// CachesplitHistoricalUSD is the same saving on requests written before it could be
	// measured per request, valued on read. Filled by the host, which holds the rates —
	// TenantMetrics itself cannot price anything.
	CachesplitHistoricalUSD float64
	CGLatencyMs             float64
	UpstreamMs              float64
	Sessions                int64
	ArchivedCount           int64
	ArchivedBytes           int64
}

// TenantMetrics rolls up per-tenant traffic since `since` (epoch ms), for the
// Prometheus exporter.
//
// One query with a LEFT JOIN onto the archive index rather than two and a merge in Go:
// a tenant that has ONLY archived data still needs a row, or its history would vanish
// from Grafana the moment its last live session was archived.
func (d *DB) TenantMetrics(since int64) ([]TenantMetricRow, error) {
	rows, err := d.sql.Query(`
		SELECT t.tenant_id,
		       COALESCE(t.requests,0), COALESCE(t.tokens_before,0), COALESCE(t.tokens_after,0),
		       COALESCE(t.saved_unique,0), COALESCE(t.cache_read,0), COALESCE(t.cache_write,0),
		       COALESCE(t.fresh_input,0), COALESCE(t.output_tokens,0),
		       COALESCE(t.cost,0), COALESCE(t.baseline,0), COALESCE(t.cg_llm,0), COALESCE(t.cache_saved,0), COALESCE(t.cachesplit_saved,0),
		       COALESCE(t.cg_ms,0), COALESCE(t.up_ms,0), COALESCE(t.sessions,0),
		       COALESCE(a.n,0), COALESCE(a.bytes,0)
		FROM (
		  SELECT tenant_id,
		         COUNT(*) requests, SUM(tokens_before) tokens_before, SUM(tokens_after) tokens_after,
		         SUM(saved_unique) saved_unique, SUM(cache_read) cache_read, SUM(cache_write) cache_write,
		         SUM(fresh_input) fresh_input, SUM(output_tokens) output_tokens,
		         SUM(cost_usd) cost, SUM(baseline_cost_usd) baseline, SUM(cg_llm_cost_usd) cg_llm,
		         SUM(cache_saved_usd) cache_saved, SUM(cachesplit_saved_usd) cachesplit_saved,
		         AVG(cg_latency_ms) cg_ms, AVG(upstream_ms) up_ms,
		         COUNT(DISTINCT session_id) sessions
		  FROM requests WHERE ts >= ? GROUP BY tenant_id
		  UNION ALL
		  -- Tenants whose live rows have all been archived still deserve a row.
		  SELECT tenant_id, 0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0 FROM archived_sessions
		  WHERE tenant_id NOT IN (SELECT tenant_id FROM requests WHERE ts >= ?)
		  GROUP BY tenant_id
		) t
		LEFT JOIN (
		  SELECT tenant_id, COUNT(*) n, SUM(content_bytes + full_bytes) bytes
		  FROM archived_sessions GROUP BY tenant_id
		) a ON a.tenant_id = t.tenant_id`, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantMetricRow
	for rows.Next() {
		var t TenantMetricRow
		if err := rows.Scan(&t.TenantID, &t.Requests, &t.TokensBefore, &t.TokensAfter,
			&t.SavedUnique, &t.CacheRead, &t.CacheWrite, &t.FreshInput, &t.OutputTokens,
			&t.CostUSD, &t.BaselineUSD, &t.CGLLMCostUSD, &t.CacheSavedUSD, &t.CachesplitSavedUSD,
			&t.CGLatencyMs, &t.UpstreamMs,
			&t.Sessions, &t.ArchivedCount, &t.ArchivedBytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ValueMetrics is the deployment-wide value of compaction, for Prometheus. Not per tenant:
// these four are the numbers an operator alerts and reports on, and there was no dollar
// series at all — /metrics exported tokens, ratios, latency, cache outcomes and one
// component's net value, so Grafana could plot everything about this proxy EXCEPT whether it
// was worth running. cg_extract_net_value_usd was the only dollar figure on the endpoint.
type ValueMetrics struct {
	// SavedUSD is baseline − billed: what compaction avoided, before our own spend.
	// NetSavedUSD subtracts CGLLMCostUSD, so it goes NEGATIVE when we spend more than we
	// save, which is a real outcome and the series reports it as one.
	CostUSD      float64
	BaselineUSD  float64
	CGLLMCostUSD float64
	SavedUSD     float64
	NetSavedUSD  float64
	// FrozenTokens is content cache-aware compaction deliberately left alone — the
	// headroom, and the number that explains a small SavedUSD.
	FrozenTokens int64
}

// ValueMetrics rolls up the value figures since `since` (epoch ms). Read-only.
func (r *Recorder) ValueMetrics(since int64) (ValueMetrics, error) {
	var v ValueMetrics
	if r == nil || r.db == nil {
		return v, nil
	}
	err := r.db.sql.QueryRow(`SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(baseline_cost_usd),0),
		COALESCE(SUM(cg_llm_cost_usd),0), COALESCE(SUM(frozen_tokens),0)
		FROM requests WHERE ts >= ?`, since).Scan(&v.CostUSD, &v.BaselineUSD, &v.CGLLMCostUSD, &v.FrozenTokens)
	if err != nil {
		return v, err
	}
	v.SavedUSD = v.BaselineUSD - v.CostUSD
	v.NetSavedUSD = v.SavedUSD - v.CGLLMCostUSD
	return v, nil
}
