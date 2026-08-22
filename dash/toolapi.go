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
	"log/slog"
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
	// Builtin marks one of Claude Code's OWN tools. Derived on read from a name allowlist
	// (see IsBuiltinTool), because the stored taxonomy cannot tell them from a user's own
	// client-side tools — both are KindTool. It exists so the UI can put them in their own
	// section, behind an expand, with a warning: they are the one group here that must NOT
	// look like a tidy saving, because removing one breaks the agent rather than slimming it.
	Builtin bool `json:"builtin"`
	// SharePct is this declaration's share of everything the account declares per request —
	// the "who owns the system prompt" number, which is the question the page is really
	// answering and which no column carried.
	SharePct float64 `json:"share_pct"`
	// Removal is how to stop carrying it, in the form the user can act on. See RemovalFor.
	Removal Removal `json:"removal"`
}

// ModelRate is one model this scope actually ran on, with the two rates that decide what
// removing a declaration is worth.
//
// It is a LIST because the answer genuinely differs per model and the page was collapsing
// them: the same 4,000-token declaration is worth an order of magnitude more on an expensive
// model than a cheap one, and an account running both has no single answer. FirstRequestUSD
// per token is the cache-WRITE rate (the tier a declaration enters the prompt at); the
// per-session figure is dominated by the cache-READ rate, because that is what every later
// turn of the session pays to carry it again.
type ModelRate struct {
	Model string `json:"model"`
	// Requests and Sessions are the weight behind this model, so a rate that applies to four
	// requests is not read as though it applied to the whole scope.
	Requests int `json:"requests"`
	Sessions int `json:"sessions"`
	// CacheWriteUSDPerToken / CacheReadUSDPerToken are the two rates, per single token.
	CacheWriteUSDPerToken float64 `json:"cache_write_usd_per_token"`
	CacheReadUSDPerToken  float64 `json:"cache_read_usd_per_token"`
	Priced                bool    `json:"priced"`
}

// SelfRemoval is a capability that STOPPED being declared partway through the window — the
// user acted on it.
//
// This is a saving the product caused and it was being thrown away. Once somebody removes an
// MCP server, the declaration is simply absent from later sessions: no component ran, no
// filter fired, and every measurement on this dashboard is about content we removed, so the
// reduction registered nowhere. Crediting it needs no new capture, only the observation that
// the inventory is a time series and this name is present in the early part and absent from
// the late part.
//
// It is kept SEPARATE from the tool-filter's realized savings and never added to them, because
// the two can describe the same tokens: an account that removed a server locally AND has it in
// its server-side filter list would otherwise be credited twice for one reduction. Overlap is
// reported rather than resolved silently — see Overlap.
type SelfRemoval struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// Tokens is what it weighed on each request that still carried it.
	Tokens int `json:"tokens"`
	// LastSeen / SessionsBefore / SessionsAfter are the evidence. SessionsAfter is how many
	// sessions ran after the last one that declared it — the claim is only as strong as that
	// number, and one session proves nothing, so the UI shows it beside every row.
	LastSeen       int64 `json:"last_seen"`
	SessionsBefore int   `json:"sessions_before"`
	SessionsAfter  int   `json:"sessions_after"`
	// AvoidedReads is Tokens x the requests in the later sessions that would have re-read it.
	AvoidedReads int64   `json:"avoided_reads"`
	AvoidedUSD   float64 `json:"avoided_usd"`
	Priced       bool    `json:"priced"`
	// Overlap is true when this name is ALSO on the account's server-side filter list, so the
	// same reduction may already be counted as a realized filter saving. Reported, not netted.
	Overlap bool `json:"overlap"`
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
	// RequestsPerSession is a MEAN over every TOOL-BEARING session in scope, captured or not,
	// while Median and Typical below are over the CAPTURED ones only — a different and smaller
	// population. That is deliberate (this one answers "how many requests does a session make",
	// those answer "how long is a session we can price"), but it means the three must never be
	// printed together without naming which population each describes: on real traffic they read
	// 3.9, 1 and 150.6. Every UI site that quotes one says so.
	RequestsPerSession       float64 `json:"requests_per_session"`
	RequestsPerSessionMedian int     `json:"requests_per_session_median"`
	// RequestsPerSessionTypical is the REQUEST-WEIGHTED mean: how many turns the session that
	// a typical request belongs to actually runs for. This is the multiplier a "cost across a
	// full session" projection must use — see the comment where it is computed for why the
	// mean and the median both answer a different question and both understate it badly.
	RequestsPerSessionTypical float64 `json:"requests_per_session_typical"`
	// DeclaredSetTokens is the summed weight of everything declared, once — the whole that
	// every SharePct is a part of. DeclaredTokens above is a per-session MEAN and is a
	// different quantity; the two were conflated and produced shares over 100%.
	DeclaredSetTokens int `json:"declared_set_tokens"`
}

