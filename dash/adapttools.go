package dash

// GET /api/adapt/tools: the MCP servers, MCP tools and skills an account declared and never
// used, each priced at what carrying it cost on the requests that really carried it.
//
// It answers the same question as the Inventory tab's never-used rows, made exact for a removal
// decision. /api/tools prices a declaration over every request of a session that declared it,
// at the session's first model. A session holds several declaration SETS (a subagent with a
// restricted tool list, a server connected mid-session), and the sets share the session id, so
// that prices a server on requests that never carried it. Here every request is matched to the
// set it carried and priced at its own model and its own cache tier.
//
// Matching: tool_declarations stores each distinct set once per session under its digest, and
// a request records that digest in requests.decl_digest. Rows written before that column existed
// record only how many tools they declared, so they map to a set of their session with that tool
// count: the latest one first seen at or before the request, else the earliest. That guess is
// often ambiguous (a session's sets commonly share a tool count and differ by the skills listing
// or a tool's description), so every item also carries the lowest and highest figure any
// same-count choice would have given.

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/skills"
)

// KindMCPServer is a whole MCP server as one adapt item: every tool of it, none ever called.
const KindMCPServer = "mcp_server"

// adaptToolsOwn matches the plugin's own tools and skills and the proxy's injected stubs. A removal
// list must never contain them: removing the restore stub would strand every hidden tool.
var adaptToolsOwn = regexp.MustCompile(`(?i)context[-_]?guru`)

