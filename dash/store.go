package dash

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/store"
	"modernc.org/sqlite" // pure-Go driver: no extra C toolchain for the dashboard
)

// memSeq names in-memory databases uniquely. See Open.
var memSeq atomic.Uint64

// DB is the dashboard's durable store. All writes go through one goroutine (see
// capture.go), so the only concurrency here is reads racing that writer, which
// SQLite in WAL mode handles.
type DB struct {
	sql  *sql.DB
	path string // "" for the in-memory store
	// dropProbe, when set, runs inside DropOldestSessions between picking the victims and
	// deleting them. Test seam for the window #772 closed: a request landing there must not
	// change which sessions the delete removes.
	dropProbe func()
	// readOnly is set by OpenReadOnly. Every write the handle could make on its own (checkpoint)
	// checks it; SQLite's mode=ro and query_only refuse any other.
	readOnly bool
	// ctx bounds the READS made through this handle, so a request the caller has abandoned stops
	// costing the process. nil means context.Background().
	//
	// Carried on a shallow copy (see WithContext) rather than added as a parameter to every
	// query method, which would have meant a context argument on fifty call sites, most of them
	// tests, for a value only the HTTP layer can supply. The struct is two fields and holds no
	// lock, so copying it is safe; `go vet` would say otherwise if it did.
	ctx context.Context
	// sizeSpread is CachesplitSizeSpread's own TTL cache (dash/cachehistory.go) — a POINTER, not
	// an inline mutex, so WithContext's shallow copy above stays lock-free and copies the
	// pointer, sharing one cache across every handle this *DB produces rather than giving each
	// shallow copy its own, independently-stale one.
	sizeSpread *cachesplitSizeSpreadCache
}

// WithContext returns a handle whose reads are cancellable, for a caller that has one.
//
// The KV-cache analysis reads up to kvCacheMaxRows rows and builds a Request per row, which is
// ~135 MB and several seconds at the ceiling. Without a context that work continued after the
// client had gone: a request killed 1.5 s in still burned 7.1 s of CPU and allocated to
// completion, so a reader who hits refresh five times commits five times the memory and gets
// none of it back. Reads made through this handle stop when the request does.
func (d *DB) WithContext(ctx context.Context) *DB {
	if d == nil {
		return nil
	}
	c := *d
	c.ctx = ctx
	return &c
}

// readCtx is the context reads run under.
func (d *DB) readCtx() context.Context {
	if d == nil || d.ctx == nil {
		return context.Background()
	}
	return d.ctx
}

// Open opens (creating if needed) the dashboard database at path. path ":memory:"
// or "" yields an ephemeral in-memory database, which is also the fallback the
// proxy uses when the configured path is unwritable — the proxy must keep serving
// traffic whatever the disk says.
//
// On a schema-version mismatch the existing file is renamed aside
// (<path>.v<old>.bak) and a fresh database is created: the METRICS in it are a
// derived view, so discarding them beats refusing to boot, and renaming beats
// deleting.
//
// The file is NOT purely derived any more, and that is the part to keep in mind
// before extending this path: archived_sessions and tenant_spend are the only
// copies of their facts (the cold-storage index and the monthly spend rollup), so
// the fresh database CARRIES THEM ACROSS from the renamed file — see
// carryNonDerived. Anything else added here that cannot be recomputed from traffic
// has to join that list.
func Open(path string) (*DB, error) {
	if path == "" || path == ":memory:" {
		// A UNIQUE name per Open, not a bare `file::memory:`.
		//
		// `cache=shared` is required: database/sql keeps a connection POOL and a private
		// in-memory database exists per connection, so every pooled connection would
		// otherwise see its own empty database. But under `cache=shared` the NAME
		// identifies the database, and `file::memory:` is a single name — so every
		// in-memory dashboard in the process WAS the same database. Two proxies falling
		// back to :memory: silently merged their history, and :memory: tests leaked rows
		// into each other (the flakiest possible failure). A per-instance name keeps the
		// pooling behaviour and removes the collision.
		// foreign_keys(1) here as well as in dsn(): the ON DELETE CASCADE from requests to
		// request_components / request_content is how retention, eviction and a tenant purge
		// avoid leaving orphan rows, and the pragma is PER CONNECTION. Without it every
		// in-memory database in this package — which is every test — quietly kept its child
		// rows while the file-backed deployment deleted them, so the tests could not see the
		// bug they were meant to catch.
		return openDSN(fmt.Sprintf("file:dashmem%d?mode=memory&cache=shared&_pragma=foreign_keys(1)",
			memSeq.Add(1)), "")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := openDSN(dsn(path), path)
	if err == nil {
		// A fresh boot inherits whatever WAL the PREVIOUS process left on disk, and
		// nothing before this point ever checkpoints it: reclaim() only runs from the
		// byte-budget path, which a deployment inside its retention budget never
		// takes. Left alone, the first ordinary write past SQLite's own
		// wal_autocheckpoint threshold (1000 pages) checkpoints the WHOLE inherited
		// backlog inline, on the writer's own commit — measured against a WAL bloated
		// to production scale, that single commit took 5.5s, and every reader sharing
		// this *sql.DB (e.g. /metrics) blocked for the same span, reproducing on a
		// cold restart with no other traffic at all. Checkpointing once here, before
		// any traffic is served, pays that backlog down up front instead of on the
		// first request that happens to land on it.
		db.checkpoint()
	}
	var mismatch *versionMismatch
	if errors.As(err, &mismatch) {
		aside := fmt.Sprintf("%s.v%s.bak", path, sanitizeVersion(mismatch.have))
		slog.Warn("dash: schema version changed; preserving the old database and starting fresh",
			"old_version", mismatch.have, "new_version", schemaVersion, "preserved_at", aside)
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("dash: %w (and could not preserve it: %v)", err, rerr)
		}
		// Move the write-ahead log with the database it belongs to. Two reasons, both
		// about the data this whole path exists to keep: a -wal left behind holds
		// commits the .bak does not (so carrying tenant_spend across would silently
		// under-count), and a foreign -wal sitting next to the FRESH file is a
		// recovery hazard nobody would ever diagnose.
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, serr := os.Stat(path + suffix); serr == nil {
				_ = os.Rename(path+suffix, aside+suffix)
			}
		}
		fresh, ferr := openDSN(dsn(path), path)
		if ferr != nil {
			return nil, ferr
		}
		carryNonDerived(fresh, aside)
		return fresh, nil
	}
	return db, err
}

