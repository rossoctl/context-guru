package dash

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
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
	// Effort, Thinking and StopReason select on captured request metadata — the
	// drill-down from a breakdown chart into the rows behind one of its bars.
	Effort     string
	Thinking   string
	StopReason string
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
		"r.reasoning_effort": f.Effort, "r.thinking_mode": f.Thinking,
		"r.stop_reason": f.StopReason,
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
	r.token_accounting, r.cache_miss_reason, r.uncompressed_reason,
	r.reasoning_effort, r.thinking_mode, r.thinking_budget, r.temperature, r.top_p,
	r.max_tokens, r.stream, r.tool_choice, r.tools, r.system_blocks,
	r.cache_bp_system, r.cache_bp_tools, r.cache_bp_messages, r.cache_bp_blocks, r.stop_reason`

func scanRequest(rows interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	var byp, ca, stream int
	var temp, topP sql.NullFloat64
	err := rows.Scan(&e.ID, &e.TS, &e.TenantID, &e.SessionID, &e.Model, &e.Provider, &e.Agent, &e.Preset, &e.Mode, &e.Route,
		&e.Status, &byp, &ca, &e.Messages, &e.TokensBefore, &e.TokensAfter,
		&e.AttemptedTokens, &e.FrozenTokens, &e.SavedUnique, &e.FreshInput, &e.CacheRead,
		&e.CacheWrite, &e.OutputTokens, &e.CostUSD, &e.BaselineCostUSD, &e.CGLLMCostUSD,
		&e.CGLatencyMs, &e.UpstreamMs, &e.Expands, &e.ExpandTokens, &e.Reverts,
		&e.TokenAccounting, &e.CacheMissReason, &e.UncompressedReason,
		&e.ReasoningEffort, &e.ThinkingMode, &e.ThinkingBudget, &temp, &topP,
		&e.MaxTokens, &stream, &e.ToolChoice, &e.Tools, &e.SystemBlocks,
		&e.CacheBPSystem, &e.CacheBPTools, &e.CacheBPMessages, &e.CacheBPBlocks, &e.StopReason)
	e.Bypassed, e.CacheAware, e.Stream = byp != 0, ca != 0, stream != 0
	// NULL stays absent rather than becoming 0: a request that set temperature=0 and one
	// that set nothing must not read the same on the row.
	if temp.Valid {
		e.Temperature = &temp.Float64
	}
	if topP.Valid {
		e.TopP = &topP.Float64
	}
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
		saved_gross, saved_unique, duration_ms, err, gates FROM request_components
		WHERE request_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var c CompRow
		var a, m, rv, sk int
		var gates string
		if err := crows.Scan(&c.Component, &c.Kind, &a, &m, &rv, &sk,
			&c.SavedGross, &c.SavedUnique, &c.DurationMs, &c.Err, &gates); err != nil {
			return nil, err
		}
		c.Acted, c.Mutated, c.Reverted, c.Skipped = a != 0, m != 0, rv != 0, sk != 0
		if gates != "" {
			// A row written before the column existed, or one whose JSON is somehow
			// unreadable, leaves Gates nil — which the UI shows as "unknown", not as
			// "gated nothing".
			_ = json.Unmarshal([]byte(gates), &c.Gates)
		}
		e.Components = append(e.Components, c)
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}
	// Recorded model calls. Always loaded, even without content access: the numbers on these
	// rows (cost, latency, tokens, gate reason, saving) are operational metrics rather than
	// transcript content, and they are what answers "was this call worth it?". The text
	// halves are loaded only WITH content access, below.
	xrows, err := d.sql.Query(`SELECT component, model, strategy, aggressiveness, cold, escalated,
		candidate_tokens, saved_tokens, prompt_tokens, completion_tokens, cache_read, cache_write,
		cost_usd, latency_ms, accepted, gate_reason, rejection, summary, before_gz, after_gz
		FROM extraction_calls WHERE request_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer xrows.Close()
	for xrows.Next() {
		var x ExtractionRow
		var cold, esc, acc int
		var bz, az []byte
		if err := xrows.Scan(&x.Component, &x.Model, &x.Strategy, &x.Aggressiveness, &cold, &esc,
			&x.CandidateTokens, &x.SavedTokens, &x.PromptTokens, &x.CompletionTok,
			&x.CacheRead, &x.CacheWrite, &x.CostUSD, &x.LatencyMs, &acc,
			&x.GateReason, &x.Rejection, &x.Summary, &bz, &az); err != nil {
			return nil, err
		}
		x.Cold, x.Escalated, x.Accepted = cold != 0, esc != 0, acc != 0
		if withContent {
			x.Before, x.After = gunzipText(bz), gunzipText(az)
		} else {
			x.Summary = "" // model-written text about a tool output; it can quote from it
		}
		e.Extractions = append(e.Extractions, x)
	}
	if err := xrows.Err(); err != nil {
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
	// LLM-call economics, from the recorded per-call rows. Zero for every deterministic
	// component, which is the point: only the components that SPEND can be net-negative,
	// and until now the components view had no dollars in it at all, so it judged an
	// expensive component on tokens and latency and could never say "underwater".
	LLMCalls        int64   `json:"llm_calls"`
	LLMCallsCold    int64   `json:"llm_calls_cold"`
	LLMCallsAcc     int64   `json:"llm_calls_accepted"`
	LLMCostUSD      float64 `json:"llm_cost_usd"`
	LLMLatencyMsAvg float64 `json:"llm_latency_ms_avg"`
	// LLMSavedTokens is what the CALLS removed, which is not the same as SavedUnique: the
	// component's savings also include frozen results replayed with no call at all, and on
	// measured traffic that replay is ~93% of its realized value.
	LLMSavedTokens int64 `json:"llm_saved_tokens"`
	// Gates totals the named reasons this component turned candidates away over the
	// window. For a component with act_rate 0 it is the whole story, and it used to be
	// visible only in /stats (service-wide) and the log line (per request) — never in the
	// dashboard, which is what a user opens when they ask why nothing was compacted.
	Gates map[string]int64 `json:"gates,omitempty"`
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The per-call economics, in a second pass over the same filtered window. A JOIN into
	// the query above would multiply the component rows by the number of calls and silently
	// inflate every SUM in it.
	xq := `SELECT x.component, COUNT(*), SUM(x.cold), SUM(x.accepted), SUM(x.cost_usd),
		AVG(x.latency_ms), SUM(x.saved_tokens)
		FROM extraction_calls x JOIN requests r ON r.id = x.request_id
		WHERE ` + cond + ` GROUP BY x.component`
	xrows, err := d.sql.Query(xq, args...)
	if err != nil {
		return nil, err
	}
	defer xrows.Close()
	byName := map[string]*ComponentRow{}
	for _, c := range out {
		byName[c.Component] = c
	}
	for xrows.Next() {
		var name string
		var calls, cold, acc, saved int64
		var cost, lat sql.NullFloat64
		if err := xrows.Scan(&name, &calls, &cold, &acc, &cost, &lat, &saved); err != nil {
			return nil, err
		}
		c, ok := byName[name]
		if !ok {
			continue // a call recorded against a component with no surviving component row
		}
		c.LLMCalls, c.LLMCallsCold, c.LLMCallsAcc = calls, cold, acc
		c.LLMCostUSD, c.LLMLatencyMsAvg, c.LLMSavedTokens = cost.Float64, lat.Float64, saved
	}
	if err := xrows.Err(); err != nil {
		return nil, err
	}
	// Gate totals, summed in SQL with json_each rather than by decoding a map per row in
	// Go: a filtered window is hundreds of thousands of component rows and the gate map is
	// the widest text on each of them.
	gq := `SELECT c.component, j.key, SUM(CAST(j.value AS INTEGER))
		FROM request_components c JOIN requests r ON r.id = c.request_id, json_each(c.gates) j
		WHERE ` + cond + ` AND c.gates <> '' GROUP BY 1, 2`
	grows, err := d.sql.Query(gq, args...)
	if err != nil {
		return nil, err
	}
	defer grows.Close()
	for grows.Next() {
		var name, gate string
		var n int64
		if err := grows.Scan(&name, &gate, &n); err != nil {
			return nil, err
		}
		c, ok := byName[name]
		if !ok {
			continue
		}
		if c.Gates == nil {
			c.Gates = map[string]int64{}
		}
		c.Gates[gate] += n
	}
	return out, grows.Err()
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
	// SavedUSD is baseline − actual − our own spend: the "saved" half of a spent-vs-saved
	// bar, derived here so every caller subtracts the same three terms. Nothing new is
	// stored for it.
	SavedUSD float64 `json:"saved_usd"`
	// CacheSavedUSD is what the provider's prompt cache saved in this bucket over
	// paying the fresh rate — the second savings series, and the one that moves when
	// compaction starts rewriting a live prefix.
	CacheSavedUSD float64 `json:"cache_saved_usd"`
	CGLatencyMs   float64 `json:"cg_latency_ms_avg"`
	UpstreamMs    float64 `json:"upstream_ms_avg"`
	Expands       int64   `json:"expands"`
	ExpandTokens  int64   `json:"expand_tokens"`
	Misses        int64   `json:"cache_misses"`
}