// adaptToolsSafe is what a name must look like to be printed in a suggested command.
var adaptToolsSafe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]+$`)

// AdaptToolsItem is one removable declaration with what carrying it cost.
type AdaptToolsItem struct {
	Kind   string `json:"kind"` // mcp_server | mcp_tool | skill
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// Tools is how many tools an mcp_server item declares (the most seen in one set).
	Tools int `json:"tools,omitempty"`
	// Tokens is the weight on ONE request that carried it (the largest seen).
	Tokens   int   `json:"tokens"`
	Sessions int   `json:"sessions"`
	Requests int   `json:"requests"`
	FirstTS  int64 `json:"first_ts"`
	LastTS   int64 `json:"last_ts"`
	// USD = USDCacheRead + USDCacheWrite + USDInput: the item's tokens at the tier each carrying
	// request was billed at. USDLow/USDHigh bound it over every same-count set an inferred
	// request could have carried; equal to USD when every request recorded its digest.
	USD           float64 `json:"usd"`
	USDLow        float64 `json:"usd_low"`
	USDHigh       float64 `json:"usd_high"`
	USDCacheRead  float64 `json:"usd_cache_read"`
	USDCacheWrite float64 `json:"usd_cache_write"`
	USDInput      float64 `json:"usd_input"`
	// Priced is false when some carrying request's model had no known rates.
	Priced bool `json:"priced"`
	// Sufficient: declared in at least suggestMinSessions sessions spanning suggestMinSpan, the
	// bar the declaration filter's own suggestions are held to.
	Sufficient bool   `json:"sufficient"`
	RemoveAs   string `json:"remove_as"` // the toolfilter `remove` entry
	Suggest    string `json:"suggest"`   // the client-side change that stops sending it
	// Remove is the same change for the shared /context-guru:adapt renderer, and LastUsedMS is
	// always null: every item here is unused in the window.
	Remove     string `json:"remove"`
	LastUsedMS *int64 `json:"last_used_ms"`
	// Basis states what "unused" rests on: the observed window, never a promise about later.
	Basis string `json:"basis"`
}

// AdaptToolsOption is one choice the report offers; Apply names it by ID.
type AdaptToolsOption struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Detail   string  `json:"detail,omitempty"`
	SavedUSD float64 `json:"saved_usd"`
	SpendUSD float64 `json:"spend_usd"`
	NetUSD   float64 `json:"net_usd"`
	// Remove is the toolfilter entries the option adds (or, for "restore", takes out).
	Remove []string `json:"remove"`
}

// AdaptToolsRecommendation is the one change the report proposes.
type AdaptToolsRecommendation struct {
	Option      string  `json:"option"`
	Change      string  `json:"change"`
	NetDeltaUSD float64 `json:"net_delta_usd"`
	PerMonthUSD float64 `json:"per_month_usd"`
	Effective   string  `json:"effective"`
	Reason      string  `json:"reason"`
}

// AdaptToolsWindow is the span the report covers.
type AdaptToolsWindow struct {
	Since    int64 `json:"since"`
	Until    int64 `json:"until"`
	Sessions int   `json:"sessions"`
	Requests int   `json:"requests"`
}

// AdaptToolsDoc is the /api/adapt/tools response.
type AdaptToolsDoc struct {
	Kind           string                    `json:"kind"`
	Version        int                       `json:"version"`
	Summary        string                    `json:"summary"`
	Window         AdaptToolsWindow          `json:"window"`
	Class          string                    `json:"class"`
	Method         string                    `json:"method"`
	MethodDetail   string                    `json:"method_detail"`
	BasisID        string                    `json:"basis_id"` // set by the handler: the offer id apply must send
	Current        string                    `json:"current"`
	Default        string                    `json:"default"`
	Best           string                    `json:"best"`
	Options        []AdaptToolsOption        `json:"options"`
	Recommendation *AdaptToolsRecommendation `json:"recommendation"`
	Warnings       []string                  `json:"warnings"`
	Coverage       AdaptToolsCoverage        `json:"coverage"`
	// RewriteUSD is the one-off cost of removing anything: the next request re-writes the
	// cached prefix instead of reading it. Priced on the latest priced request in scope (its
	// whole prompt, at write minus read), which is the most it can cost a session.
	RewriteUSD float64 `json:"rewrite_usd"`
	rewriteTS  int64
	Items      []AdaptToolsItem `json:"items"`
	TotalUSD   float64          `json:"total_usd"`
	TotalLow   float64          `json:"total_usd_low"`
	TotalHi    float64          `json:"total_usd_high"`
	Excluded   []string         `json:"excluded"`
	Filter     AdaptToolsFilter `json:"filter"`
}

// AdaptToolsCoverage says how much of the scope could answer the question.
type AdaptToolsCoverage struct {
	Sessions         int `json:"sessions"`           // sessions with a tools>0 request
	SessionsCaptured int `json:"sessions_captured"`  // ... of which have a stored inventory
	Requests         int `json:"requests"`           // tools>0 requests
	RequestsMapped   int `json:"requests_mapped"`    // ... matched to a declaration set
	RequestsExact    int `json:"requests_exact"`     // ... of which by their recorded digest
	RequestsGuessed  int `json:"requests_ambiguous"` // ... of which had 2+ same-count candidate sets
	RequestsUnpriced int `json:"requests_unpriced"`  // mapped, but incomplete token accounting
}

// AdaptToolsFilter is the caller's current toolfilter removal list.
type AdaptToolsFilter struct {
	Enabled bool     `json:"enabled"`
	Removed []string `json:"removed"`
	Reason  string   `json:"reason,omitempty"`
}

const adaptToolsMethod = "Each request is matched to the declaration set it carried and the item's tokens " +
	"are priced at that request's model and tier: cache read when the prefix was read, cache " +
	"write when it was rewritten, input when uncached. Tokens are the proxy's own count, so the " +
	"figures are a lower bound; keep-alive pings are not counted."

type adaptToolsReq struct {
	ts                    int64
	model, accounting     string
	digest                string
	tools                 int
	cacheRead, cacheWrite int64
	cacheWrite1h, fresh   int64
}

type adaptToolsSet struct {
	first  int64
	ntools int
	carry  map[string]int // item key -> tokens in this set
	mcpN   map[string]int // server -> tools of it in this set
}

// AdaptTools builds the document for one scope.
func (d *DB) AdaptTools(f Filter, price func(string) (modelinfo.Price, bool), state ToolFilterState) (*AdaptToolsDoc, error) {
	where, args := f.where()
	out := &AdaptToolsDoc{
		Kind: "tools", Version: 1, Class: "counterfactual",
		Method: "unused declarations priced on the requests that carried them", MethodDetail: adaptToolsMethod,
		Window: AdaptToolsWindow{Since: f.Since, Until: f.Until}, Warnings: []string{},
		Items: []AdaptToolsItem{},
		Excluded: []string{"Context Guru's own tools and skills", "built-in and other non-MCP tools",
			"server tools", "anything already in the removal list"},
		Filter: AdaptToolsFilter{Enabled: state.Enabled, Removed: append([]string{}, state.Removed...), Reason: state.Reason},
	}

	reqs := map[string][]adaptToolsReq{} // tenant\x00session -> requests, oldest first
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.tenant_id, r.session_id, r.ts, r.model,
		r.token_accounting, r.decl_digest, r.tools, r.cache_read, r.cache_write, r.cache_write_1h, r.fresh_input
		FROM requests r WHERE `+where+` AND r.tools > 0 ORDER BY r.ts`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, s string
		var q adaptToolsReq
		if err := rows.Scan(&t, &s, &q.ts, &q.model, &q.accounting, &q.digest, &q.tools, &q.cacheRead, &q.cacheWrite, &q.cacheWrite1h, &q.fresh); err != nil {
			rows.Close()
			return nil, err
		}
		reqs[t+"\x00"+s] = append(reqs[t+"\x00"+s], q)
		out.Coverage.Requests++
		if out.Window.Since == 0 || q.ts < out.Window.Since {
			out.Window.Since = q.ts
		}
		if q.ts > out.Window.Until {
			out.Window.Until = q.ts
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Coverage.Sessions = len(reqs)
	if f.Since > 0 {
		out.Window.Since = f.Since
	}
	if f.Until > 0 {
		out.Window.Until = f.Until
	}
	out.Window.Sessions, out.Window.Requests = len(reqs), out.Coverage.Requests
	if len(reqs) == 0 {
		out.finish(state)
		return out, nil
	}

	uses, err := d.scopedUses(f, where, args)
	if err != nil {
		return nil, err
	}
	usedTool, usedServer, usedSkill := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, u := range uses {
		usedTool[u.name] = true
		if srv, _, ok := SplitMCPName(u.name); ok {
			usedServer[srv] = true
		}
		if u.skill != "" {
			usedSkill[u.skill] = true
		}
	}
	removed := map[string]bool{}
	for _, n := range state.Removed {
		removed[n] = true
	}

	// Declarations: one set per (tenant, session, digest), read one session at a time so only that
	// session's sets are in memory. Only the candidate items are kept.
	q := `SELECT d.tenant_id, d.session_id, d.digest, d.kind, d.name, d.server, d.tokens, d.ts
		FROM tool_declarations d WHERE d.kind IN ('tool', 'mcp_tool', 'server_tool', 'skill')`
	var da []any
	if !f.TenantAll {
		q += ` AND d.tenant_id = ?`
		da = append(da, f.Tenant)
	}
	drows, err := d.sql.QueryContext(d.readCtx(), q+` ORDER BY d.tenant_id, d.session_id`, da...)
	if err != nil {
		return nil, err
	}
	items := map[string]*AdaptToolsItem{}
	// priceSession prices one session's requests against its sets.
	priceSession := func(list []adaptToolsReq, sess map[string]*adaptToolsSet) {
		out.Coverage.SessionsCaptured++
		seen := map[string]bool{}
		for _, st := range sess {
			for key := range st.carry {
				if !seen[key] {
					seen[key] = true
					items[key].Sessions++
				}
			}
		}
		byCount := map[int][]*adaptToolsSet{}
		for _, st := range sess {
			byCount[st.ntools] = append(byCount[st.ntools], st)
		}
		for _, c := range byCount {
			sort.Slice(c, func(i, j int) bool { return c[i].first < c[j].first })
		}
		for _, r := range list {
			st, cands := sess[r.digest], []*adaptToolsSet(nil)
			if st != nil {
				out.Coverage.RequestsExact++
			} else {
				cands = byCount[r.tools]
				st = adaptToolsPick(cands, r.ts)
				if len(cands) > 1 {
					out.Coverage.RequestsGuessed++
				}
			}
			if st == nil {
				continue
			}
			out.Coverage.RequestsMapped++
			if r.accounting != AccountingComplete {
				out.Coverage.RequestsUnpriced++
				continue
			}
			p, ok := modelinfo.Price{}, false
			if price != nil && r.model != "" {
				p, ok = price(r.model)
			}
			if ok && r.ts >= out.rewriteTS {
				out.rewriteTS = r.ts
				out.RewriteUSD = float64(r.cacheRead+r.cacheWrite+r.fresh) * (p.CacheWrite - p.CacheRead)
			}
			rate := p.Input
			switch {
			case r.cacheRead > 0:
				rate = p.CacheRead
			case r.cacheWrite > 0:
				// A one-hour breakpoint precedes every 5-minute one, so when the request wrote any
				// 1h tokens the declarations at the head were among them: 2x input, not 1.25x.
				if r.cacheWrite1h > 0 {
					rate = 2 * p.Input
				}
			}
			for key, tok := range st.carry {
				it := items[key]
				it.Requests++
				if !ok {
					it.Priced = false
					continue
				}
				x := float64(tok) * rate
				switch {
				case r.cacheRead > 0:
					it.USDCacheRead += x
				case r.cacheWrite > 0:
					it.USDCacheWrite += x
				default:
					it.USDInput += x
				}
			}
			if !ok {
				continue
			}
			// The bounds: per item, the least and most it weighs in any candidate set. An item
			// missing from one candidate weighs 0 there.
			if len(cands) < 2 {
				for key, tok := range st.carry {
					items[key].USDLow += float64(tok) * rate
					items[key].USDHigh += float64(tok) * rate
				}
				continue
			}
			lo, hi := map[string]int{}, map[string]int{}
			for _, c := range cands {
				for key, tok := range c.carry {
					if tok > hi[key] {
						hi[key] = tok
					}
				}
			}
			for key := range hi {
				lo[key] = hi[key]
				for _, c := range cands {
					if c.carry[key] < lo[key] {
						lo[key] = c.carry[key]
					}
				}
				items[key].USDLow += float64(lo[key]) * rate
				items[key].USDHigh += float64(hi[key]) * rate
			}
		}
		// Per-request weight and tool count are per set, so take the heaviest set.
		for _, st := range sess {
			for key, tok := range st.carry {
				it := items[key]
				if tok > it.Tokens {
					it.Tokens = tok
				}
				if n := st.mcpN[it.Name]; it.Kind == KindMCPServer && n > it.Tools {
					it.Tools = n
				}
			}
		}
	}
	cur, sess := "", map[string]*adaptToolsSet{}
	for drows.Next() {
		var t, s, dg, kind, name, server string
		var tok int
		var ts int64
		if err := drows.Scan(&t, &s, &dg, &kind, &name, &server, &tok, &ts); err != nil {
			drows.Close()
			return nil, err
		}
		sk := t + "\x00" + s
		if reqs[sk] == nil {
			continue // not in scope
		}
		if sk != cur {
			if cur != "" {
				priceSession(reqs[cur], sess)
			}
			cur, sess = sk, map[string]*adaptToolsSet{}
		}
		st := sess[dg]
		if st == nil {
			st = &adaptToolsSet{first: ts, carry: map[string]int{}, mcpN: map[string]int{}}
			sess[dg] = st
		}
		if ts < st.first {
			st.first = ts
		}
		if kind != KindSkill {
			st.ntools++
		}
		key, it := adaptToolsCandidate(kind, name, server, usedTool, usedServer, usedSkill, removed)
		if it == nil {
			continue
		}
		st.carry[key] += tok
		if it.Kind == KindMCPServer {
			st.mcpN[it.Name]++
		}
		ex := items[key]
		if ex == nil {
			ex = it
			ex.Priced, ex.FirstTS = true, ts
			items[key] = ex
		}
		if ts < ex.FirstTS {
			ex.FirstTS = ts
		}
		if ts > ex.LastTS {
			ex.LastTS = ts
		}
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return nil, err
	}
	if cur != "" {
		priceSession(reqs[cur], sess)
	}

	for _, it := range items {
		it.USD = it.USDCacheRead + it.USDCacheWrite + it.USDInput
		it.Sufficient = it.Sessions >= suggestMinSessions &&
			time.Duration(it.LastTS-it.FirstTS)*time.Millisecond >= suggestMinSpan
		out.TotalUSD += it.USD
		out.TotalLow += it.USDLow
		out.TotalHi += it.USDHigh
		out.Items = append(out.Items, *it)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].USD != out.Items[j].USD {
			return out.Items[i].USD > out.Items[j].USD
		}
		return out.Items[i].Name < out.Items[j].Name
	})
	out.finish(state)
	return out, nil
}

