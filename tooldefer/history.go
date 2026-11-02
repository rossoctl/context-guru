package tooldefer

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/internal/deferred"
	"github.com/tidwall/gjson"
)

// Tool history: per user (tenant), session and tool, whether the session declared the tool and
// whether it used it. Metadata only: tool names, session ids, project paths, times. No tool text.
//
// It lives in its OWN table, not in the dashboard's tool_declarations / tool_uses, for two reasons:
//   - the dashboard deletes rows after its retention (7 days by default), so the history would
//     reset every week, and a user who works once a week would never reach 3 sessions;
//   - the request path must never wait on a query: the whole index is in memory (Mem), loaded once
//     in the background, and written back in the background.
//
// Bounded by COUNT, not by time: the newest MaxSessions sessions per tenant are kept, the rest
// are deleted. The rule reads at most ProjectSessions (10) sessions of one project, so 64 per
// tenant covers several projects.

// MaxSessions is how many sessions per tenant the history keeps.
const MaxSessions = 64

// maxToolsPerSession bounds one session's tool set: names come off the request body.
const maxToolsPerSession = 512

// histSession is one session's entry in memory.
type histSession struct {
	id       string
	project  string
	last     int64
	declared map[string]bool
	used     map[string]bool
	dirty    bool
}

// Mem is the in-memory history. Safe for concurrent use. The zero value is not usable; use NewMem.
type Mem struct {
	mu      sync.Mutex
	tenants map[string][]*histSession // newest first
	ready   map[string]bool           // tenants loaded from the database (or known empty)
	db      *sql.DB
	loaded  bool
	now     func() time.Time
}

// NewMem returns an empty history. db may be nil: the history then lives in memory only, and a
// restart starts it from zero (so nothing is deferred until 3 new sessions).
func NewMem(db *sql.DB) *Mem {
	return &Mem{tenants: map[string][]*histSession{}, ready: map[string]bool{}, db: db, now: time.Now}
}

// Schema creates the history table. Called by Open.
const schema = `CREATE TABLE IF NOT EXISTS tool_defer_history (
  tenant_id  TEXT    NOT NULL DEFAULT '',
  session_id TEXT    NOT NULL DEFAULT '',
  project    TEXT    NOT NULL DEFAULT '',
  tool       TEXT    NOT NULL DEFAULT '',
  declared   INTEGER NOT NULL DEFAULT 0,
  used       INTEGER NOT NULL DEFAULT 0,
  last_ts    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, session_id, tool)
)`

// probeSchema keeps probe verdicts across a restart: a thread decided before it needs one.
const probeSchema = `CREATE TABLE IF NOT EXISTS tool_defer_probe (
  upstream TEXT NOT NULL DEFAULT '',
  model    TEXT NOT NULL DEFAULT '',
  route    TEXT NOT NULL DEFAULT '',
  verdict  INTEGER NOT NULL DEFAULT 0,
  ts       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (upstream, model, route)
)`

// SaveVerdict keeps one probe verdict (Unknown deletes it). Synchronous and short: it runs on a
// probe's background goroutine, never on the request path.
func (m *Mem) SaveVerdict(k ProbeKey, v Verdict, at time.Time) {
	if m == nil || m.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx, probeSchema); err != nil {
		return
	}
	var err error
	if v == Unknown {
		_, err = m.db.ExecContext(ctx, `DELETE FROM tool_defer_probe WHERE upstream = ? AND model = ? AND route = ?`,
			k.Upstream, k.Model, k.Route)
	} else {
		_, err = m.db.ExecContext(ctx, `INSERT INTO tool_defer_probe(upstream, model, route, verdict, ts) VALUES (?,?,?,?,?)
			ON CONFLICT(upstream, model, route) DO UPDATE SET verdict = excluded.verdict, ts = excluded.ts`,
			k.Upstream, k.Model, k.Route, int(v), at.UnixMilli())
	}
	if err != nil {
		slog.Debug("tooldefer: probe verdict not kept", "err", err)
	}
}