// ToolReport is the whole answer for one scope.
type ToolReport struct {
	Coverage ToolCoverage `json:"coverage"`
	Totals   ToolTotals   `json:"totals"`
	Tools    []ToolStat   `json:"tools"`
	Servers  []ServerStat `json:"servers"`
	Skills   SkillStat    `json:"skills"`
	// Models is every model this scope ran on, with its cache-write and cache-read rates, so
	// "what would removing this save me" can be answered per model instead of at one blended
	// rate that describes none of them.
	Models []ModelRate `json:"models"`
	// SelfRemoved is what the account stopped declaring partway through the window — savings
	// the user made themselves, which no other measurement on this dashboard could see.
	SelfRemoved []SelfRemoval `json:"self_removed,omitempty"`
}

// sessionLengths is the per-session request count over the CAPTURED sessions only, which is
// the population every other figure on this report is averaged over.
func sessionLengths(sessions map[string]*sessionCost, captured map[string]bool) []int {
	out := make([]int, 0, len(captured))
	for id := range captured {
		if sc := sessions[id]; sc != nil {
			if n := sc.requests(); n > 0 {
				out = append(out, n)
			}
		}
	}
	return out
}

// modelRates collapses the per-session costs into one row per model.
func modelRates(sessions map[string]*sessionCost) []ModelRate {
	by := map[string]*ModelRate{}
	for _, sc := range sessions {
		if sc.model == "" {
			continue
		}
		m := by[sc.model]
		if m == nil {
			m = &ModelRate{Model: sc.model}
			by[sc.model] = m
		}
		m.Sessions++
		m.Requests += sc.requests()
		if sc.priced && !m.Priced {
			m.CacheWriteUSDPerToken, m.CacheReadUSDPerToken = sc.price.CacheWrite, sc.price.CacheRead
			m.Priced = true
		}
	}
	out := make([]ModelRate, 0, len(by))
	for _, m := range by {
		out = append(out, *m)
	}
	return out
}

// sortModelRates puts the model that carried the most requests first: that is the one an
// account's numbers are mostly about, and a rate list ordered by name buries it.
func sortModelRates(m []ModelRate) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Requests != m[j].Requests {
			return m[i].Requests > m[j].Requests
		}
		return m[i].Model < m[j].Model
	})
}

