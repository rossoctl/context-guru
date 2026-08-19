package dash

// The read side of tool/MCP/skill inventory: "how much of what you carry do you
// actually use", per session, per account, and across a team.
//
// The headline number is the NEVER-USED token weight, and its price. Both need care,
// which is why this is not a SUM in one query:
//
//   - The weight is per SESSION, but the cost is per REQUEST: a declaration sits in the
//     cached prefix and is re-read by every turn of the session (measured: 65.2 requests
//     per claude-cli session with tools). The multiplier is therefore counted from the
//     requests table, not assumed.
//   - Each of those re-reads is billed at the tier that request actually paid — cache
//     read for a hit, cache CREATION for the turn that wrote the prefix, fresh input when
//     there was no cache at all. Pricing a cold start at the read rate would understate
//     it and pricing every request at the fresh rate would inflate the whole figure 10x.
//   - A session with no inventory rows is NOT a session with nothing unused. Every row
//     in the production database predates this capture, so "not captured" is a first-class
//     answer here: the report counts those sessions separately and never folds them into
//     a zero.

import (
	"database/sql"
	"net/http"
	"sort"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ToolStat is one declared capability, with what it costs and whether it earns it.
type ToolStat struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// Tokens is what carrying this declaration costs on ONE request.
	Tokens int `json:"tokens"`
	// SessionsDeclared / SessionsUsed / Calls: how often it was carried, how often it was
	// invoked at all, and how many calls it received.
	SessionsDeclared int `json:"sessions_declared"`
	SessionsUsed     int `json:"sessions_used"`
	Calls            int `json:"calls"`
	// UnusedReads is Tokens x the requests that re-read it in the sessions that never
	// invoked it: the actual number of billed tokens spent carrying it for nothing.
	UnusedReads int64 `json:"unused_reads"`
	// UnusedUSD prices UnusedReads at the tier each of those requests was billed at.
	// Omitted (with Priced=false) when the model's rates are unknown — an unpriced number
	// must not read as "this cost nothing".
	UnusedUSD float64 `json:"unused_usd"`
	Priced    bool    `json:"priced"`
}

// ServerStat rolls a whole MCP server up: an MCP server is what a user adds or removes,
// so it is the unit the decision is actually made in.
type ServerStat struct {
	Server           string  `json:"server"`
	Tools            int     `json:"tools"`
	ToolsUsed        int     `json:"tools_used"`
	Tokens           int     `json:"tokens"`
	SessionsDeclared int     `json:"sessions_declared"`
	SessionsUsed     int     `json:"sessions_used"`
	Calls            int     `json:"calls"`
	UnusedReads      int64   `json:"unused_reads"`
	UnusedUSD        float64 `json:"unused_usd"`
	Priced           bool    `json:"priced"`
}

// SkillStat is the skills half. It is reported apart from tools because skills are
// declared as PROSE in a role:"system" message rather than as JSON — so the inventory
// can be unknown, which no tools inventory can be.
type SkillStat struct {
	// State is ok when the listing parsed, unknown when one was present and did not, and
	// absent when no session in scope carried one. A reader must not translate unknown
	// into "no skills": see dash/toolinventory.go.
	State string `json:"state"`
	// UnknownSessions is how many sessions carried a listing this parser could not read.
	UnknownSessions int `json:"unknown_sessions"`
	// ListingTokens is the prose listing's own cost per request, averaged over the
	// sessions that carried one.
	ListingTokens int `json:"listing_tokens"`
	Declared      int `json:"declared"`
	Invoked       int `json:"invoked"`
	Calls         int `json:"calls"`
	// UnusedListingReads / UnusedListingUSD are the listing's OWN weight in the sessions
	// that invoked no skill at all: the prose is one indivisible block, so it is waste only
	// when nothing in it was used.
	UnusedListingReads int64   `json:"unused_listing_reads"`
	UnusedListingUSD   float64 `json:"unused_listing_usd"`
	// Skills is the per-skill detail, same shape as a tool.
	Skills []ToolStat `json:"skills"`
}