// nonDerivedTables are the tables a schema bump must NOT discard, with the columns
// to carry across. They are the tables whose contents cannot be recomputed from
// traffic:
//
//   - archived_sessions is the ONLY index of what has been migrated to cold
//     storage. Losing it orphans every archived object — the data is still there and
//     still costs storage, but nothing can find it, and the Archive view goes empty
//     while the sessions sit in Box.
//   - tenant_spend is the monthly spend rollup, which exists precisely so a spend
//     cap cannot be reset by deleting rows. Losing it resets every tenant's
//     month-to-date spend to zero.
//
// Columns are named rather than SELECT *: an older file whose column ORDER differs
// must fail loudly instead of writing a session id into a byte count.
var nonDerivedTables = map[string]string{
	"archived_sessions": `session_id,tenant_id,first_ts,last_ts,requests,content_path,
	                      content_bytes,full_path,full_bytes,archived_at,remote`,
	"tenant_spend": `tenant_id,month,usd`,
	// The session state (#769) is rebuilt from traffic, but only by paying for it again (a summary
	// is a model call), so a schema bump carries it.
	"session_state": `key,val,updated`,
}

// carryNonDerived copies the non-derived tables out of the database renamed aside by
// Open and into the fresh one. Best-effort BY DESIGN: every failure is logged loudly
// and boot continues, because refusing to boot over the dashboard is the exact
// failure this whole path exists to avoid. An old file that is corrupt, unreadable,
// or simply predates one of these tables all land here.
//
// One dedicated connection, because ATTACH is per-connection in SQLite and
// database/sql hands out a POOLED one — attaching on one connection and inserting on
// another would look like a missing table.
func carryNonDerived(fresh *DB, oldPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := fresh.sql.Conn(ctx)
	if err != nil {
		slog.Error("dash: could not carry the archive index and spend rollup across the schema bump",
			"preserved_at", oldPath, "err", err)
		return
	}
	defer conn.Close()
	// Read-only and immutable: this file is history now, and a corrupt one must fail
	// the ATTACH rather than get repaired in place.
	if _, err := conn.ExecContext(ctx,
		`ATTACH DATABASE ? AS old`, "file:"+oldPath+"?mode=ro"); err != nil {
		slog.Error("dash: could not open the preserved database to carry non-derived tables across; "+
			"the cold-storage index and month-to-date spend in it are NOT in the new file",
			"preserved_at", oldPath, "err", err)
		return
	}
	defer func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE old`) }()
	for table, cols := range nonDerivedTables {
		// INSERT OR REPLACE, not INSERT: the fresh database is empty, so this only ever
		// matters if a future caller runs it twice — and then last-write-wins on the
		// primary key is the right answer for both tables.
		if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO main.`+table+
			` (`+cols+`) SELECT `+cols+` FROM old.`+table); err != nil {
			// Expected and harmless when the old schema simply predates the table; a
			// genuine failure looks the same from here, so log both at ERROR and let the
			// operator read the message.
			slog.Error("dash: could not carry a non-derived table across the schema bump "+
				"(harmless if the preserved database predates it)",
				"table", table, "preserved_at", oldPath, "err", err)
			continue
		}
		var n int64
		_ = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.`+table).Scan(&n)
		slog.Info("dash: carried a non-derived table across the schema bump", "table", table, "rows", n)
	}
}

// dsn builds the driver DSN: WAL for concurrent reads while the writer commits,
// NORMAL synchronous (a lost tail of observability rows on power loss is
// acceptable; halving write cost is not), a busy timeout so a read never errors
// out under a concurrent commit, and foreign keys on for the ON DELETE CASCADEs
// retention relies on.
func dsn(path string) string {
	// auto_vacuum(2) is INCREMENTAL. It matters at hosted scale: the size rule used to
	// reclaim pages with a full VACUUM, which rewrites the entire file — fine for a
	// 512 MiB default, a multi-minute stall of the writer goroutine at tens of GB, and
	// the stall lands exactly when the disk is under pressure and observability is
	// most wanted. INCREMENTAL lets Prune reclaim a bounded number of pages per pass.
	//
	// SQLite can only set auto_vacuum on an EMPTY database, so this takes effect for
	// files created from here on. An existing file keeps auto_vacuum=NONE and falls
	// back to the full VACUUM path in Prune, which is what it did before.
	//
	// busy_timeout is FIRST. The driver runs these in order on every new connection, and the
	// journal_mode statement needs a lock: with the timeout set after it, a connection opened while
	// another held the WAL (a checkpoint, a commit) failed at once with SQLITE_BUSY instead of
	// waiting, and a burst of concurrent reads opens a burst of connections.
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_pragma=auto_vacuum(2)"
}

func openDSN(d, path string) (*DB, error) {
	sdb, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite is pure Go: unlike a CGO sqlite3 build, connections do not share
	// an mmap'd page cache, so each one carries its own in-heap page cache and VDBE state.
	// Left unbounded (database/sql's default), a burst of concurrent dashboard/metrics
	// reads opens one pooled connection per in-flight request — measured on this
	// deployment climbing past 600 concurrent connections and taking the process from
	// ~2GB to a hard OOM kill within minutes. WAL mode already gives every reader a
	// consistent snapshot without needing its own connection to do it quickly, so bounding
	// the pool caps the memory risk without needing per-connection changes elsewhere.
	//
	// First shipped at 20/10, which turned out too low for this deployment's real
	// concurrent demand: live traffic queued for a connection long enough to blow the
	// dashboard handlers' own 10s timeout (dash/api.go's dashHandlerTimeout), on a box with
	// 561% CPU and zero SQLITE_BUSY errors logged — i.e. queued for a POOL SLOT, never even
	// reaching SQLite to contend on a lock. 100/50 costs at most ~285MB more at the
	// ~2.85MB/connection rate measured against the 600-connection incident (vs. an 8GB
	// cgroup cap and ~300MB baseline) while giving 5x the headroom before a burst queues.
	sdb.SetMaxOpenConns(100)
	sdb.SetMaxIdleConns(50)
	sdb.SetConnMaxIdleTime(5 * time.Minute)
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, err
	}
	if err := migrate(sdb); err != nil {
		sdb.Close()
		return nil, err
	}
	return &DB{sql: sdb, path: path, sizeSpread: &cachesplitSizeSpreadCache{}}, nil
}

// OpenReadOnly opens an EXISTING dashboard database for reading only — for analysis tools that run
// beside a live proxy, which owns the file.
//
// Open is not safe for that: it migrates the schema (additive DDL, new columns), stamps the
// version, checkpoints the WAL, and on a version mismatch RENAMES the file aside. Any of those
// against a database a serving process holds is a write the operator never asked for. This opens
// with SQLite's mode=ro and query_only, runs none of them, and REFUSES a database whose schema
// version is not this build's: a reader cannot migrate, and a query written for one schema read
// against another returns wrong numbers rather than an error.
//
// mode=ro still reads the -wal, so rows the live writer has committed but not yet checkpointed
// are visible. A missing file is an error, not a fresh database. With NO live writer, SQLite
// still creates an empty -wal and the -shm index beside a WAL database to read it, so the
// directory must be writable; the database file itself is never modified.
func OpenReadOnly(path string) (*DB, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("dash: OpenReadOnly needs a database file")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)&cg_ro=1")
	if err != nil {
		return nil, err
	}
	// A reader beside a live proxy: a handful of connections, never the 100 the writer's pool
	// holds, so an analysis cannot crowd the serving process off its own file.
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	var have string
	if err := sdb.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&have); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("dash: read-only open: %w", err)
	}
	if have != fmt.Sprint(schemaVersion) {
		sdb.Close()
		return nil, &versionMismatch{have: have}
	}
	return &DB{sql: sdb, path: path, readOnly: true, sizeSpread: &cachesplitSizeSpreadCache{}}, nil
}

// A read-only handle cannot ALTER, and the queries read every column of additiveColumns, which a
// file written by an older proxy may lack (the Ping button's manual_ping among them). Failing each
// query with "no such column" would break a reader (the cache-campaign tool opens live databases
// this way, and operators mix tool and proxy versions), so each read-only connection gets a TEMP
// view named `requests` that adds every missing column as its default. Temp objects are searched
// before main, so every unqualified `requests` resolves to the shim; the view is per connection and
// writes nothing to the file. A file with all its columns gets no shim. query_only is switched on
// last, because it forbids creating even a temp object.
//
// The hook is the driver's process-wide one, so it acts only on DSNs carrying cg_ro=1.
func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		if !strings.Contains(dsn, "cg_ro=1") {
			return nil
		}
		ctx := context.Background()
		rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info('requests')`, nil)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		vals := make([]driver.Value, 1)
		for rows.Next(vals) == nil {
			if n, ok := vals[0].(string); ok {
				have[n] = true
			}
		}
		rows.Close()
		var extra string
		for _, c := range additiveColumns {
			if c.table == "requests" && !have[c.column] {
				extra += ", " + additiveDefault(c.ddl) + " AS " + c.column
			}
		}
		if extra != "" && len(have) > 0 {
			if _, err := conn.ExecContext(ctx, `CREATE TEMP VIEW requests AS SELECT *`+extra+` FROM main.requests`, nil); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, `PRAGMA query_only = 1`, nil)
		return err
	})
}