// sessionCost is one session's re-read multiplier, split by the tier each request was
// billed at, plus the price of the model that billed it.
type sessionCost struct {
	reads, writes, fresh int
	price                modelinfo.Price
	priced               bool
	// model is the first model seen for the session, matching the rule the price follows:
	// a session that used two models keeps the first, because a blended rate over one token
	// count belongs to no model. Recorded so the report can group rates BY model.
	model string
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
		if sc.model == "" {
			sc.model = model
		}
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
	// The MEDIAN session length, over the sessions that actually carried an inventory.
	//
	// The mean above is dragged toward 1 by short sidechain sessions (a title generation, a
	// single tool call), so quoting it as "an average session" understates what carrying a
	// declaration for a whole session costs. The median is what "a typical session" means, and
	// a per-session projection has to say WHICH it used — so both are reported and the UI names
	// the one it multiplied by.
	if rep.Coverage.Sessions > 0 {
		rep.Totals.RequestsPerSession = float64(rep.Coverage.Requests) / float64(rep.Coverage.Sessions)
	}
	if lens := sessionLengths(sessions, captured); len(lens) > 0 {
		sort.Ints(lens)
		rep.Totals.RequestsPerSessionMedian = lens[len(lens)/2]
		// The REQUEST-WEIGHTED mean session length, and it is the one a per-session projection
		// must use.
		//
		// Neither of the two obvious statistics answers the question. On real traffic most
		// sessions are one-request sidechains (a title generation, a single tool call), so the
		// median session is 1 request and the plain mean is under 4 — and quoting either as
		// "an average session" would say that carrying a declaration costs about one re-read,
		// when the sessions where the money actually goes run to dozens of turns.
		//
		// The question is not "how long is a session" but "how long is the session that a
		// given REQUEST belongs to", because that is where the repeated re-reads are. That is
		// sum(n^2)/sum(n): each session weighted by how many requests it contributes. It is
		// the same correction as the classic class-size paradox, and it is why this number is
		// an order of magnitude above the median rather than a bug.
		var n, nsq int64
		for _, v := range lens {
			n += int64(v)
			nsq += int64(v) * int64(v)
		}
		if n > 0 {
			rep.Totals.RequestsPerSessionTypical = float64(nsq) / float64(n)
		}
	}
	// The models this scope ran on, each with the two rates that decide what a removal is
	// worth. A list, because the answer differs per model by an order of magnitude and the
	// report was collapsing them into one number that belonged to no model.
	rep.Models = modelRates(sessions)

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

	sortModelRates(rep.Models)
	rep.Servers = serverRollup(rep.Tools)
	sortStats(rep.Tools)
	sortStats(rep.Skills.Skills)
	// Classification, share and removal advice, applied to every row once the list is final.
	// On READ rather than at capture: the built-in/user-added split is a name allowlist that
	// will gain entries as Claude Code does, and re-capturing history to learn a new name is
	// not a thing anyone can do. See IsBuiltinTool.
	// The share denominator is the sum of every declaration's own weight, INCLUDING the skills
	// listing — i.e. a real whole that the parts add up to.
	//
	// It is deliberately not Totals.DeclaredTokens, which is a mean over captured sessions and
	// was the first thing tried: sessions here range from a 2-tool sidechain to a full
	// inventory, so the mean is far smaller than a single large declaration and shares came out
	// at 650%. A percentage over 100 is not a rounding problem, it is proof the denominator is
	// not the whole the numerator is part of.
	whole := rep.Skills.ListingTokens
	for _, t := range rep.Tools {
		whole += t.Tokens
	}
	annotate(rep.Tools, whole)
	annotate(rep.Skills.Skills, whole)
	rep.Totals.DeclaredSetTokens = whole
	return rep
}

