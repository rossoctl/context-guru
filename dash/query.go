package dash

import (
	"database/sql"
	"fmt"
	"strings"
)

// Filter is the server-side filter set every list/aggregate query accepts. Every
// dimension the issue names is here, and filtering happens in SQL — pushing it to
// the client is the gap that makes headroom's request log unusable past a few
// hundred rows.
type Filter struct {
	Since int64 // epoch ms, inclusive; 0 = unbounded
	Until int64 // epoch ms, exclusive; 0 = unbounded
	// Tenant scopes every query to one tenant. In a hosted deployment the API layer
	// OVERWRITES this from the authenticated principal after parsing the request —
	// set, never merged — so a crafted ?tenant= cannot widen a view. A manager may
	// pass one explicitly. TenantAll opts out, and only a manager may reach it.
	Tenant string
	// TenantAll disables tenant scoping for a service-wide view. Separate from an
	// empty Tenant because "" is a legitimate tenant id (every single-tenant row),
	// so absence could not mean "everything" without making the unscoped case the
	// default — which is the wrong default for a filter that guards other people's data.
	TenantAll bool
	Session   string
	Model     string
	Provider  string
	Agent     string
	Preset    string
	Mode      string
	// Component selects requests on which this component RAN.
	Component string
	// Reason selects requests by their uncompressed reason bucket; the sentinel
	// "compacted" selects rows where we did compact.
	Reason string
	// Accounting selects by token_accounting (complete|partial|missing).
	Accounting string
	// Q is a free-text match against session id and model.
	Q string
}

// where renders the filter as a SQL predicate plus its arguments. The table must
// be aliased `r`.
func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v ...any) {
		conds = append(conds, cond)
		args = append(args, v...)
	}
	if !f.TenantAll {
		add("r.tenant_id = ?", f.Tenant)
	}
	if f.Since > 0 {
		add("r.ts >= ?", f.Since)
	}
	if f.Until > 0 {
		add("r.ts < ?", f.Until)
	}
	for col, v := range map[string]string{
		"r.session_id": f.Session, "r.model": f.Model, "r.provider": f.Provider,
		"r.agent": f.Agent, "r.preset": f.Preset, "r.mode": f.Mode,
		"r.token_accounting": f.Accounting,
	} {
		if v != "" {
			add(col+" = ?", v)
		}
	}
	switch f.Reason {
	case "":
	case "compacted":
		add("r.uncompressed_reason = ''")
	default:
		add("r.uncompressed_reason = ?", f.Reason)
	}
	if f.Component != "" {
		add("EXISTS (SELECT 1 FROM request_components c WHERE c.request_id = r.id AND c.component = ?)", f.Component)
	}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		add("(r.session_id LIKE ? OR r.model LIKE ? OR r.agent LIKE ?)", like, like, like)
	}
	if len(conds) == 0 {
		return "1=1", nil
	}
	return strings.Join(conds, " AND "), args
}

// requestCols is the column list Event rows are scanned from, in one place so the
// SELECT and the Scan cannot drift.
const requestCols = `r.id, r.ts, r.tenant_id, r.session_id, r.model, r.provider, r.agent, r.preset, r.mode, r.route,
	r.status, r.bypassed, r.cache_aware, r.messages, r.tokens_before, r.tokens_after,
	r.attempted_tokens, r.frozen_tokens, r.saved_unique, r.fresh_input, r.cache_read,
	r.cache_write, r.output_tokens, r.cost_usd, r.baseline_cost_usd, r.cg_llm_cost_usd,
	r.cg_latency_ms, r.upstream_ms, r.expands, r.expand_tokens, r.reverts,
	r.token_accounting, r.cache_miss_reason, r.uncompressed_reason`

func scanRequest(rows interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	var byp, ca int
	err := rows.Scan(&e.ID, &e.TS, &e.TenantID, &e.SessionID, &e.Model, &e.Provider, &e.Agent, &e.Preset, &e.Mode, &e.Route,
		&e.Status, &byp, &ca, &e.Messages, &e.TokensBefore, &e.TokensAfter,
		&e.AttemptedTokens, &e.FrozenTokens, &e.SavedUnique, &e.FreshInput, &e.CacheRead,
		&e.CacheWrite, &e.OutputTokens, &e.CostUSD, &e.BaselineCostUSD, &e.CGLLMCostUSD,
		&e.CGLatencyMs, &e.UpstreamMs, &e.Expands, &e.ExpandTokens, &e.Reverts,
		&e.TokenAccounting, &e.CacheMissReason, &e.UncompressedReason)
	e.Bypassed, e.CacheAware = byp != 0, ca != 0
	return &e, err
}