// ToolCoverage is the honesty half of the report: which sessions in scope can answer
// the question at all.
type ToolCoverage struct {
	Sessions int `json:"sessions"`
	// Captured / NotCaptured: sessions with an inventory, and sessions whose requests
	// predate this capture (or were dropped). NotCaptured sessions contribute NOTHING to
	// any figure above — they are not counted as fully-used and not as fully-wasted.
	Captured    int `json:"captured"`
	NotCaptured int `json:"not_captured"`
	// UnpricedSessions carried an inventory but a model with no known rates, so their
	// tokens are in UnusedReads and their dollars are in nobody's total.
	UnpricedSessions int `json:"unpriced_sessions"`
	Requests         int `json:"requests"`
}

// ToolTotals is the summary line.
type ToolTotals struct {
	// DeclaredTokens / UnusedTokens are per-session averages over the CAPTURED sessions.
	DeclaredTokens int     `json:"declared_tokens"`
	UnusedTokens   int     `json:"unused_tokens"`
	UnusedPct      float64 `json:"unused_pct"`
	// UnusedReads and UnusedUSD are the totals over the whole scope: the billed tokens
	// spent carrying never-invoked declarations, and their price.
	UnusedReads int64   `json:"unused_reads"`
	UnusedUSD   float64 `json:"unused_usd"`
	Priced      bool    `json:"priced"`
	// RequestsPerSession is the re-read multiplier the figures above rest on.
	RequestsPerSession float64 `json:"requests_per_session"`
}

// ToolReport is the whole answer for one scope.
type ToolReport struct {
	Coverage ToolCoverage `json:"coverage"`
	Totals   ToolTotals   `json:"totals"`
	Tools    []ToolStat   `json:"tools"`
	Servers  []ServerStat `json:"servers"`
	Skills   SkillStat    `json:"skills"`
}

// sessionCost is one session's re-read multiplier, split by the tier each request was
// billed at, plus the price of the model that billed it.
type sessionCost struct {
	reads, writes, fresh int
	price                modelinfo.Price
	priced               bool
}

// requests is how many times this session re-read its declarations.
func (s sessionCost) requests() int { return s.reads + s.writes + s.fresh }

// usd values n tokens carried through this session at the tiers it actually paid.
func (s sessionCost) usd(n int) float64 {
	return float64(n) * (float64(s.reads)*s.price.CacheRead +
		float64(s.writes)*s.price.CacheWrite + float64(s.fresh)*s.price.Input)
}

