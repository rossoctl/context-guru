package dash

// The decision half of the tool inventory: what an account could stop carrying, and what
// stopping has ACTUALLY saved so far.
//
// Two numbers live here and they must never be confused, which is why they are separate
// types with separate fields rather than one figure with a flag:
//
//	REALIZED   (DeclFilterSavings) — tokens the filter really did not send, priced at the
//	           tier each of those requests really was billed at. Sourced from
//	           requests.filtered_decl_tokens, which is written by the filter itself and by
//	           nothing else. A permanently-zero saving column is the failure mode this
//	           feature's coverage rules exist to prevent, so this column exists only because
//	           there is a filter populating it; where no account has opted in, it reads zero
//	           and that zero is the truth.
//	PROJECTED  (Suggestion.ProjectedUSD) — what removing an item WOULD save, extrapolated
//	           from what carrying it has cost. It is a forecast and is labelled as one
//	           everywhere it is rendered.
//
// # Sufficient observation
//
// Rule: a suggestion is offered only for an item that
//
//	(1) was declared in at least minSessions distinct sessions whose inventory WAS captured,
//	(2) whose first and last of those sessions are at least minSpan apart, and
//	(3) was invoked in NONE of them.
//
// Sessions with no captured inventory are not counted in (1) and not counted as unused
// either — "we have no rows for this session" is not evidence about what that session did
// not use, and letting it stand in for one is how absence of evidence becomes authorisation
// to remove something. It is the same rule the coverage block of the inventory report
// states, applied to the one place where a wrong answer changes what we send to the model.
//
// Why 5 sessions and 7 days rather than a round "enough": both halves target a specific way
// this can be wrong.
//
//   - The session count is about OPPORTUNITY. A measured claude-cli session with tools makes
//     ~65 requests, so five sessions is a few hundred turns in which the model chose some
//     other tool every time. One session proves nothing at all: a session that happened to
//     do no web work says nothing about WebSearch.
//   - The span is about VARIETY, and it is the half a session count alone cannot buy. Five
//     sessions in one afternoon are usually five sessions on one task, and the tool you did
//     not need this afternoon is exactly the tool you need on the next task type. A week
//     forces the sample across at least two working days.
//
// Both are parameters rather than constants because a suggestion's whole value is that a
// reader can see what it rests on: the API returns them beside the suggestions, and the
// Basis string says so in words.

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/skills"
)

// Sufficiency defaults. See the file comment for why these two numbers and not others.
const (
	suggestMinSessions = 5
	suggestMinSpan     = 7 * 24 * time.Hour
)

// Suggestion is one declaration an account could stop carrying, with the evidence behind
// the offer and the forecast of what it is worth.
type Suggestion struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// RemoveAs is the exact string to put in toolfilter's `remove` list, which is NOT always
	// Name: an MCP tool goes in as its full `mcp__server__tool` name (the whole server is
	// `mcp__server`, offered separately by the servers list) and a skill as
	// `skill__<name>`, because the two are removed by different mechanisms and a bare name
	// cannot say which is meant. See internal/skills.RemovePrefix.
	RemoveAs string `json:"remove_as"`
	// Tokens is what carrying this costs on ONE request.
	Tokens int `json:"tokens"`
	// Sessions is how many captured sessions declared it; Days is the span they cover.
	Sessions int     `json:"sessions"`
	Days     float64 `json:"days"`
	// Since is the first of those sessions, so the basis has a date and not just a count.
	Since int64 `json:"since"`
	// UnusedReads is the billed tokens already spent carrying it for nothing, and
	// ProjectedUSD their price at the tiers those requests paid. A FORECAST of what removal
	// is worth, stated as what the past would have cost without it — never a realized
	// saving, which only DeclFilterSavings reports.
	UnusedReads  int64   `json:"unused_reads"`
	ProjectedUSD float64 `json:"projected_usd"`
	Priced       bool    `json:"priced"`
	// Basis is the offer's justification in one sentence, so a reader never has to infer it
	// from the numbers beside it.
	Basis string `json:"basis"`
}

