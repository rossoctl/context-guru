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

// adaptOwn matches the plugin's own tools and skills and the proxy's injected stubs. A removal
// list must never contain them: removing the restore stub would strand every hidden tool.
var adaptOwn = regexp.MustCompile(`(?i)context[-_]?guru`)

// AdaptItem is one removable declaration with what carrying it cost.
type AdaptItem struct {
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
}

// AdaptToolsDoc is the /api/adapt/tools response.
type AdaptToolsDoc struct {
	Version  int           `json:"version"`
	Window   Window        `json:"window"`
	Coverage AdaptCoverage `json:"coverage"`
	Basis    string        `json:"basis"`
	Items    []AdaptItem   `json:"items"`
	TotalUSD float64       `json:"total_usd"`
	TotalLow float64       `json:"total_usd_low"`
	TotalHi  float64       `json:"total_usd_high"`
	Excluded []string      `json:"excluded"`
	Filter   AdaptFilter   `json:"filter"`
}

// AdaptCoverage says how much of the scope could answer the question.
type AdaptCoverage struct {
	Sessions         int `json:"sessions"`          // sessions with a tools>0 request
	SessionsCaptured int `json:"sessions_captured"` // ... of which have a stored inventory
	Requests         int `json:"requests"`          // tools>0 requests
	RequestsMapped   int `json:"requests_mapped"`   // ... matched to a declaration set
	RequestsExact    int `json:"requests_exact"`    // ... of which by their recorded digest
	RequestsUnpriced int `json:"requests_unpriced"` // mapped, but incomplete token accounting
}

// AdaptFilter is the caller's current toolfilter removal list.
type AdaptFilter struct {
	Enabled bool     `json:"enabled"`
	Removed []string `json:"removed"`
	Reason  string   `json:"reason,omitempty"`
}

const adaptBasis = "Each request is matched to the declaration set it carried and the item's tokens " +
	"are priced at that request's model and tier: cache read when the prefix was read, cache " +
	"write when it was rewritten, input when uncached. Tokens are the proxy's own count, so the " +
	"figures are a lower bound; keep-alive pings are not counted."

type adaptReq struct {
	ts                    int64
	model, accounting     string
	digest                string
	tools                 int
	cacheRead, cacheWrite int64
}

type adaptSet struct {
	first  int64
	ntools int
	carry  map[string]int // item key -> tokens in this set
	mcpN   map[string]int // server -> tools of it in this set
}

// AdaptTools builds the document for one scope.
func (d *DB) AdaptTools(f Filter, price func(string) (modelinfo.Price, bool), state ToolFilterState) (*AdaptToolsDoc, error) {
	where, args := f.where()
	out := &AdaptToolsDoc{
		Version: 1, Window: Window{Since: f.Since, Until: f.Until}, Basis: adaptBasis,
		Items: []AdaptItem{},
		Excluded: []string{"Context Guru's own tools and skills", "built-in and other non-MCP tools",
			"server tools", "anything already in the removal list"},
		Filter: AdaptFilter{Enabled: state.Enabled, Removed: append([]string{}, state.Removed...), Reason: state.Reason},
	}

	reqs := map[string][]adaptReq{} // tenant\x00session -> requests, oldest first
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.tenant_id, r.session_id, r.ts, r.model,
		r.token_accounting, r.decl_digest, r.tools, r.cache_read, r.cache_write
		FROM requests r WHERE `+where+` AND r.tools > 0 ORDER BY r.ts`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, s string
		var q adaptReq
		if err := rows.Scan(&t, &s, &q.ts, &q.model, &q.accounting, &q.digest, &q.tools, &q.cacheRead, &q.cacheWrite); err != nil {
			rows.Close()
			return nil, err
		}
		reqs[t+"\x00"+s] = append(reqs[t+"\x00"+s], q)
		out.Coverage.Requests++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.Coverage.Sessions = len(reqs)
	if len(reqs) == 0 {
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
	items := map[string]*AdaptItem{}
	// priceSession prices one session's requests against its sets.
	priceSession := func(list []adaptReq, sess map[string]*adaptSet) {
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
		byCount := map[int][]*adaptSet{}
		for _, st := range sess {
			byCount[st.ntools] = append(byCount[st.ntools], st)
		}
		for _, c := range byCount {
			sort.Slice(c, func(i, j int) bool { return c[i].first < c[j].first })
		}
		for _, r := range list {
			st, cands := sess[r.digest], []*adaptSet(nil)
			if st != nil {
				out.Coverage.RequestsExact++
			} else {
				cands = byCount[r.tools]
				st = pickSet(cands, r.ts)
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
			rate := p.Input
			switch {
			case r.cacheRead > 0:
				rate = p.CacheRead
			case r.cacheWrite > 0:
				rate = p.CacheWrite
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
	cur, sess := "", map[string]*adaptSet{}
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
			cur, sess = sk, map[string]*adaptSet{}
		}
		st := sess[dg]
		if st == nil {
			st = &adaptSet{first: ts, carry: map[string]int{}, mcpN: map[string]int{}}
			sess[dg] = st
		}
		if ts < st.first {
			st.first = ts
		}
		if kind != KindSkill {
			st.ntools++
		}
		key, it := adaptCandidate(kind, name, server, usedTool, usedServer, usedSkill, removed)
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
	return out, nil
}

// pickSet is the set a request at ts carried: the latest first seen at or before it, else the
// earliest. c is sorted by first.
func pickSet(c []*adaptSet, ts int64) *adaptSet {
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

// adaptCandidate classifies one declaration row: its item key and a fresh item, or nil when it
// is not removable or was used.
func adaptCandidate(kind, name, server string, usedTool, usedServer, usedSkill, removed map[string]bool) (string, *AdaptItem) {
	if adaptOwn.MatchString(name) || adaptOwn.MatchString(server) {
		return "", nil
	}
	switch kind {
	case KindMCPTool:
		if usedTool[name] || removed[name] || removed["mcp__"+server] {
			return "", nil
		}
		if !usedServer[server] {
			return KindMCPServer + "\x00" + server, &AdaptItem{Kind: KindMCPServer, Name: server,
				Server: server, RemoveAs: "mcp__" + server, Suggest: suggestServer(server)}
		}
		return kind + "\x00" + name, &AdaptItem{Kind: kind, Name: name, Server: server, RemoveAs: name,
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
		return kind + "\x00" + name, &AdaptItem{Kind: kind, Name: name, RemoveAs: skills.RemovePrefix + name, Suggest: s}
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
func (a *API) adaptTools(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	doc, err := a.db(r).AdaptTools(f, a.priceFn(r), a.toolFilterStateForScope(f))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, doc)
}