// annotate fills the three read-time fields on a row: whether it is a built-in, its share of
// the declared prompt, and how to stop carrying it.
//
// declared is the summed weight of the whole declared set. Zero leaves SharePct at 0 rather
// than dividing — a share of an unknown whole is not 100%.
func annotate(rows []ToolStat, declared int) {
	for i := range rows {
		t := &rows[i]
		t.Builtin = IsBuiltinTool(t.Kind, t.Name)
		if declared > 0 {
			t.SharePct = 100 * float64(t.Tokens) / float64(declared)
		}
		t.Removal = RemovalFor(t.Kind, t.Name, t.Server)
	}
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
	price := a.priceFn(r)
	rep, err := a.rec.DB().ToolReportFor(f, price)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not read the tool inventory")
		return
	}
	// Credit for what the USER removed themselves. Best-effort and non-fatal: it is an
	// addition to the report, so a deployment where it fails still gets the inventory rather
	// than an error page. Needs a pricer to put a dollar on, and the token counts stand
	// without one.
	if price != nil {
		// The account's own server-side removal list, so a reduction that the tool filter is
		// ALREADY credited for can be marked as overlapping instead of counted twice.
		filtered := map[string]bool{}
		for _, n := range a.toolFilterState(f.Tenant).Removed {
			filtered[n] = true
		}
		sr, err := a.rec.DB().SelfRemovals(f, price, filtered)
		if err != nil {
			// Non-fatal, but never silent: swallowing this returned an empty list that was
			// indistinguishable from "the account removed nothing", which is a claim.
			slog.Warn("dash: self-removal credit unavailable", "err", err)
		}
		rep.SelfRemoved = sr
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

// SelfRemovals finds declarations the account STOPPED carrying partway through the window.
//
// This is the one saving this dashboard causes and could not see. Every other measurement here
// is about content a component removed; when a user reads this page, runs `claude mcp remove`
// and stops carrying 4,000 tokens on every request, no component ran and no filter fired, so
// the reduction landed in no figure anywhere. The product got no credit for the outcome it most
// wants to cause.
//
// The method needs no new capture, only the observation that tool_declarations is a TIME SERIES
// keyed by session: a name that appears in the early sessions of a window and in none of the
// late ones stopped being declared. What makes that a claim rather than a guess is the number of
// sessions that ran AFTERWARDS without it — one proves nothing (a session may simply not have
// needed it), a dozen is strong. That count is returned on every row and the UI shows it, rather
// than a threshold being applied here and the weak rows silently dropped.
//
// Deliberately NOT netted into the declaration filter's realized savings: an account that both
// removed a server locally and has it on its server-side filter list would otherwise be credited
// twice for one reduction. Rows in that position are marked Overlap and left for the reader.
func (d *DB) SelfRemovals(f Filter, price func(string) (modelinfo.Price, bool), filtered map[string]bool) ([]SelfRemoval, error) {
	where, args := f.where()
	// Session ordering comes from the requests table (the declarations carry a ts, but one per
	// digest, not per session start), so "before" and "after" mean the same thing here as
	// everywhere else on the page.
	q := `WITH sess AS (
			SELECT r.session_id AS sid, MIN(r.ts) AS started, COUNT(*) AS reqs,
				SUM(CASE WHEN r.cache_read > 0 THEN 1 ELSE 0 END) AS reads,
				SUM(CASE WHEN r.cache_read = 0 AND r.cache_write > 0 THEN 1 ELSE 0 END) AS writes,
				SUM(CASE WHEN r.cache_read = 0 AND r.cache_write = 0 THEN 1 ELSE 0 END) AS fresh,
				MIN(r.model) AS model
			FROM requests r WHERE ` + where + ` AND r.tools > 0 GROUP BY 1)
		SELECT d.kind, d.name, d.server, MAX(d.tokens), MAX(s.started),
			COUNT(DISTINCT d.session_id)
		FROM tool_declarations d JOIN sess s ON s.sid = d.session_id`
	a := append([]any{}, args...)
	if !f.TenantAll {
		q += ` WHERE d.tenant_id = ?`
		a = append(a, f.Tenant)
	}
	q += ` GROUP BY d.kind, d.name, d.server`
	rows, err := d.sql.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cand struct {
		kind, name, server string
		tokens             int
		lastSeen           int64
		before             int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.kind, &c.name, &c.server, &c.tokens, &c.lastSeen, &c.before); err != nil {
			return nil, err
		}
		// ONLY MCP tools and skills are eligible, and that is a correctness restriction rather
		// than a scoping choice.
		//
		// The first version of this considered every declaration and confidently reported that
		// the account had "removed" Bash, Agent, TodoWrite and Monitor — because sessions
		// legitimately declare DIFFERENT inventories. A sidechain request (a title generation, a
		// single tool call) declares two tools; a working session declares forty. So a name
		// being absent from a later session is not evidence that anything was removed, and over
		// a corpus that is mostly short sessions it is evidence of nothing at all.
		//
		// MCP tools and skills are the items the question is actually about — they are what a
		// user adds and drops — and they come with a usable control: a later session either
		// carries MCP tools at all, or carries a skills listing at all, and only such a session
		// can testify about a missing one. See the cohort test below, which is the other half of
		// this fix; neither half works alone.
		if c.kind != KindMCPTool && c.kind != KindSkill {
			continue
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The sessions that ran after each candidate was last declared, and what they would have
	// paid to keep carrying it. One pass over the session list rather than a query per
	// candidate: the list is a few thousand rows and there can be hundreds of candidates.
	// Each later session's cohort: whether it declared ANY MCP tool, and whether it carried a
	// skills listing. This is what makes a later session able to testify. A session that
	// declared no MCP tools cannot be evidence that one particular MCP tool was removed — it is
	// evidence that this session was not an MCP session.
	type sessRow struct {
		started              int64
		reads, writes, fresh int
		model                string
		hasMCP, hasSkills    bool
	}
	// The cohort flags come from a SEPARATE query, not a join onto the request aggregate.
	//
	// Joining tool_declarations onto requests multiplies every request row by the number of
	// declarations in its session, so the SUMs below counted each request dozens of times and
	// the avoided-cost figures came out at $456 for a 142-token skill — larger than this
	// deployment's entire measured savings. A fanned-out join under an aggregate is silent and
	// the result is merely implausible rather than an error, which is exactly why the sanity
	// check ("is this number bigger than the whole bill?") is worth doing on any new figure.
	crows, err := d.sql.Query(`SELECT d.session_id,
			MAX(CASE WHEN d.kind = '`+KindMCPTool+`' THEN 1 ELSE 0 END),
			MAX(CASE WHEN d.kind IN ('`+KindSkill+`','`+KindSkillListing+`') THEN 1 ELSE 0 END)
		FROM tool_declarations d
		WHERE d.session_id IN (SELECT r.session_id FROM requests r WHERE `+where+` AND r.tools > 0)
		GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	cohortBySession := map[string]struct{ mcp, skills bool }{}
	for crows.Next() {
		var sid string
		var mcp, sk int
		if err := crows.Scan(&sid, &mcp, &sk); err != nil {
			crows.Close()
			return nil, err
		}
		cohortBySession[sid] = struct{ mcp, skills bool }{mcp == 1, sk == 1}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}
	srows, err := d.sql.Query(`SELECT r.session_id, MIN(r.ts),
			SUM(CASE WHEN r.cache_read > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN r.cache_read = 0 AND r.cache_write > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN r.cache_read = 0 AND r.cache_write = 0 THEN 1 ELSE 0 END),
			MIN(r.model)
		FROM requests r WHERE `+where+` AND r.tools > 0 GROUP BY r.session_id`, args...)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	var sess []sessRow
	for srows.Next() {
		var s sessRow
		var sid string
		if err := srows.Scan(&sid, &s.started, &s.reads, &s.writes, &s.fresh, &s.model); err != nil {
			return nil, err
		}
		c := cohortBySession[sid]
		s.hasMCP, s.hasSkills = c.mcp, c.skills
		sess = append(sess, s)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	var out []SelfRemoval
	for _, c := range cands {
		r := SelfRemoval{
			Kind: c.kind, Name: c.name, Server: c.server,
			Tokens: c.tokens, LastSeen: c.lastSeen, SessionsBefore: c.before,
			Overlap: filtered[c.name] || (c.server != "" && filtered[c.server]),
		}
		priced := true
		for _, s := range sess {
			if s.started <= c.lastSeen {
				continue // ran before or alongside the last sighting: proves nothing
			}
			// The cohort test. Only a session of the same KIND can testify: one that carries MCP
			// tools can say a particular MCP tool is gone, and one that carries a skills listing
			// can say a particular skill is gone. Without this the count is dominated by short
			// sessions that never declared anything of the sort.
			if c.kind == KindMCPTool && !s.hasMCP {
				continue
			}
			if c.kind == KindSkill && !s.hasSkills {
				continue
			}
			r.SessionsAfter++
			n := int64(s.reads + s.writes + s.fresh)
			r.AvoidedReads += int64(c.tokens) * n
			p, ok := price(s.model)
			if !ok || p.Zero() {
				priced = false
				continue
			}
			// Each avoided re-read valued at the tier that request actually paid — the same
			// rule the rest of this file prices by, so the two figures are comparable.
			r.AvoidedUSD += float64(c.tokens) * (float64(s.reads)*p.CacheRead +
				float64(s.writes)*p.CacheWrite + float64(s.fresh)*p.Input)
		}
		// Fewer than three qualifying later sessions is not a claim worth making. One or two
		// comparable sessions without an item is as easily a session that did not load it as a
		// removal, and a row that says "removed!" on that basis is the same mistake as the
		// built-ins version of this analysis. The UI still marks anything under a dozen as weak.
		if r.SessionsAfter < 3 {
			continue
		}
		r.Priced = priced
		out = append(out, r)
	}
	// Biggest claim first, and the UI shows the evidence beside each one.
	sort.Slice(out, func(i, j int) bool { return out[i].AvoidedReads > out[j].AvoidedReads })
	if len(out) > 25 {
		out = out[:25]
	}
	return out, nil
}