// additiveDefault is the literal a missing additive column reads as: its DEFAULT, else NULL (a
// column declared without one, like keepalive_strategy_id, reads NULL on an old row).
func additiveDefault(ddl string) string {
	if _, def, ok := strings.Cut(ddl, "DEFAULT "); ok {
		return strings.TrimSpace(def)
	}
	return "NULL"
}

// sanitizeVersion keeps a version string safe to embed in a filename.
func sanitizeVersion(v string) string {
	v = strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return '-'
	}, v)
	if v == "" {
		return "unknown"
	}
	if len(v) > 16 {
		v = v[:16]
	}
	return v
}

// Close releases the database.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// SQL returns the underlying handle, for a host that keeps a table of its own in this database.
// The session state store (package store) does: it shares this pool, so one pool serializes every
// writer in the process. The caller must not close it.
func (d *DB) SQL() *sql.DB { return d.sql }

// deleteSessionState removes the state rows of sessions this dashboard just deleted: each
// session's own id and its content-derived aliases (the session_alias column). A key another
// live session still uses is kept, whatever half it uses it by: a session whose rows carry it
// as their alias, or a headerless session whose own id IS it. Both read that state on every
// turn (cg:seen:, cg:ttl:), so removing it under them makes a warm cache read cold. Call it
// AFTER the sessions' request rows are gone — the in-use check counts what is left. Best
// effort: the state is rebuilt from traffic, so a failure only logs. The retention prune does
// NOT call it: state follows its own retention.
func (d *DB) deleteSessionState(sessions, aliases []string) {
	keys := make([]string, 0, len(sessions)+len(aliases))
	keys = append(keys, sessions...)
	keys = append(keys, aliases...)
	inUse := d.inUseIDs(keys)
	for _, k := range keys {
		if k == "" || inUse[k] {
			continue
		}
		if _, err := store.DeleteSessionState(d.sql, k); err != nil {
			slog.Warn("dash: could not delete the session state of a removed session", "session", k, "err", err)
		}
	}
}