// Page is one page of requests plus the cursor for the next one.
type Page struct {
	Requests []*Event `json:"requests"`
	// NextCursor is the `before` value for the following page; 0 = no more rows.
	NextCursor int64 `json:"next_cursor"`
	Total      int64 `json:"total"`
}

// Requests returns a page of requests newest-first, using KEYSET pagination:
// `before` is the last id seen, not an OFFSET. Offset pagination re-scans the
// skipped rows, so page 500 of a busy proxy's history costs 500 pages of work;
// keyset is O(limit) at any depth and cannot skip or duplicate a row when new
// requests arrive mid-browse.
func (d *DB) Requests(f Filter, before int64, limit int) (*Page, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	cond, filterArgs := f.where()
	q := `SELECT ` + requestCols + ` FROM requests r WHERE ` + cond
	pageArgs := append([]any(nil), filterArgs...)
	if before > 0 {
		q += " AND r.id < ?"
		pageArgs = append(pageArgs, before)
	}
	q += " ORDER BY r.id DESC LIMIT ?"
	rows, err := d.sql.Query(q, append(pageArgs, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &Page{Requests: []*Event{}}
	for rows.Next() {
		e, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		page.Requests = append(page.Requests, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// We asked for one extra row purely to learn whether another page exists —
	// cheaper and more accurate than a second COUNT against a moving table.
	if len(page.Requests) > limit {
		page.Requests = page.Requests[:limit]
		page.NextCursor = page.Requests[limit-1].ID
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond, filterArgs...).Scan(&page.Total); err != nil {
		return nil, err
	}
	return page, nil
}

// Request returns one request with its component rows and, when content was
// captured, its before/after blobs. withContent=false omits the content entirely
// (the caller decides, based on the access gate).
func (d *DB) Request(id int64, withContent bool) (*Event, error) {
	row := d.sql.QueryRow(`SELECT `+requestCols+` FROM requests r WHERE r.id = ?`, id)
	e, err := scanRequest(row)
	if err != nil {
		return nil, err
	}
	crows, err := d.sql.Query(`SELECT component, kind, acted, mutated, reverted, skipped,
		saved_gross, saved_unique, duration_ms, err FROM request_components
		WHERE request_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c CompRow
		var a, m, rv, sk int
		if err := crows.Scan(&c.Component, &c.Kind, &a, &m, &rv, &sk,
			&c.SavedGross, &c.SavedUnique, &c.DurationMs, &c.Err); err != nil {
			return nil, err
		}
		c.Acted, c.Mutated, c.Reverted, c.Skipped = a != 0, m != 0, rv != 0, sk != 0
		e.Components = append(e.Components, c)
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}
	if !withContent {
		return e, nil
	}
	trows, err := d.sql.Query(`SELECT path, before_tokens, after_tokens, before_gz, after_gz, components
		FROM request_content WHERE request_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var c ContentRow
		var bz, az []byte
		var comps string
		if err := trows.Scan(&c.Path, &c.BeforeTokens, &c.AfterTokens, &bz, &az, &comps); err != nil {
			return nil, err
		}
		c.Before, c.After = gunzipText(bz), gunzipText(az)
		if comps != "" {
			c.Components = strings.Split(comps, ",")
		}
		e.Content = append(e.Content, c)
	}
	return e, trows.Err()
}

// SessionEvents returns one session's requests oldest-first, scoped by f — so a
// hosted caller can only ever name a session that is theirs, and a session id
// belonging to somebody else comes back as zero rows rather than as a 403 that
// confirms it exists.
//
// withContent=false skips the transcript blobs entirely, which is what the list
// views and the metrics-only states want: the content columns are the bulk of the
// bytes and gunzipping a whole session to render a token count is pure waste.
func (d *DB) SessionEvents(f Filter, sessionID string, withContent bool) ([]*Event, error) {
	cond, args := f.where()
	rows, err := d.sql.Query(`SELECT r.id FROM requests r WHERE `+cond+
		` AND r.session_id = ? ORDER BY r.ts ASC, r.id ASC`, append(args, sessionID)...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*Event, 0, len(ids))
	for _, id := range ids {
		// Reusing Request rather than a bespoke join: it is the tested path that
		// assembles an Event with its components and content, and a second parallel
		// query is one that can quietly diverge from what the request view shows.
		e, err := d.Request(id, withContent)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// SessionRow is one row of the session list — the view neither reference
// implementation has at all, despite both having sessions internally.
type SessionRow struct {
	SessionID       string  `json:"session_id"`
	Turns           int64   `json:"turns"`
	Start           int64   `json:"start"`
	End             int64   `json:"end"`
	Models          string  `json:"models"`
	Providers       string  `json:"providers"`
	Agents          string  `json:"agents"`
	Presets         string  `json:"presets"`
	TokensBefore    int64   `json:"tokens_before"`
	TokensAfter     int64   `json:"tokens_after"`
	Saved           int64   `json:"saved"`
	SavedUnique     int64   `json:"saved_unique"`
	AttemptedTokens int64   `json:"attempted_tokens"`
	FrozenTokens    int64   `json:"frozen_tokens"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	OutputTokens    int64   `json:"output_tokens"`
	FreshInput      int64   `json:"fresh_input"`
	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	SavedUSD        float64 `json:"saved_usd"`
	Expands         int64   `json:"expands"`
	ExpandTokens    int64   `json:"expand_tokens"`
	Reverts         int64   `json:"reverts"`
	CGLatencyMs     float64 `json:"cg_latency_ms_avg"`
	UpstreamMs      float64 `json:"upstream_ms_avg"`
	Incomplete      int64   `json:"incomplete_rows"` // rows whose accounting is not `complete`
}

// Sessions returns the session list, most-recently-active first, filtered and
// paginated server-side.
func (d *DB) Sessions(f Filter, limit, offset int) ([]*SessionRow, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	cond, args := f.where()
	q := `SELECT r.session_id, COUNT(*), MIN(r.ts), MAX(r.ts),
		GROUP_CONCAT(DISTINCT r.model), GROUP_CONCAT(DISTINCT r.provider),
		GROUP_CONCAT(DISTINCT r.agent), GROUP_CONCAT(DISTINCT r.preset),
		SUM(r.tokens_before), SUM(r.tokens_after), SUM(r.saved_unique),
		SUM(r.attempted_tokens), SUM(r.frozen_tokens),
		SUM(r.cache_read), SUM(r.cache_write), SUM(r.output_tokens), SUM(r.fresh_input),
		SUM(r.cost_usd), SUM(r.baseline_cost_usd), SUM(r.cg_llm_cost_usd),
		SUM(r.expands), SUM(r.expand_tokens), SUM(r.reverts),
		AVG(r.cg_latency_ms), AVG(r.upstream_ms),
		SUM(CASE WHEN r.token_accounting <> 'complete' THEN 1 ELSE 0 END)
		FROM requests r WHERE ` + cond + `
		GROUP BY r.session_id ORDER BY MAX(r.ts) DESC LIMIT ? OFFSET ?`
	rows, err := d.sql.Query(q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*SessionRow{}
	for rows.Next() {
		var s SessionRow
		var models, providers, agents, presets sql.NullString
		if err := rows.Scan(&s.SessionID, &s.Turns, &s.Start, &s.End,
			&models, &providers, &agents, &presets,
			&s.TokensBefore, &s.TokensAfter, &s.SavedUnique,
			&s.AttemptedTokens, &s.FrozenTokens,
			&s.CacheRead, &s.CacheWrite, &s.OutputTokens, &s.FreshInput,
			&s.CostUSD, &s.BaselineCostUSD, &s.CGLLMCostUSD,
			&s.Expands, &s.ExpandTokens, &s.Reverts,
			&s.CGLatencyMs, &s.UpstreamMs, &s.Incomplete); err != nil {
			return nil, 0, err
		}
		s.Models, s.Providers, s.Agents, s.Presets = models.String, providers.String, agents.String, presets.String
		s.Saved = s.TokensBefore - s.TokensAfter
		s.SavedUSD = s.BaselineCostUSD - s.CostUSD - s.CGLLMCostUSD
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int64
	err = d.sql.QueryRow(`SELECT COUNT(DISTINCT r.session_id) FROM requests r WHERE `+cond, args...).Scan(&total)
	return out, total, err
}

// ComponentRow is one component's economics across the filtered window — the view
// that makes "which components earn their place" obvious without reading a doc.
type ComponentRow struct {
	Component       string  `json:"component"`
	Kind            string  `json:"kind"`
	Runs            int64   `json:"runs"`
	Acted           int64   `json:"acted"`
	Mutated         int64   `json:"mutated"`
	Reverted        int64   `json:"reverted"`
	Skipped         int64   `json:"skipped"`
	SavedGross      int64   `json:"saved_gross"`
	SavedUnique     int64   `json:"saved_unique"`
	OvercountRatio  float64 `json:"overcount_ratio"`
	DurationMsTotal float64 `json:"duration_ms_total"`
	DurationMsAvg   float64 `json:"duration_ms_avg"`
	Errors          int64   `json:"errors"`
	// ActRate is acted/runs: how often the component finds anything to do.
	ActRate float64 `json:"act_rate"`
}

// Components aggregates per-component accounting over the filtered window.
func (d *DB) Components(f Filter) ([]*ComponentRow, error) {
	cond, args := f.where()
	// Note: the filter's own Component clause deliberately still applies — it
	// selects the REQUESTS in scope, and we then report every component that ran on
	// them, which is how you see what a component co-occurs with.
	q := `SELECT c.component, MAX(c.kind), COUNT(*),
		SUM(c.acted), SUM(c.mutated), SUM(c.reverted), SUM(c.skipped),
		SUM(c.saved_gross), SUM(c.saved_unique), SUM(c.duration_ms),
		SUM(CASE WHEN c.err <> '' THEN 1 ELSE 0 END)
		FROM request_components c JOIN requests r ON r.id = c.request_id
		WHERE ` + cond + ` GROUP BY c.component ORDER BY SUM(c.saved_unique) DESC, c.component`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ComponentRow{}
	for rows.Next() {
		var c ComponentRow
		var kind sql.NullString
		if err := rows.Scan(&c.Component, &kind, &c.Runs, &c.Acted, &c.Mutated, &c.Reverted,
			&c.Skipped, &c.SavedGross, &c.SavedUnique, &c.DurationMsTotal, &c.Errors); err != nil {
			return nil, err
		}
		c.Kind = kind.String
		if c.SavedUnique > 0 {
			c.OvercountRatio = float64(c.SavedGross) / float64(c.SavedUnique)
		}
		if c.Runs > 0 {
			c.DurationMsAvg = c.DurationMsTotal / float64(c.Runs)
			c.ActRate = float64(c.Acted) / float64(c.Runs)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// Bucket is one time bucket of the series. Bucketing is done in SQL at query
// time (ts/bucket*bucket), so there are no rollup tables to keep consistent and
// any bucket size works without a migration.
type Bucket struct {
	TS              int64   `json:"ts"`
	Requests        int64   `json:"requests"`
	TokensBefore    int64   `json:"tokens_before"`
	TokensAfter     int64   `json:"tokens_after"`
	Saved           int64   `json:"saved"`
	SavedUnique     int64   `json:"saved_unique"`
	AttemptedTokens int64   `json:"attempted_tokens"`
	FrozenTokens    int64   `json:"frozen_tokens"`
	FreshInput      int64   `json:"fresh_input"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	OutputTokens    int64   `json:"output_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	CGLatencyMs     float64 `json:"cg_latency_ms_avg"`
	UpstreamMs      float64 `json:"upstream_ms_avg"`
	Expands         int64   `json:"expands"`
	ExpandTokens    int64   `json:"expand_tokens"`
	Misses          int64   `json:"cache_misses"`
}

// Series buckets the filtered window into fixed-width buckets of bucketMs.
func (d *DB) Series(f Filter, bucketMs int64) ([]*Bucket, error) {
	if bucketMs <= 0 {
		bucketMs = 60_000
	}
	cond, args := f.where()
	q := fmt.Sprintf(`SELECT (r.ts/%d)*%d AS b, COUNT(*),
		SUM(r.tokens_before), SUM(r.tokens_after), SUM(r.saved_unique),
		SUM(r.attempted_tokens), SUM(r.frozen_tokens),
		SUM(r.fresh_input), SUM(r.cache_read), SUM(r.cache_write), SUM(r.output_tokens),
		SUM(r.cost_usd), SUM(r.baseline_cost_usd), SUM(r.cg_llm_cost_usd),
		AVG(r.cg_latency_ms), AVG(r.upstream_ms),
		SUM(r.expands), SUM(r.expand_tokens),
		SUM(CASE WHEN r.cache_miss_reason NOT IN ('hit','') THEN 1 ELSE 0 END)
		FROM requests r WHERE %s GROUP BY b ORDER BY b`, bucketMs, bucketMs, cond)
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Bucket{}
	for rows.Next() {
		var b Bucket
		var cgAvg, upAvg sql.NullFloat64
		if err := rows.Scan(&b.TS, &b.Requests, &b.TokensBefore, &b.TokensAfter, &b.SavedUnique,
			&b.AttemptedTokens, &b.FrozenTokens, &b.FreshInput, &b.CacheRead, &b.CacheWrite,
			&b.OutputTokens, &b.CostUSD, &b.BaselineCostUSD, &b.CGLLMCostUSD,
			&cgAvg, &upAvg, &b.Expands, &b.ExpandTokens, &b.Misses); err != nil {
			return nil, err
		}
		b.CGLatencyMs, b.UpstreamMs = cgAvg.Float64, upAvg.Float64
		b.Saved = b.TokensBefore - b.TokensAfter
		out = append(out, &b)
	}
	return out, rows.Err()
}

// facetQueries are the distinct-value lists that populate the filter dropdowns.
var facetQueries = map[string]string{
	"model":    "model",
	"provider": "provider",
	"agent":    "agent",
	"preset":   "preset",
	"mode":     "mode",
	"reason":   "uncompressed_reason",
}

// selfBlanked returns f with ONE dimension's own value cleared.
//
// A dropdown must not be scoped by its own selection. With agent=bob set, scoping the
// agent list by the whole filter returns exactly ["bob"], so that dimension becomes a
// one-way door — the only way to reach another agent is to clear every filter, which is
// precisely the "I have to press Clear every time" complaint. Every OTHER dimension
// stays scoped, which is the half that earns its keep: with agent=bob set, the model
// list should still narrow to the models bob actually used.
//
// Tenant and TenantAll are deliberately NOT in the switch. They are not user-facing
// dimensions, they are the authorization scope the API layer overwrites from the
// authenticated principal — blanking either here would turn a dropdown into a
// cross-tenant enumeration, which is the bug this file already fixed once for the
// component list.
func selfBlanked(f Filter, dim string) Filter {
	switch dim {
	case "model":
		f.Model = ""
	case "provider":
		f.Provider = ""
	case "agent":
		f.Agent = ""
	case "preset":
		f.Preset = ""
	case "mode":
		f.Mode = ""
	case "reason":
		f.Reason = ""
	case "component":
		f.Component = ""
	}
	return f
}

// Facets returns the distinct values available for each filter dimension, so the
// UI's dropdowns show only what the data actually contains.
func (d *DB) Facets(f Filter) (map[string][]string, error) {
	out := map[string][]string{}
	for name, col := range facetQueries {
		cond, args := selfBlanked(f, name).where()
		rows, err := d.sql.Query(
			`SELECT DISTINCT r.`+col+` FROM requests r WHERE `+cond+` AND r.`+col+` <> '' ORDER BY 1 LIMIT 200`, args...)
		if err != nil {
			return nil, err
		}
		var vals []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			vals = append(vals, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out[name] = vals
	}
	// Components come from the join table, not a requests column — so the scoping has
	// to be written out via the join rather than inherited from the loop above. It was
	// missing here, which made one tenant's dropdown an enumeration of every component
	// every OTHER tenant runs.
	ccond, cargs := selfBlanked(f, "component").where()
	rows, err := d.sql.Query(`SELECT DISTINCT c.component
		FROM request_components c JOIN requests r ON r.id = c.request_id
		WHERE `+ccond+` ORDER BY 1 LIMIT 200`, cargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comps []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		comps = append(comps, v)
	}
	out["component"] = comps
	return out, rows.Err()
}