// LoadVerdicts seeds p with every kept verdict. Called once at start, off the request path.
func (m *Mem) LoadVerdicts(p *Prober) {
	if m == nil || m.db == nil || p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx, probeSchema); err != nil {
		return
	}
	rows, err := m.db.QueryContext(ctx, `SELECT upstream, model, route, verdict, ts FROM tool_defer_probe`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k ProbeKey
		var v int
		var ts int64
		if rows.Scan(&k.Upstream, &k.Model, &k.Route, &v, &ts) == nil {
			p.Seed(k, Verdict(v), time.UnixMilli(ts))
		}
	}
}

// Open makes the table, loads the whole history into memory and starts the background writer.
// It returns at once; the load runs in the background, and until it finishes Ready reports false
// (the rule then defers nothing). stop ends the writer after a last flush.
//
// p, when not nil, is seeded with the kept probe verdicts first, in the same background load.
// Known window: a request that arrives in the first moments after a start, before the load ends,
// reads no verdict and no history. A NEW thread then decides "nothing" (safe), and a thread whose
// record was lost is sent undeferred once. The disk store keeps the record itself, so that second
// case needs a restart with the session-state database off.
func (m *Mem) Open(flushEvery time.Duration, p *Prober) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	if m.db == nil {
		m.mu.Lock()
		m.loaded = true
		m.mu.Unlock()
		close(finished)
		return func() {}
	}
	if flushEvery <= 0 {
		flushEvery = 5 * time.Second
	}
	go func() {
		defer close(finished)
		m.LoadVerdicts(p)
		if err := m.load(); err != nil {
			slog.Warn("tooldefer: history not loaded; it starts empty", "err", err)
		}
		m.mu.Lock()
		m.loaded = true
		m.mu.Unlock()
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				m.flush()
				return
			case <-t.C:
				m.flush()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); <-finished }) }
}

// Ready reports whether the history is loaded. Before that, the rule must not decide: an empty
// history read as "no sessions" is right (defer nothing), but the set it stores would then be
// "nothing" for that thread forever. Deciding "nothing" is the safe direction, so callers may
// still decide; Ready exists for tests and metrics.
func (m *Mem) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded
}

