package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, the one the dashboard uses: the build stays CGO-free
)

// Disk is the optional on-disk copy of the SESSION STATE: the keys a session needs between turns
// (summary checkpoint, last billed input, freeze decisions and so on, see statePrefixes). It
// backs a Memory store; it is not a Store itself.
//
// How it works:
//   - Memory.Put hands each state key to enqueue. Enqueue only records the value in a map, so a
//     write adds no disk wait to the request. A flusher goroutine writes the map in ONE
//     transaction every flushEvery. Writes to one key inside that window collapse to the last.
//   - Nothing is read at startup. Memory.LoadSession reads ONE session's keys when a request for
//     that session finds no recent load in memory. Memory.Get reads one key of a global
//     namespace (cg:xres:, cg:swx:) on a miss, because those keys carry no session id.
//   - Rows older than the retention are deleted by a sweep (open, then hourly).
//   - Rewind payloads (the stash) never reach this type: PutStash does not call enqueue, and
//     enqueue refuses every key outside statePrefixes.
//
// Fail open: every error is logged once, counted, and otherwise ignored. A request never waits on
// a disk error. A crash loses at most flushEvery of writes; Close flushes the rest.
//
// Known limit: the file is not encrypted. Open creates it with mode 0600.
type Disk struct {
	db        *sql.DB
	metaOnly  bool
	retention time.Duration
	now       func() time.Time

	mu      sync.Mutex
	pending map[string]pendingRow

	flushEvery time.Duration
	stop       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	logged     sync.Map // op -> struct{}: each distinct op is logged once
	errN       atomic.Int64
}

type pendingRow struct {
	val     []byte
	updated int64
}

// DefaultDiskRetention is how long a disk row lives after its last write: 30 days.
const DefaultDiskRetention = 720 * time.Hour

// DefaultStateTTL is the memory lifetime the proxy gives the state keys when the disk copy is on.
const DefaultStateTTL = 24 * time.Hour

const (
	defaultFlushEvery = time.Second
	sweepEvery        = time.Hour
	loadTimeout       = 3 * time.Second
	maxPending        = 50000
)

// DiskOptions configures OpenDisk. The zero value is "everything", 720 h.
type DiskOptions struct {
	// MetadataOnly keeps content keys (summary text, results, freeze decisions, held answers) off
	// the disk. Only the metadata keys are written.
	MetadataOnly bool
	// Retention is how long a row lives after its last write. Zero => DefaultDiskRetention.
	Retention time.Duration
	// FlushEvery is how often pending writes go to the file. Zero => 1 s.
	FlushEvery time.Duration
}

// metadataPrefixes hold counts, hashes, ids, flags and verdicts: no transcript text and no model
// output text.
var metadataPrefixes = []string{
	BilledPrefix, BilledThreadPrefix, LenPrefix, TTLPrefix, SeenPrefix,
	SweepDropPrefix, SweepKeepPrefix, SweepExpandPrefix, RestorePrefix, UsagePrefix,
}

// contentPrefixes hold text the proxy made or kept: the summary, extract_llm's rewrite, a frozen
// replacement, and the sweep's held answer.
var contentPrefixes = []string{
	SumPrefix, ResultPrefix, XResultPrefix, FrozenPrefix, SweepHeldPrefix,
}

// globalPrefixes carry no session id in the key, so LoadSession cannot find them.
// Memory.Get reads them from disk one key at a time.
var globalPrefixes = []string{XResultPrefix, SweepExpandPrefix}

// StatePrefixes is every namespace the disk copy can hold. It is also the set that takes the
// state lifetime in memory (Options.StateTTLSeconds).
var StatePrefixes = append(append([]string{}, metadataPrefixes...), contentPrefixes...)