// DayMs is one day of buckets. Per-DAY usage bars are Series with this bucket — the
// bucketing is done in SQL from the raw ts (see the package comment), so a day-wide
// bucket needs no new query, no rollup table and no migration, and the "selectable time
// range" is the Since/Until the filter already carries.
//
// Buckets are UTC days, matching the tenant_spend month key, rather than the viewer's
// local day. That is a deliberate limitation and not a rounding error: a local-day
// bucket would make the same row land in different bars for two people reading the same
// dashboard.
// ponytail: UTC days; take an offset parameter here if per-viewer local days ever matter.
const DayMs int64 = 24 * 60 * 60 * 1000

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
		SUM(r.cost_usd), SUM(r.baseline_cost_usd), SUM(r.cg_llm_cost_usd), SUM(r.cache_saved_usd),
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
			&b.OutputTokens, &b.CostUSD, &b.BaselineCostUSD, &b.CGLLMCostUSD, &b.CacheSavedUSD,
			&cgAvg, &upAvg, &b.Expands, &b.ExpandTokens, &b.Misses); err != nil {
			return nil, err
		}
		b.CGLatencyMs, b.UpstreamMs = cgAvg.Float64, upAvg.Float64
		b.Saved = b.TokensBefore - b.TokensAfter
		b.SavedUSD = b.BaselineCostUSD - b.CostUSD - b.CGLLMCostUSD
		out = append(out, &b)
	}
	return out, rows.Err()
}