// DeclFilterSaving is the REALIZED accounting: what the filter actually stopped sending, at
// the tier actually billed.
//
// It is returned as a POINTER and OMITTED when nothing was measured, never rendered as a
// zero: the page that shows it prints "nothing realized" for an absent figure and would
// otherwise have to decide for itself whether a zero means "no saving" or "no data". An
// estimate in this field would silently become a lie on that page, so there is no path that
// puts one here — the only source is requests.filtered_decl_tokens.
type DeclFilterSaving struct {
	// Requests and Sessions are how much traffic the filter acted on.
	Requests int `json:"requests"`
	Sessions int `json:"sessions"`
	// Reads is the billed tokens avoided: the declarations' weight on every request that did
	// not carry them. Named for what it is on the wire — re-reads of a cached prefix are
	// where almost all of it lives.
	Reads int64 `json:"reads"`
	// Since is the first request the filter acted on in this scope, so the figure has a
	// start date rather than an implied "ever".
	Since int64 `json:"since"`
	// Tokens split by the tier the request carrying them was billed at. A prefix that was
	// read from cache would have re-read these at 0.1x; the turn that CREATED the prefix
	// would have written them at 1.25x; a request with no cache at all at 1.0x. Reported
	// split because one blended number cannot be checked.
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	FreshTokens      int64 `json:"fresh_tokens"`
	// USD is the sum over rows of tokens x that row's tier rate. Priced is false when any
	// contributing model had no known rates, in which case USD is the priced subset only
	// and must not be read as the total — a consumer renders the token count and an
	// "unpriced" mark rather than a dollar.
	USD    float64 `json:"usd"`
	Priced bool    `json:"priced"`
}

// ExcludedDecl is one declaration the account has opted out of, in the shape the inventory
// reports it (kind and name, so a page can match it against a row of GET /api/tools).
type ExcludedDecl struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// Since is when the filter first acted for this account, NOT a per-item audit date: the
	// configuration stores names, not timestamps, and the per-item history is in the audit
	// log where the change was recorded. Zero when the filter has not acted yet.
	Since int64 `json:"since,omitempty"`
}

// ToolFilterState is the caller's removal configuration, supplied by the host: dash holds no
// tenant configuration of its own, and inventing one here would be a second source of truth
// for what the proxy is actually sending.
type ToolFilterState struct {
	// Enabled is false when there is nothing for a switch to write to — no per-account
	// configuration on this deployment, a stored document that does not load, or toolfilter
	// absent from the effective pipeline. A consumer states Reason once and shows the
	// analysis without the control.
	Enabled bool
	Removed []string
	Reason  string
}