func hasAnyPrefix(key string, ps []string) bool {
	for _, p := range ps {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func isStateKey(key string) bool { return hasAnyPrefix(key, StatePrefixes) }

func (d *Disk) allowed(key string) bool {
	if d.metaOnly {
		return hasAnyPrefix(key, metadataPrefixes)
	}
	return isStateKey(key)
}

func (d *Disk) prefixes() []string {
	if d.metaOnly {
		return metadataPrefixes
	}
	return StatePrefixes
}

// OpenDisk opens (and creates) the state file at path with mode 0600. A non-nil error means the
// caller runs memory only. The file is not created when OpenDisk is not called, which is how the
// off switch works.
func OpenDisk(path string, o DiskOptions) (*Disk, error) {
	if path == "" {
		return nil, fmt.Errorf("session state db: empty path")
	}
	// Create the file ourselves so the mode is 0600 whatever the umask. SQLite gives its -wal and
	// -shm files the mode of the main file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS session_state (key BLOB PRIMARY KEY, val BLOB NOT NULL, updated INTEGER NOT NULL) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS session_state_updated ON session_state(updated)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	ret := o.Retention
	if ret <= 0 {
		ret = DefaultDiskRetention
	}
	fe := o.FlushEvery
	if fe <= 0 {
		fe = defaultFlushEvery
	}
	d := &Disk{
		db: db, metaOnly: o.MetadataOnly, retention: ret, now: time.Now,
		pending: map[string]pendingRow{}, flushEvery: fe,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	d.sweep()
	go d.run()
	return d, nil
}

// Errors reports how many disk operations failed since open.
func (d *Disk) Errors() int64 { return d.errN.Load() }

func (d *Disk) fail(op string, err error) {
	d.errN.Add(1)
	if _, seen := d.logged.LoadOrStore(op, struct{}{}); !seen {
		slog.Warn("context-guru: session state disk error; continuing with memory only for what fails",
			"op", op, "err", err)
	}
}

// enqueue records a state write for the next flush. It never blocks on the disk.
func (d *Disk) enqueue(key string, val []byte, now time.Time) {
	if d == nil || !d.allowed(key) {
		return
	}
	cp := append([]byte(nil), val...)
	d.mu.Lock()
	// At the cap a NEW key is dropped, but an update to a key already waiting is kept: otherwise
	// the flush would write the older value.
	if _, waiting := d.pending[key]; waiting || len(d.pending) < maxPending {
		d.pending[key] = pendingRow{val: cp, updated: now.UnixMilli()}
	}
	d.mu.Unlock()
}

func (d *Disk) run() {
	defer close(d.done)
	flush := time.NewTicker(d.flushEvery)
	sweep := time.NewTicker(sweepEvery)
	defer flush.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-flush.C:
			d.Flush()
		case <-sweep.C:
			d.sweep()
		case <-d.stop:
			d.Flush()
			return
		}
	}
}

// Flush writes the pending rows now, in one transaction.
func (d *Disk) Flush() {
	d.mu.Lock()
	batch := d.pending
	d.pending = map[string]pendingRow{}
	d.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		d.fail("begin", err)
		return
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO session_state(key,val,updated) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET val=excluded.val, updated=excluded.updated`)
	if err != nil {
		_ = tx.Rollback()
		d.fail("prepare", err)
		return
	}
	defer st.Close()
	for k, r := range batch {
		if _, err := st.ExecContext(ctx, []byte(k), r.val, r.updated); err != nil {
			_ = tx.Rollback()
			d.fail("write", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		d.fail("commit", err)
	}
}

func (d *Disk) sweep() { d.sweepAt(d.now()) }

func (d *Disk) sweepAt(now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cut := now.Add(-d.retention).UnixMilli()
	if _, err := d.db.ExecContext(ctx, `DELETE FROM session_state WHERE updated < ?`, cut); err != nil {
		d.fail("sweep", err)
	}
}

// Close flushes pending rows and closes the file. Safe to call twice.
func (d *Disk) Close() error {
	if d == nil {
		return nil
	}
	var err error
	d.closeOnce.Do(func() {
		close(d.stop)
		<-d.done
		err = d.db.Close()
	})
	return err
}

type diskRow struct {
	key string
	val []byte
}

// loadSession returns the stored rows of one session: every state key whose session part is
// session, on any thread. It may also return a key of a session whose id starts with this one
// (for example "a:b" for "a"): those rows are real state of another session and restoring them
// is harmless.
func (d *Disk) loadSession(session string) ([]diskRow, bool) {
	if session == "" {
		return nil, true
	}
	var (
		conds []string
		args  []any
	)
	for _, p := range d.prefixes() {
		if hasAnyPrefix(p, globalPrefixes) {
			continue
		}
		lo := p + session
		conds = append(conds, "(key >= ? AND key < ?)")
		args = append(args, []byte(lo), []byte(lo+"\xff"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	rows, err := d.db.QueryContext(ctx, `SELECT key, val FROM session_state WHERE `+strings.Join(conds, " OR "), args...)
	if err != nil {
		d.fail("load", err)
		return nil, false
	}
	defer rows.Close()
	var out []diskRow
	for rows.Next() {
		var k, v []byte
		if err := rows.Scan(&k, &v); err != nil {
			d.fail("scan", err) // a corrupt row: skip it, keep the rest (not a failed load)
			continue
		}
		key := string(k)
		if !boundaryOK(key, session, d.prefixes()) {
			continue
		}
		out = append(out, diskRow{key: key, val: v})
	}
	if err := rows.Err(); err != nil {
		d.fail("load", err)
		return out, false
	}
	return out, true
}

// boundaryOK checks the byte after the session id. Keys continue with ":" (id), "\x00" (thread),
// "\x01" or "\x02" (sweep drop records), or end there.
func boundaryOK(key, session string, prefixes []string) bool {
	for _, p := range prefixes {
		if !strings.HasPrefix(key, p+session) {
			continue
		}
		rest := key[len(p)+len(session):]
		if rest == "" {
			return true
		}
		switch rest[0] {
		case ':', 0, 1, 2:
			return true
		}
	}
	return false
}

func (d *Disk) isGlobal(key string) bool { return d.allowed(key) && hasAnyPrefix(key, globalPrefixes) }

// get reads one key.
func (d *Disk) get(key string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	d.mu.Lock()
	if r, ok := d.pending[key]; ok {
		d.mu.Unlock()
		return r.val, true
	}
	d.mu.Unlock()
	var v []byte
	err := d.db.QueryRowContext(ctx, `SELECT val FROM session_state WHERE key = ?`, []byte(key)).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		d.fail("get", err)
		return nil, false
	}
	return v, true
}

// SessionLoader is an OPTIONAL Store capability: restoring one session's state from the disk copy
// into memory. apply calls it once per request, before the pipeline reads the store. A store
// without a disk copy does not implement it, or does nothing.
type SessionLoader interface {
	LoadSession(session string)
}

// LoadSession restores a session's state keys from disk when memory has no recent load of that
// session. See SessionLoader. One disk read per session per state lifetime; keys already in
// memory are not overwritten.
func (m *Memory) LoadSession(session string) {
	if m == nil || session == "" {
		return
	}
	m.mu.Lock()
	d := m.disk
	if d == nil {
		m.mu.Unlock()
		return
	}
	now := m.now()
	life := m.stateTTL
	if life <= 0 {
		life = m.ttl
	}
	if at, ok := m.loadedAt[session]; ok && now.Sub(at) < life {
		m.loadedAt[session] = now // sliding, like the entries it covers
		m.mu.Unlock()
		return
	}
	// A read failed lately: do not pay the read timeout again on every request.
	if at, ok := m.loadFailedAt[session]; ok && now.Sub(at) < loadRetryAfter {
		m.mu.Unlock()
		return
	}
	m.markLoaded(session, now)
	m.mu.Unlock()

	rows, ok := d.loadSession(session) // outside the lock: this is a disk read
	if !ok {
		// A failed read must not block a retry for a whole lifetime, so the mark goes (from the
		// order list too, or a failing disk would grow it by one per request). It does block a
		// retry for loadRetryAfter, so a hanging disk costs one timeout per session per minute.
		m.mu.Lock()
		m.unmarkLoaded(session)
		if len(m.loadFailedAt) >= maxLoaded {
			clear(m.loadFailedAt) // bounded; a cleared mark costs one early retry
		}
		m.loadFailedAt[session] = m.now()
		m.mu.Unlock()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		if el, ok := m.items[r.key]; ok && !m.now().After(el.Value.(*entry).expires) {
			continue // memory is newer than disk
		}
		m.putLocked(r.key, r.val)
	}
}

// maxLoaded bounds the loaded-session record, like noteLost bounds its marks: past it the OLDEST
// id is dropped on insert. A dropped id costs one more disk read when that session returns.
const maxLoaded = 4096

// markLoaded records a load. The caller holds m.mu. No scan: eviction pops the oldest id.
func (m *Memory) markLoaded(session string, now time.Time) {
	if _, ok := m.loadedAt[session]; !ok {
		for len(m.loadedAt) >= maxLoaded && len(m.loadedOrder) > 0 {
			oldest := m.loadedOrder[0]
			m.loadedOrder = m.loadedOrder[1:]
			delete(m.loadedAt, oldest)
		}
		m.loadedOrder = append(m.loadedOrder, session)
	}
	m.loadedAt[session] = now
}

// unmarkLoaded forgets a session's load mark. The caller holds m.mu. It scans the order list, on
// the failure path only.
func (m *Memory) unmarkLoaded(session string) {
	delete(m.loadedAt, session)
	for i, k := range m.loadedOrder {
		if k == session {
			m.loadedOrder = append(m.loadedOrder[:i], m.loadedOrder[i+1:]...)
			break
		}
	}
}

// loadRetryAfter is the pause before a session whose disk read failed is read again.
const loadRetryAfter = time.Minute

// AttachDisk makes d the write-through copy of this store's state. Call it once, before serving.
func (m *Memory) AttachDisk(d *Disk) {
	m.mu.Lock()
	m.disk = d
	m.mu.Unlock()
}

// getGlobal is Get's disk fallback for the namespaces with no session id in the key.
func (m *Memory) getGlobal(key string) ([]byte, bool) {
	m.mu.Lock()
	d := m.disk
	m.mu.Unlock()
	if d == nil || !d.isGlobal(key) {
		return nil, false
	}
	v, ok := d.get(key)
	if !ok {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, live := m.items[key]; live && !m.now().After(el.Value.(*entry).expires) {
		return el.Value.(*entry).payload, true // another goroutine wrote it meanwhile
	}
	m.putLocked(key, v)
	return v, true
}