// GroupRow is one bar of a breakdown: everything a spent-vs-saved comparison needs for
// one value of one dimension.
type GroupRow struct {
	// Key is the dimension's value. "" means the request did not carry it (no effort set,
	// no stop reason reported) and the UI must label it as unset rather than hide it —
	// "most of my traffic sets no effort at all" is a finding, not a gap.
	Key             string  `json:"key"`
	Requests        int64   `json:"requests"`
	Sessions        int64   `json:"sessions"`
	TokensBefore    int64   `json:"tokens_before"`
	TokensAfter     int64   `json:"tokens_after"`
	Saved           int64   `json:"saved"`
	SavedUnique     int64   `json:"saved_unique"`
	FreshInput      int64   `json:"fresh_input"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	OutputTokens    int64   `json:"output_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	// SpentUSD is what was actually paid (billed + our own spend); SavedUSD is baseline
	// minus that. The pair is the "spent vs saved" comparison, per group.
	SpentUSD float64 `json:"spent_usd"`
	SavedUSD float64 `json:"saved_usd"`
	// CacheSavedUSD is the prompt-cache saving for this group, and TotalSavedUSD is
	// both savings together. Per MODEL this is the number that was most wrong before:
	// the rates now come from the operator's price list, so a gateway that charges half
	// the public API's rate is no longer reported at the public rate.
	CacheSavedUSD float64 `json:"cache_saved_usd"`
	TotalSavedUSD float64 `json:"total_saved_usd"`
	// Incomplete counts rows whose accounting is not `complete`, i.e. rows whose cost
	// contribution is unknown rather than zero. A bar with Requests>0 and
	// Incomplete==Requests is a bar whose money figures mean nothing, and the UI has to
	// say so instead of drawing a zero.
	Incomplete int64 `json:"incomplete_rows"`
}