// ToolReportFor builds the report for one filter scope. price resolves a model's rates
// and may be nil, in which case nothing is priced and the report says so.
//
// Aggregation happens in Go rather than in one SQL statement on purpose: the rows are
// per session (a few tens per session, a few thousand in a scope), and the pricing step
// needs each session's own model and cache tiers — which is a join no aggregate SUM can
// express honestly.
func (d *DB) ToolReportFor(f Filter, price func(string) (modelinfo.Price, bool)) (*ToolReport, error) {
	where, args := f.where()
	// Sessions in scope, their re-read multiplier and the tiers they paid. tools>0
	// because a request that declared nothing has no inventory to be missing.
	rows, err := d.sql.Query(`SELECT r.session_id, r.model,
		SUM(CASE WHEN r.cache_read > 0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN r.cache_read = 0 AND r.cache_write > 0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN r.cache_read = 0 AND r.cache_write = 0 THEN 1 ELSE 0 END)
		FROM requests r WHERE `+where+` AND r.tools > 0 GROUP BY 1, 2`, args...)
	if err != nil {
		return nil, err
	}
	sessions := map[string]*sessionCost{}
	for rows.Next() {
		var id, model string
		var reads, writes, fresh int
		if err := rows.Scan(&id, &model, &reads, &writes, &fresh); err != nil {
			rows.Close()
			return nil, err
		}
		sc := sessions[id]
		if sc == nil {
			sc = &sessionCost{}
			sessions[id] = sc
		}
		sc.reads, sc.writes, sc.fresh = sc.reads+reads, sc.writes+writes, sc.fresh+fresh
		if price != nil {
			// A session that used two models keeps the FIRST priced one: mixing rates across
			// a single token count would produce a figure that belongs to no model.
			if p, ok := price(model); ok && !sc.priced {
				sc.price, sc.priced = p, true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rep := &ToolReport{}
	if len(sessions) == 0 {
		rep.Skills.State = "absent"
		return rep, nil
	}

	decls, err := d.scopedDecls(f, where, args)
	if err != nil {
		return nil, err
	}
	uses, err := d.scopedUses(f, where, args)
	if err != nil {
		return nil, err
	}
	return buildToolReport(sessions, decls, uses), nil
}

// declRow / useRow are the scoped raw rows, one per session and name.
type declRow struct {
	session, kind, name, server string
	tokens                      int
}
type useRow struct {
	session, name, skill string
	calls                int
}

// scopedDecls reads the declarations of the sessions the filter selects. The tenant
// predicate is applied to THIS table as well as to the subquery: the rows carry their
// own tenant_id, and a scoping bug in one place should not be enough to cross accounts.
func (d *DB) scopedDecls(f Filter, where string, args []any) ([]declRow, error) {
	q := `SELECT d.session_id, d.kind, d.name, d.server, MAX(d.tokens)
		FROM tool_declarations d WHERE d.session_id IN
		  (SELECT r.session_id FROM requests r WHERE ` + where + ` AND r.tools > 0)`
	a := append([]any{}, args...)
	if !f.TenantAll {
		q += ` AND d.tenant_id = ?`
		a = append(a, f.Tenant)
	}
	q += ` GROUP BY 1, 2, 3, 4`
	rows, err := d.sql.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []declRow
	for rows.Next() {
		var r declRow
		if err := rows.Scan(&r.session, &r.kind, &r.name, &r.server, &r.tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scopedUses reads the invocations of the sessions the filter selects.
func (d *DB) scopedUses(f Filter, where string, args []any) ([]useRow, error) {
	q := `SELECT u.session_id, u.name, u.skill, SUM(u.calls)
		FROM tool_uses u WHERE u.session_id IN
		  (SELECT r.session_id FROM requests r WHERE ` + where + ` AND r.tools > 0)`
	a := append([]any{}, args...)
	if !f.TenantAll {
		q += ` AND u.tenant_id = ?`
		a = append(a, f.Tenant)
	}
	q += ` GROUP BY 1, 2, 3`
	rows, err := d.sql.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []useRow
	for rows.Next() {
		var r useRow
		if err := rows.Scan(&r.session, &r.name, &r.skill, &r.calls); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// statKey identifies one declared capability across sessions.
type statKey struct{ kind, name string }

// buildToolReport does the declared-vs-used diff and the pricing. Split out from the
// queries so the arithmetic — which is the part that can be wrong in an expensive
// direction — is testable without a database.
func buildToolReport(sessions map[string]*sessionCost, decls []declRow, uses []useRow) *ToolReport {
	rep := &ToolReport{}
	// Which capabilities each session invoked. A skill counts as used under its own name,
	// and the Skill tool counts as used by any skill call.
	usedIn := map[string]map[statKey]int{}
	for _, u := range uses {
		m := usedIn[u.session]
		if m == nil {
			m = map[statKey]int{}
			usedIn[u.session] = m
		}
		m[statKey{KindTool, u.name}] += u.calls
		if _, _, ok := SplitMCPName(u.name); ok {
			m[statKey{KindMCPTool, u.name}] += u.calls
		} else {
			m[statKey{KindServerTool, u.name}] += u.calls
		}
		if u.skill != "" {
			m[statKey{KindSkill, u.skill}] += u.calls
		}
	}

	stats := map[statKey]*ToolStat{}
	captured := map[string]bool{}
	declTok, unusedTok := map[string]int{}, map[string]int{}
	listingTok, listingSessions, unknownListings := 0, 0, 0
	skillsSeen := false
	for _, dr := range decls {
		captured[dr.session] = true
		sc := sessions[dr.session]
		if sc == nil {
			continue // not in scope (defensive: the query joined on scope)
		}
		if dr.kind == KindSkillListing {
			skillsSeen = true
			listingTok += dr.tokens
			listingSessions++
			if dr.server == SkillsUnknown {
				unknownListings++
			}
			// The listing's own tokens count as declared weight, and as WASTE only when the
			// session invoked no skill at all — the prose is one indivisible block.
			declTok[dr.session] += dr.tokens
			if usedSkill(usedIn[dr.session]) == 0 {
				unusedTok[dr.session] += dr.tokens
				rep.Skills.UnusedListingReads += int64(dr.tokens) * int64(sc.requests())
				if sc.priced {
					rep.Skills.UnusedListingUSD += sc.usd(dr.tokens)
				}
			}
			continue
		}
		k := statKey{dr.kind, dr.name}
		st := stats[k]
		if st == nil {
			st = &ToolStat{Kind: dr.kind, Name: dr.name, Server: dr.server, Priced: true}
			stats[k] = st
		}
		if dr.tokens > st.Tokens {
			st.Tokens = dr.tokens
		}
		st.SessionsDeclared++
		declTok[dr.session] += dr.tokens
		calls := usedIn[dr.session][k]
		if calls > 0 {
			st.SessionsUsed++
			st.Calls += calls
			continue
		}
		unusedTok[dr.session] += dr.tokens
		st.UnusedReads += int64(dr.tokens) * int64(sc.requests())
		if sc.priced {
			st.UnusedUSD += sc.usd(dr.tokens)
		} else {
			st.Priced = false
		}
	}

	// Coverage, and the honest denominator: only captured sessions can say anything about
	// what they did not use.
	for id, sc := range sessions {
		rep.Coverage.Sessions++
		rep.Coverage.Requests += sc.requests()
		if !captured[id] {
			rep.Coverage.NotCaptured++
			continue
		}
		rep.Coverage.Captured++
		if !sc.priced {
			rep.Coverage.UnpricedSessions++
		}
	}
	rep.Totals.Priced = rep.Coverage.Captured > 0 && rep.Coverage.UnpricedSessions == 0
	if n := rep.Coverage.Captured; n > 0 {
		var dsum, usum int
		for id := range captured {
			dsum += declTok[id]
			usum += unusedTok[id]
		}
		rep.Totals.DeclaredTokens = dsum / n
		rep.Totals.UnusedTokens = usum / n
		if dsum > 0 {
			rep.Totals.UnusedPct = 100 * float64(usum) / float64(dsum)
		}
	}
	if rep.Coverage.Sessions > 0 {
		rep.Totals.RequestsPerSession = float64(rep.Coverage.Requests) / float64(rep.Coverage.Sessions)
	}

	for _, st := range stats {
		rep.Totals.UnusedReads += st.UnusedReads
		rep.Totals.UnusedUSD += st.UnusedUSD
		if st.Kind == KindSkill {
			rep.Skills.Skills = append(rep.Skills.Skills, *st)
			rep.Skills.Declared++
			if st.SessionsUsed > 0 {
				rep.Skills.Invoked++
				rep.Skills.Calls += st.Calls
			}
			continue
		}
		rep.Tools = append(rep.Tools, *st)
	}
	rep.Totals.UnusedReads += rep.Skills.UnusedListingReads
	rep.Totals.UnusedUSD += rep.Skills.UnusedListingUSD

	rep.Skills.State = "absent"
	switch {
	case unknownListings > 0 && rep.Skills.Declared == 0:
		rep.Skills.State = SkillsUnknown
	case skillsSeen:
		rep.Skills.State = SkillsOK
	}
	rep.Skills.UnknownSessions = unknownListings
	if listingSessions > 0 {
		rep.Skills.ListingTokens = listingTok / listingSessions
	}

	rep.Servers = serverRollup(rep.Tools)
	sortStats(rep.Tools)
	sortStats(rep.Skills.Skills)
	return rep
}

// usedSkill reports how many skill invocations a session made.
func usedSkill(m map[statKey]int) int {
	n := 0
	for k, c := range m {
		if k.kind == KindSkill {
			n += c
		}
	}
	return n
}

// serverRollup groups the MCP tools by their server.
func serverRollup(tools []ToolStat) []ServerStat {
	by := map[string]*ServerStat{}
	for _, t := range tools {
		if t.Kind != KindMCPTool || t.Server == "" {
			continue
		}
		s := by[t.Server]
		if s == nil {
			s = &ServerStat{Server: t.Server, Priced: true}
			by[t.Server] = s
		}
		s.Tools++
		s.Tokens += t.Tokens
		s.Calls += t.Calls
		s.UnusedReads += t.UnusedReads
		s.UnusedUSD += t.UnusedUSD
		if t.SessionsUsed > 0 {
			s.ToolsUsed++
		}
		if t.SessionsDeclared > s.SessionsDeclared {
			s.SessionsDeclared = t.SessionsDeclared
		}
		if t.SessionsUsed > s.SessionsUsed {
			s.SessionsUsed = t.SessionsUsed
		}
		if !t.Priced {
			s.Priced = false
		}
	}
	out := make([]ServerStat, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UnusedReads != out[j].UnusedReads {
			return out[i].UnusedReads > out[j].UnusedReads
		}
		return out[i].Server < out[j].Server
	})
	return out
}

// sortStats orders by what the page is for: the most wasted weight first.
func sortStats(s []ToolStat) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].UnusedReads != s[j].UnusedReads {
			return s[i].UnusedReads > s[j].UnusedReads
		}
		if s[i].Tokens != s[j].Tokens {
			return s[i].Tokens > s[j].Tokens
		}
		return s[i].Name < s[j].Name
	})
}

// toolRoutes is this feature's mounted surface, in the same shape as api.go's table so
// the scope decision is data here too. scopeTenant, not scopeManager: the rows are the
// caller's own configuration, and a manager reaches the team's view through the same
// ?tenant= parameter every other tenant-scoped route honours.
func (a *API) toolRoutes() []route {
	return []route{
		{"GET /api/tools", scopeTenant, a.tools},
		// The decision surface: what is currently excluded, what excluding it has actually
		// saved, and what is safe to offer next. scopeTenant like the report it reads. The
		// WRITE half is the control plane's (POST /api/toolfilter) — see dash/toolsuggest.go.
		{"GET /api/toolfilter", scopeTenant, a.toolFilterDoc},
	}
}

// MountTools registers the inventory routes. Separate from Mount only to keep this
// feature additive to the API surface; a caller mounts both.
func (a *API) MountTools(m *http.ServeMux) {
	for _, rt := range a.toolRoutes() {
		m.HandleFunc(rt.pattern, rt.h)
	}
}

// tools serves the declared-vs-used report for the caller's scope. ?session= narrows it
// to one session (through the standard filter, so an id belonging to someone else
// simply selects nothing rather than 403-ing and confirming that it exists).
func (a *API) tools(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	rep, err := a.rec.DB().ToolReportFor(f, a.priceFn(r))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not read the tool inventory")
		return
	}
	writeJSON(w, rep)
}

// priceFn resolves a model's rates for the duration of one request, or nil when the
// deployment has no pricer. Shared by every route that has to price a token count at the
// tier it was billed at, so two of them cannot disagree about what "unpriced" means.
func (a *API) priceFn(r *http.Request) func(string) (modelinfo.Price, bool) {
	if a.pricer == nil {
		return nil
	}
	ctx := r.Context()
	return func(model string) (modelinfo.Price, bool) {
		if model == "" {
			return modelinfo.Price{}, false
		}
		return a.pricer.Price(ctx, model)
	}
}

// countInventoryRows is a test and diagnostic helper: how many inventory rows exist.
func (d *DB) countInventoryRows() (decls, uses int64, err error) {
	err = d.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tool_declarations),
		(SELECT COUNT(*) FROM tool_uses)`).Scan(&decls, &uses)
	if err == sql.ErrNoRows {
		err = nil
	}
	return decls, uses, err
}