// finish fills the shared /context-guru:adapt envelope from the items: the options, the one
// recommended change, the basis id an apply must match, and the caveats.
func (out *AdaptToolsDoc) finish(state ToolFilterState) {
	var all, enough []string
	var allUSD, enoughUSD float64
	from := time.UnixMilli(out.Window.Since).UTC().Format("2006-01-02")
	to := time.UnixMilli(out.Window.Until).UTC().Format("2006-01-02")
	for i := range out.Items {
		it := &out.Items[i]
		it.Remove = it.Suggest
		it.Basis = fmt.Sprintf("declared in %d sessions, unused in the observed window %s to %s", it.Sessions, from, to)
		all, allUSD = append(all, it.RemoveAs), allUSD+it.USD
		if it.Sufficient {
			enough, enoughUSD = append(enough, it.RemoveAs), enoughUSD+it.USD
		}
	}
	out.Current, out.Default, out.Best = "keep", "keep", "keep"
	out.Options = []AdaptToolsOption{{ID: "keep", Label: "Keep everything", Remove: []string{}}}
	if len(enough) > 0 {
		out.Options = append(out.Options, AdaptToolsOption{ID: "remove-sufficient",
			Label:  fmt.Sprintf("Filter the %d unused items seen in %d+ sessions over %d+ days", len(enough), suggestMinSessions, int(suggestMinSpan.Hours()/24)),
			Detail: strings.Join(enough, ", "), SavedUSD: enoughUSD, SpendUSD: out.RewriteUSD,
			NetUSD: enoughUSD - out.RewriteUSD, Remove: enough})
	}
	if len(all) > len(enough) {
		out.Options = append(out.Options, AdaptToolsOption{ID: "remove-unused",
			Label:  fmt.Sprintf("Filter all %d unused items", len(all)),
			Detail: strings.Join(all, ", "), SavedUSD: allUSD, SpendUSD: out.RewriteUSD,
			NetUSD: allUSD - out.RewriteUSD, Remove: all})
	}
	var restore []string
	for _, n := range state.Removed {
		if strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, skills.RemovePrefix) {
			restore = append(restore, n)
		}
	}
	if len(restore) > 0 {
		out.Options = append(out.Options, AdaptToolsOption{ID: "restore",
			Label:  fmt.Sprintf("Stop filtering the %d MCP and skill entries already filtered", len(restore)),
			Detail: strings.Join(restore, ", "), Remove: restore})
	}
	// Only items that meet the filter's own evidence bar are recommended: "unused in this
	// window" is hindsight, and an item seen in a handful of sessions says little about the next.
	days := float64(out.Window.Until-out.Window.Since) / float64(24*time.Hour/time.Millisecond)
	switch {
	case len(all) == 0:
		out.Summary = "Every MCP server, MCP tool and skill you declared was used in this window."
	case len(enough) > 0 && enoughUSD <= out.RewriteUSD:
		out.Summary = fmt.Sprintf("%d declared items were unused from %s to %s, but removing them would have saved %s, less than the %s one-off prefix rewrite it causes.",
			len(all), from, to, usdText(enoughUSD), usdText(out.RewriteUSD))
	case len(enough) == 0:
		out.Summary = fmt.Sprintf("%d declared items were unused from %s to %s (%s), but none was seen in enough sessions to recommend removing it yet.",
			len(all), from, to, usdText(allUSD))
	default:
		out.Best = "remove-sufficient"
		out.Summary = fmt.Sprintf("%d declared items were unused from %s to %s; removing the %d with enough evidence would have saved %s, %s after the one-off prefix rewrite.",
			len(all), from, to, len(enough), usdText(enoughUSD), usdText(enoughUSD-out.RewriteUSD))
		rec := &AdaptToolsRecommendation{Option: "remove-sufficient", NetDeltaUSD: enoughUSD - out.RewriteUSD,
			Change:    "filter " + strings.Join(head(enough, 3), ", "),
			Effective: "now",
			Reason:    fmt.Sprintf("declared in %d+ sessions over %d+ days and never called", suggestMinSessions, int(suggestMinSpan.Hours()/24))}
		if len(enough) > 3 {
			rec.Change += fmt.Sprintf(" and %d more", len(enough)-3)
		}
		if days >= 1 {
			rec.PerMonthUSD = enoughUSD / days * 30 // steady state: the rewrite is paid once
		}
		out.Recommendation = rec
	}
	c := out.Coverage
	if n := c.Sessions - c.SessionsCaptured; n > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d of %d sessions have no stored inventory and are not counted.", n, c.Sessions))
	}
	if c.RequestsGuessed > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d of %d requests have no recorded declaration set and more than one set it could have been; the total could be %s to %s.",
			c.RequestsGuessed, c.RequestsMapped, usdText(out.TotalLow), usdText(out.TotalHi)))
	}
	if n := c.Requests - c.RequestsMapped; n > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d requests match no stored declaration set and are not counted, in the range either.", n))
	}
	if c.RequestsUnpriced > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d requests with incomplete token accounting are not priced.", c.RequestsUnpriced))
	}
	if len(all) > 0 {
		out.Warnings = append(out.Warnings, "Unused means unused in this window, not that it will never be needed.",
			fmt.Sprintf("Removing changes the cached prefix, so the next request re-writes it once: about %s, counted as the option's spend.", usdText(out.RewriteUSD)))
	}
}

