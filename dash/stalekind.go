package dash

// Stale declaration kinds (#817).
//
// An older proxy filed every OpenAI function tool as a `server_tool`, and every tool of a
// Responses body under one name, "function". The same tool is now a `tool` or `mcp_tool`. The
// primary key of tool_declarations includes the kind, so a session whose set was stored before
// the upgrade and again after it holds BOTH rows, and every reader would count the tool twice.
//
// staleRules is the ONE rule that drops the old row. Every reader of the table applies it to the
// facts it builds, whatever its path (the per-session query, the whole-table pass, the prompt
// view, the carried-cost read, SelfRemovals). It is not a SQL clause: a copy in some queries and
// not in others made the narrow and the broad report disagree on the same data.
//
// The rule, per (tenant, session): the NEWEST kind wins.
//   - A `server_tool` fact is stale when a `tool` or `mcp_tool` fact of the same name is newer.
//   - A `server_tool` fact named "function" (the Responses phantom) is stale when any `tool` or
//     `mcp_tool` fact of the session is newer.
//
// Nothing is deleted. A session that never re-sends its tools after the upgrade keeps its old
// kind: no newer fact exists to win.
//
// Limit: the rule can hide a GENUINE `server_tool` fact whose name equals the name of a later
// client tool of the same session (for example `web_search` declared first by the provider and
// later as a client tool). That direction is an undercount, and it is rare.
type staleRules struct {
	sess map[ruleKey]*ruleSessionFacts
}

type ruleKey struct{ tenant, session string }

type ruleSessionFacts struct {
	byName map[string]int64 // newest tool / mcp_tool fact, per name
	any    int64            // newest tool / mcp_tool fact of the session
}

func newStaleRules() *staleRules { return &staleRules{sess: map[ruleKey]*ruleSessionFacts{}} }

// observe records one fact. Only the kinds that can supersede a server_tool matter.
func (r *staleRules) observe(tenant, session, kind, name string, ts int64) {
	if kind != KindTool && kind != KindMCPTool {
		return
	}
	k := ruleKey{tenant, session}
	s := r.sess[k]
	if s == nil {
		s = &ruleSessionFacts{byName: map[string]int64{}}
		r.sess[k] = s
	}
	if ts > s.byName[name] {
		s.byName[name] = ts
	}
	if ts > s.any {
		s.any = ts
	}
}

// merge folds another rule set in (a reader that sees a session in several slices).
func (r *staleRules) merge(o *staleRules) {
	for k, s := range o.sess {
		for name, ts := range s.byName {
			r.observe(k.tenant, k.session, KindTool, name, ts)
		}
		if d := r.sess[k]; d != nil && s.any > d.any {
			d.any = s.any
		}
	}
}

// stale reports whether the fact is an old server_tool that a newer fact replaced.
func (r *staleRules) stale(tenant, session, kind, name string, ts int64) bool {
	if kind != KindServerTool {
		return false
	}
	s := r.sess[ruleKey{tenant, session}]
	if s == nil {
		return false
	}
	return s.byName[name] > ts || (name == "function" && s.any > ts)
}

// dropStale returns rows without its stale facts. at reads the fact fields of one row; ts is the
// newest time the fact was seen. The input slice is reused.
func dropStale[T any](rows []T, at func(*T) (tenant, session, kind, name string, ts int64)) []T {
	r := newStaleRules()
	for i := range rows {
		t, s, k, n, ts := at(&rows[i])
		r.observe(t, s, k, n, ts)
	}
	out := rows[:0]
	for i := range rows {
		t, s, k, n, ts := at(&rows[i])
		if !r.stale(t, s, k, n, ts) {
			out = append(out, rows[i])
		}
	}
	return out
}