// inUseIDs tells which of the given keys a request row still uses, as a session's alias or as
// a session's own id. TWO queries for the whole list, not one per key: this runs on the writer
// goroutine during eviction passes, and requests holds millions of rows in production. An
// answer that cannot be read — the query fails to open, a row fails to scan, or the scan ends
// early — says every key is in use. Keeping rows is the safe side, and retention bounds them.
func (d *DB) inUseIDs(keys []string) map[string]bool {
	inUse := map[string]bool{}
	if len(keys) == 0 {
		return inUse
	}
	queries := []string{
		`SELECT DISTINCT session_alias FROM requests WHERE session_alias IN (`,
		`SELECT DISTINCT session_id FROM requests WHERE session_id IN (`,
	}
	// giveUp keeps the promise above however the answer breaks: every key counts as in use.
	// Logged once per call, whatever the failure and however many queries it hits.
	var warned bool
	giveUp := func(err error) {
		if !warned {
			slog.Warn("dash: could not tell whether a removed session's state is still in use",
				"err", err)
			warned = true
		}
		for _, k := range keys {
			inUse[k] = true
		}
	}
	for _, q := range queries {
		for _, chunk := range chunkStrings(keys) {
			rows, err := d.sql.QueryContext(d.readCtx(), q+placeholders(len(chunk))+`)`, anyArgs(chunk)...)
			if err != nil {
				giveUp(err)
				return inUse
			}
			for rows.Next() {
				var k string
				// A row that fails to scan says the same thing as a query that fails to open:
				// the answer is not trustworthy, and a key it could not deliver must not read
				// as unused.
				if err := rows.Scan(&k); err != nil {
					rows.Close()
					giveUp(err)
					return inUse
				}
				inUse[k] = true
			}
			// A scan that fails midway ends the loop early: the keys it did not reach would
			// read as unused, and the caller would then delete state a live session still
			// reads. That is the one unsafe direction this function must never fail in.
			if err := rows.Err(); err != nil {
				rows.Close()
				giveUp(err)
				return inUse
			}
			rows.Close()
		}
	}
	return inUse
}