func (m *Mem) load() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT tenant_id, session_id, project, tool, declared, used, last_ts
		FROM tool_defer_history`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byKey := map[[2]string]*histSession{}
	for rows.Next() {
		var tenant, sid, project, tool string
		var decl, used int
		var last int64
		if err := rows.Scan(&tenant, &sid, &project, &tool, &decl, &used, &last); err != nil {
			return err
		}
		k := [2]string{tenant, sid}
		s := byKey[k]
		if s == nil {
			s = &histSession{id: sid, project: project, declared: map[string]bool{}, used: map[string]bool{}}
			byKey[k] = s
		}
		if last > s.last {
			s.last = last
		}
		if decl != 0 {
			s.declared[tool] = true
		}
		if used != 0 {
			s.used[tool] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(byKey) == 0 {
		// A new table: seed it once from the dashboard's own inventory tables, so a user who picks
		// ultrahigh has their recent sessions at once rather than after 3 more. Best effort: an
		// older dashboard database without these tables simply seeds nothing.
		if err := seed(ctx, m.db, byKey); err != nil {
			slog.Debug("tooldefer: history not seeded from the dashboard", "err", err)
		}
		for _, s := range byKey {
			s.dirty = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range byKey {
		// A session seen live before the load finished wins: it is newer.
		if m.find(k[0], s.id) != nil {
			continue
		}
		m.tenants[k[0]] = append(m.tenants[k[0]], s)
	}
	for t := range m.tenants {
		m.sortLocked(t)
	}
	return nil
}

// seed reads the newest MaxSessions sessions per tenant from tool_declarations and tool_uses.
// Kinds tool and mcp_tool only: a skill is not a tool this package defers.
func seed(ctx context.Context, db *sql.DB, byKey map[[2]string]*histSession) error {
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, session_id, name, MAX(ts) FROM tool_declarations
		WHERE kind IN ('tool','mcp_tool') AND name <> '' GROUP BY tenant_id, session_id, name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tenant, sid, name string
		var ts int64
		if err := rows.Scan(&tenant, &sid, &name, &ts); err != nil {
			rows.Close()
			return err
		}
		k := [2]string{tenant, sid}
		s := byKey[k]
		if s == nil {
			s = &histSession{id: sid, declared: map[string]bool{}, used: map[string]bool{}}
			byKey[k] = s
		}
		s.declared[name] = true
		if ts > s.last {
			s.last = ts
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	urows, err := db.QueryContext(ctx, `SELECT tenant_id, session_id, project, name FROM tool_uses WHERE name <> ''`)
	if err != nil {
		return err
	}
	defer urows.Close()
	for urows.Next() {
		var tenant, sid, project, name string
		if err := urows.Scan(&tenant, &sid, &project, &name); err != nil {
			return err
		}
		if s := byKey[[2]string{tenant, sid}]; s != nil {
			s.used[name] = true
			if s.project == "" {
				s.project = NormProject(project)
			}
		}
	}
	return urows.Err()
}

func (m *Mem) find(tenant, sid string) *histSession {
	for _, s := range m.tenants[tenant] {
		if s.id == sid {
			return s
		}
	}
	return nil
}

func (m *Mem) sortLocked(tenant string) {
	ss := m.tenants[tenant]
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].last > ss[j].last })
	if len(ss) > MaxSessions {
		ss = ss[:MaxSessions]
	}
	m.tenants[tenant] = ss
}

// Observe records one request: the tools it declares (from the ORIGINAL body, Claude Code's own
// deferred list included) and the tools its recent turns used or loaded through search. tenant
// may be "" (single-tenant). It never blocks on the database.
func (m *Mem) Observe(tenant, session, project string, body []byte, ts int64) {
	if m == nil || session == "" {
		return
	}
	declared := DeclaredNames(body)
	if len(declared) == 0 {
		return // an auxiliary request with no tools: not evidence about any tool
	}
	used := recentUses(body)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.find(tenant, session)
	if s == nil {
		s = &histSession{id: session, project: project, declared: map[string]bool{}, used: map[string]bool{}}
		m.tenants[tenant] = append([]*histSession{s}, m.tenants[tenant]...)
		// First sight in this process: read the whole transcript once, so a session resumed after
		// a restart keeps the uses it made before.
		for n := range usedInTranscript(body) {
			used[n] = true
		}
	}
	if s.project == "" {
		s.project = project
	}
	for n := range declared {
		if !s.declared[n] && len(s.declared) < maxToolsPerSession {
			s.declared[n], s.dirty = true, true
		}
	}
	for n := range used {
		if n != "" && !s.used[n] && len(s.used) < maxToolsPerSession {
			s.used[n], s.dirty = true, true
		}
	}
	if ts > s.last {
		s.last, s.dirty = ts, true
	}
	m.sortLocked(tenant)
}

// UserSessions is History.UserSessions.
func (m *Mem) UserSessions(tenant, exclude string, n int) []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Session
	for _, s := range m.tenants[tenant] {
		if s.id == exclude {
			continue
		}
		// Copies, built under the lock: Decide reads them after it returns, while Observe may be
		// writing the live maps for another request. Live maps here crashed the process
		// (concurrent map read and map write), which no recover catches.
		out = append(out, Session{Declared: copySet(s.declared), Used: copySet(s.used)})
		if len(out) == n {
			break
		}
	}
	return out
}

// ProjectUsed is History.ProjectUsed.
func (m *Mem) ProjectUsed(tenant, project, tool, exclude string, n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := 0
	for _, s := range m.tenants[tenant] {
		if s.id == exclude || s.project != project {
			continue
		}
		if inSet(s.used, tool) {
			return true
		}
		if seen++; seen == n {
			break
		}
	}
	return false
}

// flush writes the changed sessions and deletes the ones that fell out of the newest MaxSessions.
func (m *Mem) flush() {
	type row struct {
		tenant, sid, project, tool string
		decl, used                 int
		last                       int64
	}
	m.mu.Lock()
	var rows []row
	keep := map[string][]string{}
	for tenant, ss := range m.tenants {
		for _, s := range ss {
			keep[tenant] = append(keep[tenant], s.id)
			if !s.dirty {
				continue
			}
			s.dirty = false
			names := map[string]bool{}
			for n := range s.declared {
				names[n] = true
			}
			for n := range s.used {
				names[n] = true
			}
			for n := range names {
				rows = append(rows, row{tenant, s.id, s.project, n, b2i(s.declared[n]), b2i(s.used[n]), s.last})
			}
		}
	}
	m.mu.Unlock()
	if len(rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Debug("tooldefer: history flush", "err", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO tool_defer_history(tenant_id, session_id, project, tool, declared, used, last_ts)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(tenant_id, session_id, tool) DO UPDATE SET
		project = excluded.project, declared = MAX(declared, excluded.declared), used = MAX(used, excluded.used),
		last_ts = MAX(last_ts, excluded.last_ts)`)
	if err != nil {
		return
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.tenant, r.sid, r.project, r.tool, r.decl, r.used, r.last); err != nil {
			slog.Debug("tooldefer: history flush", "err", err)
			return
		}
	}
	for tenant, ids := range keep {
		if len(ids) < MaxSessions {
			continue
		}
		args := []any{tenant}
		for _, id := range ids {
			args = append(args, id)
		}
		q := `DELETE FROM tool_defer_history WHERE tenant_id = ? AND session_id NOT IN (?` +
			strings.Repeat(",?", len(ids)-1) + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Debug("tooldefer: history flush", "err", err)
	}
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DeclaredNames is every client tool the request declares: the `tools` array (a tool the client
// deferred included, Claude Code's placeholder and our own tools left out) and the names on
// Claude Code's deferred-tools list.
func DeclaredNames(body []byte) map[string]bool {
	out := map[string]bool{}
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		if n == "" || t.Get("type").Exists() || n == placeholder || n == SearchName ||
			strings.HasPrefix(n, "context_guru_") || len(n) > deferred.MaxNameLen {
			return true
		}
		out[n] = true
		return true
	})
	if strings.Contains(string(body), deferred.Header) {
		for _, text := range deferred.Texts(body) {
			lists, _ := deferred.Lists(text)
			for _, l := range lists {
				for _, n := range l.Names {
					out[n] = true
				}
			}
		}
	}
	return out
}

// recentUses reads the uses of the last few messages only: the agent resends the whole transcript
// on every request, and the new turn is at its end.
func recentUses(body []byte) map[string]bool {
	msgs := gjson.GetBytes(body, "messages")
	n := int(msgs.Get("#").Int())
	from := n - 4
	if from < 0 {
		from = 0
	}
	out := map[string]bool{}
	for i := from; i < n; i++ {
		m := msgs.Get(itoa(i))
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			switch b.Get("type").String() {
			case "tool_use":
				out[b.Get("name").String()] = true
			case "tool_search_tool_result":
				b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
					out[r.Get("tool_name").String()] = true
					return true
				})
			case "tool_result":
				b.Get("content").ForEach(func(_, r gjson.Result) bool {
					if r.Get("type").String() == "tool_reference" {
						out[r.Get("tool_name").String()] = true
					}
					return true
				})
			}
			return true
		})
	}
	delete(out, "")
	return out
}

// NormProject maps a worktree or scratch path to the repository it belongs to, so the project
// veto does not split one repository into many projects.
func NormProject(p string) string {
	if i := strings.Index(p, "/.claude/worktrees/"); i > 0 {
		return p[:i]
	}
	return p
}