// DeclFilterSavings totals the realized saving over a scope.
//
// One row at a time rather than one SUM, for the same reason the inventory report is built
// in Go: each row's tokens are priced at ITS model's rates and ITS cache tier, and no
// aggregate can express that without either picking one model or blending rates that belong
// to none.
func (d *DB) DeclFilterSavings(f Filter, price func(string) (modelinfo.Price, bool)) (*DeclFilterSaving, error) {
	where, args := f.where()
	rows, err := d.sql.Query(`SELECT r.session_id, r.model, r.filtered_decl_tokens,
		r.cache_read, r.cache_write, r.ts FROM requests r
		WHERE `+where+` AND r.filtered_decl_tokens > 0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &DeclFilterSaving{Priced: true}
	sessions := map[string]bool{}
	for rows.Next() {
		var session, model string
		var tok, cacheRead, cacheWrite, ts int64
		if err := rows.Scan(&session, &model, &tok, &cacheRead, &cacheWrite, &ts); err != nil {
			return nil, err
		}
		out.Requests++
		sessions[session] = true
		out.Reads += tok
		if out.Since == 0 || ts < out.Since {
			out.Since = ts
		}
		var rate float64
		p, priced := modelinfo.Price{}, false
		if price != nil && model != "" {
			p, priced = price(model)
		}
		switch {
		case cacheRead > 0:
			// The prefix was served from cache on this request, so these declarations would
			// have been re-read at the cache-read rate. This is the overwhelmingly common case
			// and the cheapest tier, which is exactly why the figure must be computed this way
			// rather than at the fresh-input rate: doing the latter inflates it 10x.
			out.CacheReadTokens += tok
			rate = p.CacheRead
		case cacheWrite > 0:
			// No read, but a write: this request CREATED the prefix, so carrying them would
			// have cost the creation rate (1.25x fresh).
			out.CacheWriteTokens += tok
			rate = p.CacheWrite
		default:
			out.FreshTokens += tok
			rate = p.Input
		}
		if priced {
			out.USD += float64(tok) * rate
		} else {
			out.Priced = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Sessions = len(sessions)
	return out, nil
}

// declWindow is the observation window of one declared item: how many captured sessions
// declared it and when the first and last of them ran.
type declWindow struct {
	sessions    int
	first, last int64
}

// declWindows reads the per-item observation window for a scope. Separate from the report's
// own aggregation because the report answers "what does this cost" and this answers "how
// much do we actually know" — and only the second may authorise a removal.
func (d *DB) declWindows(f Filter) (map[statKey]declWindow, error) {
	where, args := f.where()
	q := `SELECT d.kind, d.name, COUNT(DISTINCT d.session_id), MIN(d.ts), MAX(d.ts)
		FROM tool_declarations d WHERE d.session_id IN
		  (SELECT r.session_id FROM requests r WHERE ` + where + ` AND r.tools > 0)`
	a := append([]any{}, args...)
	if !f.TenantAll {
		q += ` AND d.tenant_id = ?`
		a = append(a, f.Tenant)
	}
	q += ` GROUP BY 1, 2`
	rows, err := d.sql.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[statKey]declWindow{}
	for rows.Next() {
		var k statKey
		var w declWindow
		if err := rows.Scan(&k.kind, &k.name, &w.sessions, &w.first, &w.last); err != nil {
			return nil, err
		}
		out[k] = w
	}
	return out, rows.Err()
}

// ToolFilterDoc is the whole opt-in surface for one scope: what is currently excluded, what
// excluding it has ACTUALLY saved, and what is safe to offer next.
//
// Realized and Suggestions are separate fields and never merged, because they answer
// different questions and only one of them is a fact. See the file comment.
type ToolFilterDoc struct {
	// Enabled reports whether the removal list can be changed through this API. False is a
	// working state, not an error: the analysis below is still served, and Reason says why
	// the control is unavailable.
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
	// Excluded is what the account has opted out of. Never nil, so a consumer can count it
	// without a guard.
	Excluded []ExcludedDecl `json:"excluded"`
	// Realized is what the filter actually avoided, at the tier actually billed. OMITTED —
	// not zeroed — when nothing has been measured, so an absent figure reads as "nothing
	// realized" and can never be confused with the projection below.
	Realized *DeclFilterSaving `json:"realized,omitempty"`
	// Suggestions are candidates that pass the sufficiency rule, heaviest first. PROJECTIONS.
	Suggestions []Suggestion `json:"suggestions"`
	// MinSessions / MinDays are the sufficiency thresholds these suggestions were held to,
	// returned so the page can state them and so a reader can tell a changed threshold from
	// a changed corpus.
	MinSessions int     `json:"min_sessions"`
	MinDays     float64 `json:"min_days"`
	// Coverage is the inventory report's own honesty block, carried through unchanged: how
	// many sessions in scope could answer the question at all.
	Coverage ToolCoverage `json:"coverage"`
	// Withheld is how many never-used items did NOT reach the bar. Reported because a short
	// list of suggestions has two very different causes — nothing is wasted, or nothing is
	// yet known — and silence cannot distinguish them.
	Withheld int `json:"withheld"`
}

// ToolFilterDocFor builds the control document for one scope. state may be the zero value,
// which is a deployment with no per-account configuration: the analysis is served and the
// control is reported unavailable.
func (d *DB) ToolFilterDocFor(f Filter, price func(string) (modelinfo.Price, bool), state ToolFilterState) (*ToolFilterDoc, error) {
	rep, err := d.ToolReportFor(f, price)
	if err != nil {
		return nil, err
	}
	realized, err := d.DeclFilterSavings(f, price)
	if err != nil {
		return nil, err
	}
	win, err := d.declWindows(f)
	if err != nil {
		return nil, err
	}
	out := &ToolFilterDoc{
		Enabled:     state.Enabled,
		Reason:      state.Reason,
		Excluded:    excludedFrom(state.Removed, realized.Since),
		MinSessions: suggestMinSessions,
		MinDays:     suggestMinSpan.Hours() / 24,
		Coverage:    rep.Coverage,
		Suggestions: []Suggestion{},
	}
	// Nothing measured means the field is ABSENT, never a zero. This is the whole of the
	// realized-vs-projected discipline at the wire: a consumer that sees no realized figure
	// says so, and a consumer that borrows the projection to fill the gap cannot, because
	// there is no gap to fill.
	if realized.Requests > 0 {
		out.Realized = realized
	}
	// Skills ARE offered here now, alongside the tools. They used to be excluded because the
	// only removal mechanism was the `tools` array and a skill is not in it — that is no longer
	// true (apply.filterSkillListing cuts the listing entry), and the reason the exclusion gave
	// for itself turned out to argue the other way: the Skill tool's schema carries no enum, so a
	// model that names an unlisted skill anyway still RUNS it. That makes over-removing a skill
	// fail OPEN, where over-removing a tool fails silent. The safer of the two was the one being
	// withheld. See docs/how-to/declaration-removal.md.
	excluded := map[statKey]bool{}
	for _, e := range out.Excluded {
		excluded[statKey{e.Kind, e.Name}] = true
	}
	for _, st := range append(append([]ToolStat{}, rep.Tools...), rep.Skills.Skills...) {
		if excluded[statKey{st.Kind, st.Name}] {
			continue // already opted out: this is a realized saving, not a suggestion
		}
		if s, ok := suggest(st, win[statKey{st.Kind, st.Name}]); ok {
			out.Suggestions = append(out.Suggestions, s)
		} else if st.SessionsUsed == 0 && st.Tokens > 0 {
			out.Withheld++
		}
	}
	sort.Slice(out.Suggestions, func(i, j int) bool {
		if out.Suggestions[i].UnusedReads != out.Suggestions[j].UnusedReads {
			return out.Suggestions[i].UnusedReads > out.Suggestions[j].UnusedReads
		}
		return out.Suggestions[i].Name < out.Suggestions[j].Name
	})
	return out, nil
}

// excludedFrom classifies the configured names the way the inventory reports them, so a
// consumer can match an exclusion against an inventory row without re-deriving the kind.
// Sorted, because the configuration's order is not a fact about anything.
func excludedFrom(names []string, since int64) []ExcludedDecl {
	out := make([]ExcludedDecl, 0, len(names))
	for _, n := range names {
		e := ExcludedDecl{Kind: KindTool, Name: n, Since: since}
		if skill := strings.TrimPrefix(n, skills.RemovePrefix); skill != n {
			// A skill entry carries the prefix in the CONFIG and not in the report: the page
			// matches an exclusion against an inventory row by (kind, name), and a row's name is
			// the skill's own. Reporting the prefixed form here would leave every skill's switch
			// permanently off-looking-on — the write would land and the checkbox would not move.
			e.Kind, e.Name = KindSkill, skill
		} else if server, _, ok := SplitMCPName(n); ok {
			e.Kind, e.Server = KindMCPTool, server
		} else if strings.HasPrefix(n, "mcp__") {
			// The bare `mcp__<server>` form: a whole server, which is its own unit and not a
			// tool. Reported with the server name so a page can group it with that server.
			e.Kind, e.Server = "mcp_server", strings.TrimPrefix(n, "mcp__")
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// suggest applies the sufficiency rule to one item. The ONLY place the rule lives.
func suggest(st ToolStat, w declWindow) (Suggestion, bool) {
	// Server-side tools (web_search, code_execution, the mcp_toolset connector) are declared
	// by a `type` the provider resolves, not by a schema we can drop from the array without
	// changing what the request IS. Never offered. Nor is the skills LISTING: it is one
	// indivisible block of prose, and it shrinks by removing the skills inside it.
	if st.Kind == KindServerTool || st.Kind == KindSkillListing {
		return Suggestion{}, false
	}
	// A built-in is never offered either, and this is the second gate on that rather than the
	// first: buildToolReport keeps them out of rep.Tools, so nothing reaches here. It stays
	// because "suggest removing Read" is the single worst thing this API could say, and one
	// caller's filter is not where that belongs.
	if IsBuiltinTool(st.Kind, st.Name) {
		return Suggestion{}, false
	}
	// Positive, sufficient observation, and every clause is required.
	span := time.Duration(w.last-w.first) * time.Millisecond
	if st.SessionsUsed > 0 || st.Tokens <= 0 ||
		w.sessions < suggestMinSessions || span < suggestMinSpan {
		return Suggestion{}, false
	}
	days := span.Hours() / 24
	removeAs := st.Name
	if st.Kind == KindSkill {
		removeAs = skills.RemovePrefix + st.Name
	}
	return Suggestion{
		Kind: st.Kind, Name: st.Name, Server: st.Server, RemoveAs: removeAs,
		Tokens: st.Tokens, Sessions: w.sessions, Days: days, Since: w.first,
		UnusedReads: st.UnusedReads, ProjectedUSD: st.UnusedUSD, Priced: st.Priced,
		Basis: fmt.Sprintf("declared but never invoked across %d of your sessions since %s (%.0f days)",
			w.sessions, time.UnixMilli(w.first).UTC().Format("2006-01-02"), days),
	}, true
}

// SetToolFilterState supplies the caller's removal configuration. dash holds no tenant
// configuration of its own — the host owns it, validates it and audits changes to it — so
// this is a read hook rather than a store. Unset means "no per-account configuration on this
// deployment": the analysis is still served and the control reports itself unavailable.
//
// The WRITE path is deliberately not here. It is the compaction configuration, so it goes
// through the control plane's existing account-update path, where validation, the audit
// trail and manager gating already live; a second writer would be a second set of rules.
func (a *API) SetToolFilterState(fn func(tenantID string) ToolFilterState) { a.toolFilterFn = fn }

// toolFilterState resolves the caller's removal configuration, or the honest default.
func (a *API) toolFilterState(tenantID string) ToolFilterState {
	if a.toolFilterFn == nil {
		return ToolFilterState{Reason: "this proxy has no per-account configuration, so the " +
			"removal list is whatever its own config file sets"}
	}
	return a.toolFilterFn(tenantID)
}

// toolFilterStateForScope resolves the removal configuration for the scope being VIEWED, and
// refuses — with a reason a reader can act on — when the view spans more than one account.
//
// This is the read-side twin of the bug writeToolFilterDoc's comment records, and it shipped
// unfixed while the write side was patched. A MANAGER's default scope is the whole service, so
// f.Tenant is "" — and the registry lookup for tenant "" fails, which surfaced as
// `enabled:false, reason:"could not read your account"`. The effect on the live page: every
// opt-out switch on the Inventory tab rendered DISABLED for the one role permitted to use them,
// under a message saying something had gone wrong with their account. Nothing had; the request
// was ambiguous, and the removal list is a per-account setting with no service-wide meaning.
//
// Found by clicking the switch on a real hosted deployment. It is invisible from the tests
// because every fixture that exercises the control pins a tenant, and invisible from the page
// because a disabled switch under an explanatory paragraph looks like a deployment that has not
// enabled the feature.
func (a *API) toolFilterStateForScope(f Filter) ToolFilterState {
	if f.TenantAll || f.Tenant == "" {
		if a.auth == nil {
			// Single-tenant: TenantAll is the only scope there is, so this is not ambiguity — it
			// is a deployment with no per-account configuration to write to. Its own answer.
			return a.toolFilterState("")
		}
		return ToolFilterState{Reason: "you are viewing every account's traffic, and a removal " +
			"list belongs to ONE account — there is no service-wide list to switch. Pick an " +
			"account (or your own) in the account selector and the switches become live; the " +
			"analysis below is unaffected either way."}
	}
	return a.toolFilterState(f.Tenant)
}

// ToolFilterDocument builds the removal control document for a request's own scope. Exported
// because the control plane's write route answers with it too: a switch must repaint from the
// same document it would have read, and two builders would drift.
func (a *API) ToolFilterDocument(r *http.Request) (*ToolFilterDoc, error) {
	f, _, ok := a.scope(r)
	if !ok {
		return nil, errNotPermitted
	}
	return a.rec.DB().ToolFilterDocFor(f, a.priceFn(r), a.toolFilterStateForScope(f))
}

// errNotPermitted is returned to a caller that has no scope, so the control plane can tell
// "could not build the document" from "you may not see it".
var errNotPermitted = errors.New("not permitted")

// toolFilter serves the removal control document for the caller's scope.
func (a *API) toolFilterDoc(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	doc, err := a.rec.DB().ToolFilterDocFor(f, a.priceFn(r), a.toolFilterStateForScope(f))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not read the removal report")
		return
	}
	writeJSON(w, doc)
}