// sessionAliases lists the distinct alias ids stored on the given sessions' rows, minus each
// row's own session id (a headerless turn's id IS its alias, and its rows store none). Read
// BEFORE the rows are deleted: the alias is a column, and the column goes with them.
func (d *DB) sessionAliases(sessions ...string) ([]string, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	var out []string
	for _, chunk := range chunkStrings(sessions) {
		rows, err := d.sql.QueryContext(d.readCtx(), `SELECT DISTINCT session_alias FROM requests
			WHERE session_alias <> '' AND session_alias <> session_id
			AND session_id IN (`+placeholders(len(chunk))+`)`, anyArgs(chunk)...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var a string
			if rows.Scan(&a) == nil {
				out = append(out, a)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
	}
	return out, nil
}

// Path returns the on-disk path ("" when in-memory).
func (d *DB) Path() string { return d.path }

// gzPair is one blob's before/after compression, precomputed outside the transaction.
type gzPair struct{ before, after []byte }

// InsertEvents writes events straight to the store in one transaction, without the recorder's queue. For test fixtures in other packages.
func (d *DB) InsertEvents(evs []*Event) error { return d.insertBatch(evs) }

// insertBatch writes a batch of captured events in ONE transaction — the whole
// point of batching: a per-request fsync would make the writer the bottleneck
// under agent traffic. A failed batch is logged and dropped; observability never
// retries into a growing backlog.
func (d *DB) insertBatch(evs []*Event) error {
	// Every gzip compression this batch needs, done BEFORE the transaction opens. gzip is
	// real CPU work — up to ContentMaxPerRequest x 2 blobs per event, plus one pair per
	// extraction call — and this driver blocks a concurrent READ for the writer's WHOLE open
	// transaction (see CLAUDE.local.md). Compressing inside the transaction was paying that
	// CPU cost while every reader on the box waited for it; precomputing here means the
	// transaction itself does nothing but the inserts it actually needs the lock for.
	contentGz := make([][]gzPair, len(evs))
	xcallGz := make([][]gzPair, len(evs))
	for i, e := range evs {
		cg := make([]gzPair, len(e.Content))
		for j, c := range e.Content {
			cg[j] = gzPair{gzipText(c.Before), gzipText(c.After)}
		}
		contentGz[i] = cg
		xg := make([]gzPair, len(e.Extractions))
		for j, x := range e.Extractions {
			xg[j] = gzPair{gzipText(x.Before), gzipText(x.After)}
		}
		xcallGz[i] = xg
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	reqStmt, err := tx.Prepare(`INSERT INTO requests(
		ts, tenant_id, session_id, model, provider, agent, preset, mode, route, status, bypassed, cache_aware,
		messages, tokens_before, tokens_after, attempted_tokens, frozen_tokens, saved_unique,
		fresh_input, cache_read, cache_write, output_tokens,
		cost_usd, baseline_cost_usd, cg_llm_cost_usd, cache_saved_usd, cachesplit_saved_usd,
		split_stable_tokens, split_tail_hash, filtered_decl_tokens, decl_digest,
		cache_ttl, sse_buffered, ttfb_ms,
		cache_write_1h, keepalive, keepalive_pings, keepalive_saved_usd, cg_latency_ms, upstream_ms,
		expands, expand_tokens, reverts, token_accounting, cache_miss_reason, uncompressed_reason,
		reasoning_effort, thinking_mode, thinking_budget, temperature, top_p, max_tokens, stream,
		tool_choice, tools, system_blocks,
		cache_bp_system, cache_bp_tools, cache_bp_messages, cache_bp_blocks, stop_reason,
		keepalive_strategy_id, billed_token_factor, thread_id,
		waiting_for, wait_tool, wake_cause, open_background, open_teammate, monitor_expiry_ms,
		agent_compaction, cache_miss_cause, manual_ping, manual_saved_usd, project, source,
		sweep_expands, sweep_expand_tokens, sweep_false_expands, sweep_answer_usd, session_alias
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer reqStmt.Close()
	compStmt, err := tx.Prepare(`INSERT INTO request_components(
		request_id, component, kind, acted, mutated, reverted, skipped, saved_gross, saved_unique,
		saved_usd, duration_ms, err, gates, events
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer compStmt.Close()
	contentStmt, err := tx.Prepare(`INSERT INTO request_content(
		request_id, seq, path, before_tokens, after_tokens, before_gz, after_gz, components
	) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer contentStmt.Close()
	xcallStmt, err := tx.Prepare(`INSERT INTO extraction_calls(
		request_id, seq, component, tenant_id, session_id, ts, cold, escalated, aggressiveness,
		strategy, model, candidate_tokens, saved_tokens, prompt_tokens, completion_tokens,
		cache_read, cache_write, cost_usd, latency_ms, accepted, gate_reason, rejection, summary,
		before_gz, after_gz
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer xcallStmt.Close()

	lifeStmt, err := tx.Prepare(`INSERT INTO request_lifecycle(
		request_id, result_count, result_bytes, result_error, since_prev_end_ms, granted_ttl, ping_n)
		VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer lifeStmt.Close()

	spend := map[spendKey]float64{}
	for idx, e := range evs {
		// nil, not "", when no strategy applied — so the column stores SQL NULL and stays
		// distinguishable from a row written before this feature existed either way; see
		// the keepalive_strategy_id column comment in schema.go.
		var strategyID any
		if e.KeepAliveStrategyID != "" {
			strategyID = e.KeepAliveStrategyID
		}
		res, err := reqStmt.Exec(
			e.TS, e.TenantID, e.SessionID, e.Model, e.Provider, e.Agent, e.Preset, e.Mode, e.Route, e.Status,
			boolInt(e.Bypassed), boolInt(e.CacheAware),
			e.Messages, e.TokensBefore, e.TokensAfter, e.AttemptedTokens, e.FrozenTokens, e.SavedUnique,
			e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens,
			e.CostUSD, e.BaselineCostUSD, e.CGLLMCostUSD, e.CacheSavedUSD, e.CachesplitSavedUSD,
			e.SplitStableTokens, int64(e.SplitTailHash), e.FilteredDeclTokens, e.DeclDigest,
			e.CacheTTL, boolInt(e.SSEBuffered), e.TTFBMs,
			e.CacheWrite1h, boolInt(e.KeepAlive), e.KeepAlivePings, e.KeepAliveSavedUSD,
			e.CGLatencyMs, e.UpstreamMs,
			e.Expands, e.ExpandTokens, e.Reverts, e.TokenAccounting, e.CacheMissReason, e.UncompressedReason,
			e.ReasoningEffort, e.ThinkingMode, e.ThinkingBudget, e.Temperature, e.TopP, e.MaxTokens,
			boolInt(e.Stream), e.ToolChoice, e.Tools, e.SystemBlocks,
			e.CacheBPSystem, e.CacheBPTools, e.CacheBPMessages, e.CacheBPBlocks, e.StopReason,
			strategyID, e.BilledTokenFactor, e.ThreadID,
			e.WaitingFor, e.WaitTool, e.WakeCause, e.OpenBackground, e.OpenTeammate, e.MonitorExpiryMs,
			boolInt(e.AgentCompaction), e.CacheMissCause, boolInt(e.ManualPing), e.ManualSavedUSD,
			// Sanitized HERE, at the write, rather than trusted from the caller: this is the last
			// point before the value is on disk and on the SSE feed, and a path is a
			// client-chosen string that could be named after a credential.
			ProjectPath(e.Project),
			/* Allow-listed rather than stored as given: this is the field every honesty rule
			 * downstream keys off, so an unrecognised value must not quietly become a third
			 * kind of row. Anything but the one known marker records as measured, which is the
			 * safe direction — it claims LESS about the row. */
			sourceOf(e.Source),
			e.SweepExpands, e.SweepExpandTokens, e.SweepFalseExpands, e.SweepAnswerUSD,
			e.SessionAlias,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		e.ID = id
		if l := e.Lifecycle; l != nil {
			if _, err := lifeStmt.Exec(id, l.ResultCount, l.ResultBytes, boolInt(l.ResultError),
				l.SincePrevEndMs, l.GrantedTTL, l.PingN); err != nil {
				return err
			}
		}
		for _, c := range e.Components {
			if _, err := compStmt.Exec(id, c.Component, c.Kind,
				boolInt(c.Acted), boolInt(c.Mutated), boolInt(c.Reverted), boolInt(c.Skipped),
				c.SavedGross, c.SavedUnique, c.SavedUSD, c.DurationMs, c.Err,
				gatesJSON(c.Gates), gatesJSON(c.Events)); err != nil {
				return err
			}
		}
		for i, c := range e.Content {
			if _, err := contentStmt.Exec(id, i, c.Path, c.BeforeTokens, c.AfterTokens,
				contentGz[idx][i].before, contentGz[idx][i].after, strings.Join(c.Components, ",")); err != nil {
				return err
			}
		}
		for i, x := range e.Extractions {
			// tenant_id and session_id are denormalized onto the row on purpose: the queries
			// that matter ("what did extraction cost this account this month", "show me this
			// session's calls") would otherwise all need the join, and a request row can be
			// evicted by retention while this row is still interesting.
			if _, err := xcallStmt.Exec(id, i, x.Component, e.TenantID, e.SessionID, e.TS,
				boolInt(x.Cold), boolInt(x.Escalated), x.Aggressiveness, x.Strategy, x.Model,
				x.CandidateTokens, x.SavedTokens, x.PromptTokens, x.CompletionTok,
				x.CacheRead, x.CacheWrite, x.CostUSD, x.LatencyMs, boolInt(x.Accepted),
				x.GateReason, x.Rejection, x.Summary,
				xcallGz[idx][i].before, xcallGz[idx][i].after); err != nil {
				return err
			}
		}
		spend[spendKey{e.TenantID, monthKey(e.TS)}] += e.CostUSD + e.CGLLMCostUSD
	}
	// In the SAME transaction as the rows: a rollup committed separately would either
	// double-count a retried batch or lose a committed one, and this number gates
	// spending the organisation's money.
	for k, usd := range spend {
		if _, err := tx.Exec(`INSERT INTO tenant_spend(tenant_id,month,usd) VALUES (?,?,?)
			ON CONFLICT(tenant_id,month) DO UPDATE SET usd = usd + excluded.usd`,
			k.tenant, k.month, usd); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// spendKey is one tenant-month bucket of the spend rollup.
type spendKey struct{ tenant, month string }

// monthKey renders an event timestamp as the UTC calendar month the cap bills against.
func monthKey(tsMillis int64) string {
	return time.UnixMilli(tsMillis).UTC().Format("2006-01")
}

// gatesJSON encodes a gate map for storage.
//
// A component that gated NOTHING stores "{}", not the empty string. The two are different
// facts and the UI shows them differently: "{}" is "this component turned nothing away",
// while an empty string can only mean "written before this column existed", i.e. unknown.
// Collapsing them made every healthy component read "unknown" - the exact confusion the
// column was added to remove.
func gatesJSON(g map[string]int) string {
	if g == nil {
		return "{}"
	}
	b, err := json.Marshal(g)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gzipText compresses one captured before/after blob. Content is the bulk of the
// database, it is highly repetitive agent transcript text, and it is only ever
// read one request at a time by the diff view — so paying CPU on the writer
// goroutine to keep the file small is the right trade.
func gzipText(s string) []byte {
	if s == "" {
		return nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func gunzipText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return ""
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return ""
	}
	return string(out)
}

// Prune enforces retention by BOTH age and size, in that order: drop everything
// older than maxAge, then — if the file is still over maxBytes — drop the oldest
// requests until it fits. Age alone cannot bound a burst; size alone silently
// erases a quiet week. Content rows and component rows go with their request via
// ON DELETE CASCADE. Returns how many request rows were deleted.
//
// maxAge <= 0 disables the age rule; maxBytes <= 0 disables the size rule.
func (d *DB) Prune(now time.Time, maxAge time.Duration, maxBytes int64) (int64, error) {
	var deleted int64
	if maxAge > 0 {
		cutoff := now.Add(-maxAge).UnixMilli()
		res, err := d.sql.Exec(`DELETE FROM requests WHERE ts < ?`, cutoff)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if maxBytes <= 0 {
		return deleted, nil
	}
	// Size rule. Loop because deleting a slice does not immediately shrink the
	// file (SQLite reuses freed pages), so we bound the work: at most a few
	// rounds, each dropping the oldest 10% of rows, and stop as soon as the
	// estimated payload fits.
	for round := 0; round < 8; round++ {
		size, err := d.sizeBytes()
		if err != nil || size <= maxBytes {
			return deleted, err
		}
		var total int64
		if err := d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*) FROM requests`).Scan(&total); err != nil {
			return deleted, err
		}
		if total == 0 {
			return deleted, nil
		}
		drop := total / 10
		if drop < 1 {
			drop = 1
		}
		// No tenant column, and there cannot be one — a byte budget is a property of
		// the whole file. That is why the per-tenant row quota runs BEFORE this
		// (Recorder.janitorPass): reached while one tenant's traffic is what put the
		// database over budget, this statement evicts the QUIET tenants, because
		// theirs are the oldest rows it selects.
		//
		// Row-granular, deliberately, and this is the ONE rule that is. The byte
		// budget is a hard cap the operator asked to be honoured, and it has to be
		// satisfiable even when everything in the database belongs to a single
		// session — dropping whole sessions there means dropping all of it. The
		// disk-pressure rule (Recorder.relieveDiskPressure) evicts whole sessions
		// instead, because it has a floor to stop at and so never needs a last resort.
		res, err := d.sql.Exec(
			`DELETE FROM requests WHERE id IN (SELECT id FROM requests ORDER BY ts ASC LIMIT ?)`, drop)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
		// Reclaim the pages so the next sizeBytes reflects the deletion.
		if err := d.reclaim(); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// sizeBytes reports the database's payload size. page_count*page_size is exact
// for the main file and works for an in-memory database too (where a stat would
// have nothing to look at).
func (d *DB) sizeBytes() (int64, error) {
	var pages, pageSize int64
	if err := d.sql.QueryRowContext(d.readCtx(), `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := d.sql.QueryRowContext(d.readCtx(), `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	total := pages * pageSize
	// Count the write-ahead log too. It is real disk, it can reach a large fraction
	// of the main file between checkpoints, and a budget that ignores it silently
	// under-counts by however much is currently un-checkpointed.
	if d.path != "" && d.path != ":memory:" {
		if fi, err := os.Stat(d.path + "-wal"); err == nil {
			total += fi.Size()
		}
	}
	return total, nil
}

// reclaim frees deleted pages back to the filesystem. With INCREMENTAL auto-vacuum
// (every database this build creates) it reclaims a bounded batch; on an older file
// with auto_vacuum=NONE, incremental_vacuum is a silent no-op, so a full VACUUM is
// the only thing that shrinks it.
func (d *DB) reclaim() error {
	// Checkpoint FIRST. In WAL mode a delete lands in the log, not the main file, so
	// two things go wrong without this: incremental_vacuum finds no free pages to
	// release (they are not in the main file yet), and sizeBytes — which counts the
	// WAL, because it is real disk — reports the deletion as GROWTH. The size loop
	// then reads its own progress backwards and deletes again.
	d.checkpoint()
	var mode int
	if err := d.sql.QueryRowContext(d.readCtx(), `PRAGMA auto_vacuum`).Scan(&mode); err == nil && mode == 2 {
		if _, err := d.sql.Exec(`PRAGMA incremental_vacuum(2000)`); err != nil {
			return err
		}
	} else if _, err := d.sql.Exec(`VACUUM`); err != nil {
		return err
	}
	// And again, to truncate the WAL the reclaim itself just wrote.
	d.checkpoint()
	return nil
}

// checkpoint folds the write-ahead log into the main database and truncates it.
//
// wal_checkpoint(TRUNCATE) does not error when it cannot run: it returns a row,
// busy/log/checkpointed, where busy=1 means another connection held a read lock and
// the WAL was left untouched. d.sql is a pool shared with every dashboard and
// /metrics reader, so a reader being active is the normal case on a busy deployment
// — and correlated with the write traffic growing the WAL, which is exactly when the
// bloat protection this call exists for is most needed.
//
// The retry is inside the call, not around it: the pool's busy_timeout(5000) makes
// SQLite re-ask for the lock through the busy handler for up to 5 seconds, so an
// outer backoff loop would stack full 5s waits on top of each other (measured: five
// attempts, 25s). What the old code missed is the ROW — busy=1 after that whole
// budget means the bloat protection did not run, and err != nil means the pragma
// itself failed. A reader holding the lock is only the most COMMON cause of the
// first, so the WARN names the fact (the checkpoint did not complete) rather than
// one cause of it, and the attributes say which: a fact worth a log line rather
// than silence — a WAL left to grow made one writer commit take 5.5s and blocked
// /metrics for 5.2s, and nothing pointed here (#166).
func (d *DB) checkpoint() {
	if d.readOnly || d.path == "" || d.path == ":memory:" {
		return
	}
	var busy, logFrames, checkpointed int
	err := d.sql.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	if err == nil && busy == 0 {
		return
	}
	slog.Warn("dash: WAL checkpoint did not complete",
		"busy", busy, "wal_frames", logFrames, "checkpointed", checkpointed, "err", err)
}

// optimize refreshes the query planner's statistics for tables whose statistics are missing or
// stale. Called from the janitor's recurring pass; see janitorPass for the measurement and for
// why it is not done at Open.
//
// The 0x10002 MASK IS LOAD-BEARING and a bare `PRAGMA optimize` here is a silent no-op. 0x00002 is
// "run ANALYZE where it would help"; 0x10000 is what lifts the restriction that a table must have
// been queried BY THIS CONNECTION to be considered. The janitor takes a fresh connection from the
// pool and runs no queries of its own, so under the default mask SQLite correctly concludes this
// connection has used nothing and analyses nothing. Measured on a copy of the production database,
// on a cold connection: bare `PRAGMA optimize` completes in 0.00s and leaves sqlite_stat1 absent;
// `PRAGMA optimize(0x10002)` produces all 26 statistic rows, also in 0.00s.
//
// It is fast because it SAMPLES rather than doing a full scan — which is the whole reason it can
// live on a recurring pass, where a bare ANALYZE (72s on this database) could not. The sampled
// statistics are good enough for the decision that actually matters here, join order: with them the
// component aggregate plans as SEARCH requests USING idx_requests_ts -> SEARCH request_components
// USING idx_rc_request and runs in 0.209s, against 2.0s and a full 1.58M-row scan without them.
//
// Errors are logged rather than returned: out-of-date statistics make queries slower, never wrong,
// so a failure here must not stop the rest of a janitor pass.
func (d *DB) optimize() {
	if d.path == "" || d.path == ":memory:" {
		return
	}
	if _, err := d.sql.Exec(`PRAGMA optimize(0x10002)`); err != nil {
		slog.Warn("dash: PRAGMA optimize failed; query plans will use whatever statistics exist", "err", err)
	}
}

// DropOldestSessions deletes the n least-recently-active SESSIONS and returns how
// many request rows went with them. Component and content rows follow via
// ON DELETE CASCADE.
//
// Session granularity, not row granularity, and that is the whole point: evicting
// the oldest individual requests tears conversations in half, so the dashboard shows
// a session whose first turns have vanished and whose totals no longer add up. A
// session is the unit a user reasons about, so it is the unit that disappears.
//
// "Oldest" is MAX(ts) per session — last activity, not first. A long-running session
// that is still in use must not be evicted because it started a week ago.
//
// The victims are picked ONCE, and the delete removes exactly that set (#772). The
// delete used to re-run the ordering subquery itself, so a request landing between the
// pick and the delete could change the order: the rows it removed were then not the rows
// the state cleanup acted on, and a session that stayed lost its state to one that went.
// The delete runs in chunks of sqlChunk ids, each chunk one statement and so one
// transaction. A failed chunk returns before the state cleanup, so victims the earlier
// chunks already removed keep their state until retention — the safe direction. The pick
// stays outside the deletes, like every other read
// here (ArchiveSessionFull reads its events outside its delete too): a read-then-write
// transaction on this shared pool can fail to upgrade its WAL snapshot when the
// session-state flush commits in between.
func (d *DB) DropOldestSessions(n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	// Before the delete: afterwards the victims cannot be found. A failed pick returns its
	// error rather than reading as an empty table (#775 review).
	victims, err := d.oldestSessionIDs(n)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	if d.dropProbe != nil {
		d.dropProbe()
	}
	// The aliases are columns of the rows about to go, so they are read before the delete.
	aliases, err := d.sessionAliases(victims...)
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, chunk := range chunkStrings(victims) {
		res, err := d.sql.Exec(`DELETE FROM requests WHERE session_id IN (`+placeholders(len(chunk))+`)`,
			anyArgs(chunk)...)
		if err != nil {
			return affected, err
		}
		n, _ := res.RowsAffected()
		affected += n
	}
	d.deleteSessionState(victims, aliases)
	return affected, nil
}

func (d *DB) oldestSessionIDs(n int) ([]string, error) {
	rows, err := d.sql.Query(`SELECT session_id FROM requests GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

func anyArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// sqlChunk caps how many ids one IN (...) list binds. SQLite caps a statement's variables
// (32,766 in this driver), and the disk rule can ask for a tenth of all sessions at once, so
// the lists go in batches instead. A var, not a const, so a test can shrink it and exercise
// the batch boundaries.
var sqlChunk = 500

// chunkStrings splits ss into sqlChunk-sized batches, keeping every element and its order.
func chunkStrings(ss []string) [][]string {
	var out [][]string
	for len(ss) > sqlChunk {
		out = append(out, ss[:sqlChunk])
		ss = ss[sqlChunk:]
	}
	if len(ss) > 0 {
		out = append(out, ss)
	}
	return out
}

// TenantRowCounts reports how many request rows each tenant holds. The caller
// compares each against that tenant's own quota, which is why this does not filter:
// the quota is per tenant now (a manager sets it), so there is no single threshold to
// push into the query.
//
// Fairness before scarcity: without this, the global disk rule lets one heavy user's
// traffic evict everyone else's history, which is the shared-service failure where the
// person causing the problem is the last to notice it.
func (d *DB) TenantRowCounts() (map[string]int64, error) {
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT tenant_id, COUNT(*) c FROM requests
		WHERE tenant_id <> '' GROUP BY tenant_id ORDER BY c DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// tenantRowCount counts one tenant's request rows.
func (d *DB) tenantRowCount(tenant string) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*) FROM requests WHERE tenant_id = ?`, tenant).Scan(&n)
	return n, err
}

// DropOldestSessionsOfTenant trims one tenant back toward its quota, oldest session
// first, and returns the rows deleted. DESTRUCTIVE, and only correct when there is no
// cold storage to migrate to — Recorder.trimTenantToQuota picks between the two. The
// dropped sessions' state rows go with them, exactly as in DropOldestSessions (#772).
func (d *DB) DropOldestSessionsOfTenant(tenant string, targetRows int64) (int64, error) {
	var deleted int64
	// Bounded: each pass drops one session, and a tenant with a pathological number
	// of tiny sessions must not hold the writer goroutine for an unbounded time.
	for pass := 0; pass < 64; pass++ {
		n, err := d.tenantRowCount(tenant)
		if err != nil {
			return deleted, err
		}
		if n <= targetRows {
			return deleted, nil
		}
		// The victim is picked first and deleted by id, so the state cleanup below acts on
		// the session that actually went (#772) — the delete no longer re-picks it itself.
		var id string
		err = d.sql.QueryRowContext(d.readCtx(), `SELECT session_id FROM requests WHERE tenant_id = ?
			GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT 1`, tenant).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return deleted, nil
		}
		if err != nil {
			return deleted, err
		}
		aliases, err := d.sessionAliases(id)
		if err != nil {
			return deleted, err
		}
		res, err := d.sql.Exec(`DELETE FROM requests WHERE tenant_id = ? AND session_id = ?`, tenant, id)
		if err != nil {
			return deleted, err
		}
		got, _ := res.RowsAffected()
		if got == 0 {
			return deleted, nil // nothing left to drop; avoid spinning
		}
		d.deleteSessionState([]string{id}, aliases)
		deleted += got
	}
	return deleted, nil
}

// setBypassTokens stores the token count of a bypassed request after the fact. A bypassed row
// is forwarded untouched, so after equals before. Priced with the count still unknown, such a
// row is labelled "missing"; with a count it is "partial", exactly as when it was counted inline.
func (d *DB) setBypassTokens(id int64, before, attempted, frozen int) error {
	_, err := d.sql.Exec(`UPDATE requests SET tokens_before = ?, tokens_after = ?, attempted_tokens = ?,
		frozen_tokens = ?,
		token_accounting = CASE WHEN token_accounting = ? AND ? > 0 THEN ? ELSE token_accounting END
		WHERE id = ?`, before, before, attempted, frozen, AccountingMissing, before, AccountingPartial, id)
	return err
}