func usdText(x float64) string {
	if x > 0 && x < 0.005 {
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", x)
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// adaptToolsPick is the set a request at ts carried: the latest first seen at or before it, else the
// earliest. c is sorted by first.
func adaptToolsPick(c []*adaptToolsSet, ts int64) *adaptToolsSet {
	if len(c) == 0 {
		return nil
	}
	best := c[0]
	for _, st := range c[1:] {
		if st.first <= ts {
			best = st
		}
	}
	return best
}

// adaptToolsCandidate classifies one declaration row: its item key and a fresh item, or nil when it
// is not removable or was used.
func adaptToolsCandidate(kind, name, server string, usedTool, usedServer, usedSkill, removed map[string]bool) (string, *AdaptToolsItem) {
	if adaptToolsOwn.MatchString(name) || adaptToolsOwn.MatchString(server) {
		return "", nil
	}
	// Names are client-supplied and are printed inside commands to copy: anything that is not a
	// plain identifier is not offered at all.
	if !adaptToolsSafe.MatchString(name) || (server != "" && !adaptToolsSafe.MatchString(server)) {
		return "", nil
	}
	switch kind {
	case KindMCPTool:
		if usedTool[name] || removed[name] || removed["mcp__"+server] {
			return "", nil
		}
		if !usedServer[server] {
			return KindMCPServer + "\x00" + server, &AdaptToolsItem{Kind: KindMCPServer, Name: server,
				Server: server, RemoveAs: "mcp__" + server, Suggest: suggestServer(server)}
		}
		return kind + "\x00" + name, &AdaptToolsItem{Kind: kind, Name: name, Server: server, RemoveAs: name,
			Suggest: `add "` + name + `" to permissions.deny in your Claude Code settings`}
	case KindSkill:
		if usedSkill[name] || removed[skills.RemovePrefix+name] {
			return "", nil
		}
		s := `set "skillOverrides": {"` + name + `": "off"} in your Claude Code settings`
		// skillOverrides does not reach a plugin's skills (checked on Claude Code 2.1.296), so a
		// plugin skill goes with its whole plugin, or through the proxy filter alone.
		if plugin, _, ok := strings.Cut(name, ":"); ok {
			s = "claude plugin disable " + plugin + " (removes every skill of the plugin; to drop only this one, use the proxy filter)"
		}
		return kind + "\x00" + name, &AdaptToolsItem{Kind: kind, Name: name, RemoveAs: skills.RemovePrefix + name, Suggest: s}
	}
	return "", nil
}

// suggestServer is the client-side command that stops a server being declared. The server half
// of a tool name says where it came from: a plugin, a claude.ai connector, or the user's config.
func suggestServer(server string) string {
	switch {
	case strings.HasPrefix(server, "plugin_"):
		plugin, _, _ := strings.Cut(strings.TrimPrefix(server, "plugin_"), "_")
		return "claude plugin disable " + plugin + " (the plugin that provides this server)"
	case strings.HasPrefix(server, "claude_ai_"):
		return "disconnect the " + strings.TrimPrefix(server, "claude_ai_") + " connector in claude.ai settings"
	}
	return "claude mcp remove " + server
}

// adaptTools serves GET /api/adapt/tools for the caller's scope.
//
// A removal list belongs to one account, so the report defaults to the caller's own traffic even
// for a manager (whose default dashboard scope is the whole service); ?tenant= still widens it.
func (a *API) adaptTools(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("tenant") == "" {
		q := r.URL.Query()
		q.Set("tenant", "me")
		r = r.Clone(r.Context())
		r.URL.RawQuery = q.Encode()
	}
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	// One running report per account and a few an hour: each reads the account's whole history.
	done, ok := a.adaptReportSlot("tools|" + f.Tenant)
	if !ok {
		httpErr(w, http.StatusTooManyRequests, "a report is already running for this account, or "+
			"too many were asked for in the last hour; wait and try again")
		return
	}
	defer done()
	doc, err := a.db(r).AdaptTools(f, a.priceFn(r), a.toolFilterStateForScope(f))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The options are frozen as offered: apply takes one of exactly these, from this report,
	// for AdaptOfferTTL (the shared adapt helper's rule).
	opts := make(map[string]any, len(doc.Options))
	for _, o := range doc.Options {
		opts[o.ID] = o.Remove
	}
	doc.BasisID = a.OfferAdapt("tools", f.Tenant, opts)
	writeJSON(w, doc)
}
