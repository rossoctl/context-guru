package dash

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

// The metadata that predicts when a user returns (issue #1198). The pieces that are read off one
// request (the wait tool's parameters, the prompt, the ping plan, the time zone) are filled by the
// capture site. The pieces that need history (the turn, the concurrency, the person's hash, a
// permission prompt that arrives after the row) live here, on the writer goroutine, in memory that
// is rebuilt from the database after a restart. Everything here is a number, a flag, an enum or a
// keyed hash. A value that cannot be computed stays NULL: absent means unknown.

// strategyMetaCols are the requests columns of these fields, in insert order (see store.go).
const strategyMetaCols = `wait_tool_timeout_ms, wait_tool_background, wait_prompt, tz_offset_min, turn,
		active_threads, ka_planned_pings, ka_min_prefix_tokens, ka_idle_s, user_hash,
		client_entrypoint, permission_mode, subagent_type, parent_fanout`

// strategyMetaN is the number of columns in strategyMetaCols.
const strategyMetaN = 14

// strategyMetaArgs are the insert values for strategyMetaCols. A zero value is NULL.
func strategyMetaArgs(e *Event) []any {
	var bg any
	if e.WaitToolBackground != nil {
		bg = boolInt(*e.WaitToolBackground)
	}
	return []any{e.WaitToolTimeoutMs, bg, nullStr(e.WaitPrompt), e.TZOffsetMin, nullInt(e.Turn),
		nullInt(e.ActiveThreads), e.KAPlannedPings, e.KAMinPrefixTokens, e.KAIdleS, nullStr(e.UserHash),
		nullStr(e.ClientEntrypoint), nullStr(e.PermissionMode), nullStr(e.SubagentType), nullInt(e.ParentFanout)}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

const (
	// ActiveThreadsWindow is how far back a thread counts as active for Event.ActiveThreads. Ten
	// minutes is two 5-minute cache lifetimes: a subagent that has run longer than that without a
	// request is no longer likely to be what the main thread waits for, and a longer window would
	// count threads that finished long ago. Fixed, so rows stay comparable across installs.
	ActiveThreadsWindow = 10 * time.Minute
	// promptWindow is how late after a request a Notification can still be matched to it.
	promptWindow = 30 * time.Minute
	// maxClientSignals caps the client_signals table. Each row is about 100 bytes.
	maxClientSignals = 100000
	// Bounds of the in-memory maps. Past them a map is emptied and rebuilt lazily from the database,
	// which costs one query per thread or session and loses no fact.
	maxTurnKeys    = 50000
	maxSessionKeys = 20000
	maxPending     = 1000
	userKeyFile    = "user_hash.key"
)

// ClientSignal is one accepted client lifecycle event (proxy/clientsignal.go).
type ClientSignal struct {
	TS       int64 // epoch ms
	TenantID string
	Session  string // the tenant-scoped session id the client's hook named
	Event    string // SessionEnd | SessionStart | Notification
	Reason   string // the reason, source or notification type; from a closed set
	// PermissionMode is the client's permission mode from its hook input (allow-listed by the host); "" when unsent.
	PermissionMode string
}

// promptOfSignal is the wait_prompt value a Notification sets on the request it interrupts, or ""
// when the signal does not mean a person is being asked (idle_prompt is the client idle, not blocked).
func promptOfSignal(s ClientSignal) string {
	if s.Event != "Notification" {
		return ""
	}
	switch s.Reason {
	case "permission_prompt":
		return "permission"
	case "elicitation_dialog":
		return "other"
	}
	return ""
}

type pendingPrompt struct {
	kind            string
	ts              int64
	tenant, session string
	recv            time.Time // when the writer got the signal; the grace counts from here
}

// strategyMeta is the writer's history for these fields. Only the writer goroutine uses it, apart
// from the tests; the mutex makes that safe either way.
type strategyMeta struct {
	mu      sync.Mutex
	turns   map[string]int              // tenant\0session\0thread -> real requests so far
	threads map[string]map[string]int64 // tenant\0session -> thread -> start ts of its latest real request
	pending map[string]pendingPrompt    // tenant\0session -> a Notification that found no row to mark yet
	modes   map[string]string           // tenant\0session -> the latest permission mode a hook reported

	keyOnce sync.Once
	key     []byte // nil when no key could be made: no hash is stored
}

func newStrategyMeta() *strategyMeta {
	return &strategyMeta{turns: map[string]int{}, threads: map[string]map[string]int64{}, pending: map[string]pendingPrompt{}, modes: map[string]string{}}
}

// realRow reports whether an event is a request the user's client sent: not a keep-alive ping, not an
// imported row, and with a session to count in.
func realRow(e *Event) bool { return !e.KeepAlive && e.Source == "" && e.SessionID != "" }

// fill sets Turn, ActiveThreads and the pending permission prompt on a real row, and hashes the
// person's id. It runs on the writer goroutine, beside the wait-cause closure, and never fails: a
// database error leaves the field unset.
func (r *Recorder) fillStrategyMeta(e *Event, userKey string) {
	r.fillSpawn(e)
	sm := r.sm
	if sm == nil {
		return
	}
	if userKey != "" {
		e.UserHash = sm.hash(r, e.TenantID, userKey)
	}
	if !realRow(e) {
		return
	}
	tk := e.TenantID + "\x00" + e.SessionID + "\x00" + e.ThreadID
	sk := e.TenantID + "\x00" + e.SessionID

	sm.mu.Lock()
	n, haveTurn := sm.turns[tk]
	m, haveSess := sm.threads[sk]
	sm.mu.Unlock()
	// First sight of a thread or session in this process (a start, a restart, or an emptied map):
	// rebuild from the rows already stored. Outside the lock, because it is a query.
	if !haveTurn {
		n = r.storedTurns(e.TenantID, e.SessionID, e.ThreadID)
	}
	var seed map[string]int64
	if !haveSess {
		seed = r.storedThreads(e.TenantID, e.SessionID, e.TS-ActiveThreadsWindow.Milliseconds())
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if cur, ok := sm.turns[tk]; ok {
		n = cur // another path filled it while the lock was off
	} else if len(sm.turns) >= maxTurnKeys {
		sm.turns = map[string]int{}
	}
	n++
	sm.turns[tk] = n
	e.Turn = n
	if m := sm.modes[sk]; m != "" {
		e.PermissionMode = m
	}

	if m = sm.threads[sk]; m == nil {
		if len(sm.threads) >= maxSessionKeys {
			sm.threads = map[string]map[string]int64{}
		}
		m = map[string]int64{}
		for t, ts := range seed {
			m[t] = ts
		}
		sm.threads[sk] = m
	}
	if e.TS > m[e.ThreadID] {
		m[e.ThreadID] = e.TS
	}
	cut := e.TS - ActiveThreadsWindow.Milliseconds()
	active := 0
	for t, ts := range m {
		if ts < cut {
			delete(m, t)
			continue
		}
		active++
	}
	e.ActiveThreads = active

	// A prompt mark that markPending chose for this row. The same rule as the database update in
	// writeSignal: a row whose prompt is none (or not computed) takes it; a question stays.
	if e.promptMark != "" && (e.WaitPrompt == "none" || e.WaitPrompt == "") {
		e.WaitPrompt = e.promptMark
	}
	e.promptMark = ""
}

// markPending hands each waiting prompt mark to the LAST real row of its session in the batch that
// started at or before the signal: that row is the one the prompt interrupted. A mark with no such
// row stays for a later batch until it is older than promptWindow. Runs on the writer, before the
// batch is filled.
func (r *Recorder) markPending(batch []*Event) {
	sm := r.sm
	if sm == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.pending) == 0 {
		return
	}
	var newest int64
	for _, e := range batch {
		newest = max(newest, e.TS)
	}
	for sk, p := range sm.pending {
		var best *Event
		for _, e := range batch {
			if realRow(e) && e.TenantID+"\x00"+e.SessionID == sk && e.TS <= p.ts &&
				p.ts-e.TS <= promptWindow.Milliseconds() && (best == nil || e.TS >= best.TS) {
				best = e
			}
		}
		if best != nil {
			best.promptMark = p.kind
			delete(sm.pending, sk)
		} else if newest-p.ts > promptWindow.Milliseconds() {
			delete(sm.pending, sk)
		}
	}
}

// storedTurns is the number of real requests the database already holds for a thread, or its
// highest stored turn when that is larger (rows pruned by retention lower the count, not the turn).
func (r *Recorder) storedTurns(tenant, session, thread string) int {
	var cnt, mx int64
	err := r.db.sql.QueryRowContext(r.db.readCtx(), `SELECT COUNT(*), COALESCE(MAX(turn), 0) FROM requests
		WHERE tenant_id = ? AND session_id = ? AND thread_id = ? AND keepalive = 0 AND source = ''`,
		tenant, session, thread).Scan(&cnt, &mx)
	if err != nil {
		return 0
	}
	return int(max(cnt, mx))
}

// storedThreads is the latest real request start of each thread of the session since ts.
func (r *Recorder) storedThreads(tenant, session string, since int64) map[string]int64 {
	out := map[string]int64{}
	rows, err := r.db.sql.QueryContext(r.db.readCtx(), `SELECT thread_id, MAX(ts) FROM requests
		WHERE tenant_id = ? AND session_id = ? AND keepalive = 0 AND source = '' AND ts >= ? GROUP BY thread_id`,
		tenant, session, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var ts int64
		if rows.Scan(&t, &ts) == nil {
			out[t] = ts
		}
	}
	return out
}

// hash is the pseudonymous id of a person: HMAC-SHA256 over the tenant and the person's key, cut to
// 64 bits. The tenant is part of the input so one person in two tenants gets two ids. "" when the
// secret cannot be made.
func (sm *strategyMeta) hash(r *Recorder, tenant, key string) string {
	sm.keyOnce.Do(func() { sm.key = loadUserKey(r.db.path) })
	if sm.key == nil || key == "" {
		return ""
	}
	h := hmac.New(sha256.New, sm.key)
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// loadUserKey reads the install's secret from user_hash.key next to the database, and creates it
// (32 random bytes, mode 0600) on first use. An in-memory database gets a secret that lives as long
// as the process, like the database itself. nil when the file can be neither read nor made: the
// hash is then left NULL rather than computed with a key nobody can reproduce.
func loadUserKey(dbPath string) []byte {
	if dbPath == "" {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil
		}
		return k
	}
	path := filepath.Join(filepath.Dir(dbPath), userKeyFile)
	for range 2 {
		if b, err := os.ReadFile(path); err == nil {
			if k, err := hex.DecodeString(string(trimNL(b))); err == nil && len(k) == 32 {
				return k
			}
			slog.Warn("dash: the user-hash key file is not valid; user_hash stays unset", "path", path)
			return nil
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue // another process made it between the read and the create: read it
		}
		if err != nil {
			slog.Warn("dash: could not create the user-hash key file; user_hash stays unset", "path", path, "err", err)
			return nil
		}
		_, werr := f.WriteString(hex.EncodeToString(k) + "\n")
		if cerr := f.Close(); werr != nil || cerr != nil {
			os.Remove(path)
			return nil
		}
		return k
	}
	return nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// RecordSignal hands a client lifecycle event to the writer. It never blocks and never fails: a
// full queue drops the event. Safe from any goroutine.
func (r *Recorder) RecordSignal(s ClientSignal) {
	if r == nil || r.sigCh == nil || s.Event == "" {
		return
	}
	select {
	case r.sigCh <- s:
	default:
		r.dropped.Add(1)
	}
}

// writeSignal stores one signal. A permission prompt or a dialog also marks the request it
// interrupted, but not at once: the row may still be on its way to the writer. The mark waits in
// memory for Options.PromptGrace. If a real row of the session that started at or before the signal
// is written in that time, markPending marks the last such row. When the grace ends, settlePending
// marks the latest stored row instead. Runs on the writer goroutine.
func (r *Recorder) writeSignal(s ClientSignal) {
	if s.TS == 0 {
		s.TS = time.Now().UnixMilli()
	}
	if s.PermissionMode != "" && r.sm != nil {
		r.sm.mu.Lock()
		if len(r.sm.modes) >= maxSessionKeys {
			r.sm.modes = map[string]string{}
		}
		r.sm.modes[s.TenantID+"\x00"+s.Session] = clip(s.PermissionMode)
		r.sm.mu.Unlock()
	}
	if _, err := r.db.sql.Exec(`INSERT INTO client_signals(ts, tenant_id, session_id, event, reason) VALUES (?,?,?,?,?)`,
		s.TS, s.TenantID, s.Session, clip(s.Event), clip(s.Reason)); err != nil {
		slog.Debug("dash: could not store a client signal", "err", err)
		return
	}
	kind := promptOfSignal(s)
	if kind == "" || r.sm == nil {
		return
	}
	r.sm.mu.Lock()
	defer r.sm.mu.Unlock()
	if len(r.sm.pending) >= maxPending {
		r.sm.pending = map[string]pendingPrompt{}
	}
	r.sm.pending[s.TenantID+"\x00"+s.Session] = pendingPrompt{kind: kind, ts: s.TS, tenant: s.TenantID, session: s.Session, recv: time.Now()}
}

// settlePending ends the grace of the prompt marks that waited long enough (all of them when force is
// set) and marks the latest stored row of each session. The rule is the same as in fillStrategyMeta:
// a row whose prompt is none or not computed takes the mark; a question stays.
func (r *Recorder) settlePending(force bool) {
	sm := r.sm
	if sm == nil {
		return
	}
	var due []pendingPrompt
	sm.mu.Lock()
	for sk, p := range sm.pending {
		if force || time.Since(p.recv) >= r.opts.PromptGrace {
			due = append(due, p)
			delete(sm.pending, sk)
		}
	}
	sm.mu.Unlock()
	for _, p := range due {
		if _, err := r.db.sql.Exec(`UPDATE requests SET wait_prompt = ? WHERE id = (
				SELECT id FROM requests WHERE tenant_id = ? AND session_id = ? AND keepalive = 0 AND source = ''
					AND ts <= ? AND ts >= ? ORDER BY ts DESC, id DESC LIMIT 1)
				AND (wait_prompt IS NULL OR wait_prompt = 'none')`,
			p.kind, p.tenant, p.session, p.ts, p.ts-promptWindow.Milliseconds()); err != nil {
			slog.Debug("dash: could not mark a prompt", "err", err)
		}
	}
}

func clip(s string) string {
	if len(s) <= 64 {
		return s
	}
	cut := 64
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// pruneClientSignals deletes signals older than maxAge (when it is positive) and keeps at most
// maxClientSignals rows, the newest.
func (d *DB) pruneClientSignals(now time.Time, maxAge time.Duration) {
	if maxAge > 0 {
		if _, err := d.sql.Exec(`DELETE FROM client_signals WHERE ts < ?`, now.Add(-maxAge).UnixMilli()); err != nil {
			slog.Debug("dash: could not prune client signals", "err", err)
		}
	}
	if _, err := d.sql.Exec(`DELETE FROM client_signals WHERE id <= (SELECT MAX(id) FROM client_signals) - ?`, maxClientSignals); err != nil {
		slog.Debug("dash: could not cap client signals", "err", err)
	}
}

// strategyMetaScan reads the nullable strategy columns of one requests row (scanRequest).
type strategyMetaScan struct {
	timeout, turn, active, planned, floor, idle, tz, bg sql.NullInt64
	prompt, entry, mode, spawn                          sql.NullString
	fanout                                              sql.NullInt64
}

func (s *strategyMetaScan) dest() []any {
	return []any{&s.timeout, &s.bg, &s.prompt, &s.tz, &s.turn, &s.active, &s.planned, &s.floor, &s.idle, &s.entry, &s.mode, &s.spawn, &s.fanout}
}

// into sets the Event fields; NULL stays unset.
func (s *strategyMetaScan) into(e *Event) {
	ptr := func(n sql.NullInt64) *int64 {
		if !n.Valid {
			return nil
		}
		v := n.Int64
		return &v
	}
	intPtr := func(n sql.NullInt64) *int {
		if !n.Valid {
			return nil
		}
		v := int(n.Int64)
		return &v
	}
	e.WaitToolTimeoutMs = ptr(s.timeout)
	if s.bg.Valid {
		b := s.bg.Int64 != 0
		e.WaitToolBackground = &b
	}
	e.WaitPrompt = s.prompt.String
	e.ClientEntrypoint, e.PermissionMode, e.SubagentType, e.ParentFanout = s.entry.String, s.mode.String, s.spawn.String, int(s.fanout.Int64)
	e.TZOffsetMin = intPtr(s.tz)
	e.Turn, e.ActiveThreads = int(s.turn.Int64), int(s.active.Int64)
	e.KAPlannedPings, e.KAMinPrefixTokens, e.KAIdleS = intPtr(s.planned), intPtr(s.floor), intPtr(s.idle)
}

// builtinSubagents are the subagent types that ship with the client. Any other type is a name the
// user chose, so it is stored as a keyed hash.
var builtinSubagents = map[string]bool{"general-purpose": true, "Explore": true, "Plan": true,
	"statusline-setup": true, "claude-code-guide": true}

// fillSpawn turns the raw subagent type of Event.SpawnTypeRaw into SubagentType.
func (r *Recorder) fillSpawn(e *Event) {
	raw := e.spawnRaw
	e.spawnRaw = ""
	if raw == "" || r.sm == nil {
		return
	}
	if builtinSubagents[raw] {
		e.SubagentType = raw
		return
	}
	if h := r.sm.hash(r, e.TenantID, "s:"+raw); h != "" {
		e.SubagentType = "h:" + h
	}
}