// breakdownDims are the dimensions a breakdown may group by, mapped to the SQL that
// produces the key. An ALLOWLIST, and that is the whole point: the dimension arrives in a
// query parameter and is interpolated into the statement, so anything not on this map is
// refused rather than escaped.
//
// The numeric dimensions are cast to TEXT so one row type serves every dimension —
// `cache_breakpoints` is the count of breakpoints on the request, which is the placement
// question this project exists to answer, asked in dollars.
//
// That count is made BY THE PIPELINE, so on an observe-mode row it was never made: the
// enforced path runs no pipeline, the four columns read zero like every other
// trace-derived field, and casting them would key the row as a counted "0" — the same bar
// as a request that genuinely arrived without a breakpoint. Keyed as "" instead, which
// GroupRow.Key documents the UI as labelling unset. It matters more here than for an
// obviously-empty field: a proxy running wholly in observe mode would otherwise draw one
// full-height "0" bar and read as the finding "none of my traffic sets a breakpoint",
// about traffic nobody inspected. Bypassed rows are NOT affected — apply counts
// breakpoints before it checks bypass, so their zero is a real zero.
var breakdownDims = map[string]string{
	// tenant is the dimension a MANAGER groups by, and it is what makes an A/B comparison
	// possible without a second column: a variant is a set of tenants, so per-tenant rows
	// fold into per-variant rows by summing (see the /api/variants rollup in proxy). Safe
	// for everyone else by construction — a.scope overwrites Filter.Tenant from the
	// principal, so a plain user grouping by tenant gets exactly one group: their own.
	"tenant":            "r.tenant_id",
	"model":             "r.model",
	"provider":          "r.provider",
	"agent":             "r.agent",
	"preset":            "r.preset",
	"mode":              "r.mode",
	"reasoning_effort":  "r.reasoning_effort",
	"thinking_mode":     "r.thinking_mode",
	"stop_reason":       "r.stop_reason",
	"tool_choice":       "r.tool_choice",
	"cache_miss_reason": "r.cache_miss_reason",
	"cache_breakpoints": "CASE WHEN r.mode = '" + ModeObserve + "' THEN '' ELSE " +
		"CAST(r.cache_bp_system + r.cache_bp_tools + r.cache_bp_messages + r.cache_bp_blocks AS TEXT) END",
	"stream": "CASE WHEN r.stream <> 0 THEN 'stream' ELSE 'unary' END",
}

// BreakdownDims lists the valid dimensions, sorted, for the API's error message and the
// UI's dimension picker.
func BreakdownDims() []string {
	out := make([]string, 0, len(breakdownDims))
	for k := range breakdownDims {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Breakdown aggregates the filtered window by one dimension: requests, tokens, and
// SPENT VS SAVED for each value. One query rather than one per dimension — per-model
// cost, cost by reasoning effort, and cost by cache_control breakpoint count are the same
// GROUP BY over the same columns, and three near-identical functions is three places for
// the savings arithmetic to drift.
//
// An unknown dimension is an error rather than a silent fallback: a caller that
// mistypes the dimension must not be handed a chart of some other dimension's numbers.
func (d *DB) Breakdown(f Filter, dim string) ([]*GroupRow, error) {
	expr, ok := breakdownDims[dim]
	if !ok {
		return nil, fmt.Errorf("dash: unknown breakdown dimension %q", dim)
	}
	cond, args := f.where()
	q := `SELECT ` + expr + ` AS k, COUNT(*), COUNT(DISTINCT r.session_id),
		COALESCE(SUM(r.tokens_before),0), COALESCE(SUM(r.tokens_after),0), COALESCE(SUM(r.saved_unique),0),
		COALESCE(SUM(r.fresh_input),0), COALESCE(SUM(r.cache_read),0), COALESCE(SUM(r.cache_write),0),
		COALESCE(SUM(r.output_tokens),0),
		COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
		COALESCE(SUM(r.cache_saved_usd),0),
		COALESCE(SUM(CASE WHEN r.token_accounting <> 'complete' THEN 1 ELSE 0 END),0)
		FROM requests r WHERE ` + cond + `
		GROUP BY k ORDER BY SUM(r.cost_usd) DESC, COUNT(*) DESC, k LIMIT 200`
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GroupRow{}
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.Key, &g.Requests, &g.Sessions,
			&g.TokensBefore, &g.TokensAfter, &g.SavedUnique,
			&g.FreshInput, &g.CacheRead, &g.CacheWrite, &g.OutputTokens,
			&g.CostUSD, &g.BaselineCostUSD, &g.CGLLMCostUSD, &g.CacheSavedUSD, &g.Incomplete); err != nil {
			return nil, err
		}
		g.Saved = g.TokensBefore - g.TokensAfter
		g.SpentUSD = g.CostUSD + g.CGLLMCostUSD
		g.SavedUSD = g.BaselineCostUSD - g.SpentUSD
		g.TotalSavedUSD = g.SavedUSD + g.CacheSavedUSD
		out = append(out, &g)
	}
	return out, rows.Err()
}

// facetQueries are the distinct-value lists that populate the filter dropdowns.
var facetQueries = map[string]string{
	"model":       "model",
	"provider":    "provider",
	"agent":       "agent",
	"preset":      "preset",
	"mode":        "mode",
	"reason":      "uncompressed_reason",
	"effort":      "reasoning_effort",
	"thinking":    "thinking_mode",
	"stop_reason": "stop_reason",
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
	case "effort":
		f.Effort = ""
	case "thinking":
		f.Thinking = ""
	case "stop_reason":
		f.StopReason = ""
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
