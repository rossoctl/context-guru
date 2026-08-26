package dash

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
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

	_ "modernc.org/sqlite" // pure-Go driver: no extra C toolchain for the dashboard
)

// memSeq names in-memory databases uniquely. See Open.
var memSeq atomic.Uint64

// DB is the dashboard's durable store. All writes go through one goroutine (see
// capture.go), so the only concurrency here is reads racing that writer, which
// SQLite in WAL mode handles.
type DB struct {
	sql  *sql.DB
	path string // "" for the in-memory store
	// ctx bounds the READS made through this handle, so a request the caller has abandoned stops
	// costing the process. nil means context.Background().
	//
	// Carried on a shallow copy (see WithContext) rather than added as a parameter to every
	// query method, which would have meant a context argument on fifty call sites, most of them
	// tests, for a value only the HTTP layer can supply. The struct is two fields and holds no
	// lock, so copying it is safe; `go vet` would say otherwise if it did.
	ctx context.Context
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
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=auto_vacuum(2)"
}

func openDSN(d, path string) (*DB, error) {
	sdb, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, err
	}
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, err
	}
	if err := migrate(sdb); err != nil {
		sdb.Close()
		return nil, err
	}
	return &DB{sql: sdb, path: path}, nil
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

// Path returns the on-disk path ("" when in-memory).
func (d *DB) Path() string { return d.path }

// insertBatch writes a batch of captured events in ONE transaction — the whole
// point of batching: a per-request fsync would make the writer the bottleneck
// under agent traffic. A failed batch is logged and dropped; observability never
// retries into a growing backlog.
func (d *DB) insertBatch(evs []*Event) error {
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
		split_stable_tokens, split_tail_hash, filtered_decl_tokens,
		cache_ttl, sse_buffered, ttfb_ms,
		cache_write_1h, keepalive, keepalive_pings, keepalive_saved_usd, cg_latency_ms, upstream_ms,
		expands, expand_tokens, reverts, token_accounting, cache_miss_reason, uncompressed_reason,
		reasoning_effort, thinking_mode, thinking_budget, temperature, top_p, max_tokens, stream,
		tool_choice, tools, system_blocks,
		cache_bp_system, cache_bp_tools, cache_bp_messages, cache_bp_blocks, stop_reason,
		keepalive_strategy_id
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer reqStmt.Close()
	compStmt, err := tx.Prepare(`INSERT INTO request_components(
		request_id, component, kind, acted, mutated, reverted, skipped, saved_gross, saved_unique,
		saved_usd, duration_ms, err, gates
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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

	spend := map[spendKey]float64{}
	for _, e := range evs {
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
			e.SplitStableTokens, int64(e.SplitTailHash), e.FilteredDeclTokens,
			e.CacheTTL, boolInt(e.SSEBuffered), e.TTFBMs,
			e.CacheWrite1h, boolInt(e.KeepAlive), e.KeepAlivePings, e.KeepAliveSavedUSD,
			e.CGLatencyMs, e.UpstreamMs,
			e.Expands, e.ExpandTokens, e.Reverts, e.TokenAccounting, e.CacheMissReason, e.UncompressedReason,
			e.ReasoningEffort, e.ThinkingMode, e.ThinkingBudget, e.Temperature, e.TopP, e.MaxTokens,
			boolInt(e.Stream), e.ToolChoice, e.Tools, e.SystemBlocks,
			e.CacheBPSystem, e.CacheBPTools, e.CacheBPMessages, e.CacheBPBlocks, e.StopReason,
			strategyID,
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		e.ID = id
		for _, c := range e.Components {
			if _, err := compStmt.Exec(id, c.Component, c.Kind,
				boolInt(c.Acted), boolInt(c.Mutated), boolInt(c.Reverted), boolInt(c.Skipped),
				c.SavedGross, c.SavedUnique, c.SavedUSD, c.DurationMs, c.Err, gatesJSON(c.Gates)); err != nil {
				return err
			}
		}
		for i, c := range e.Content {
			if _, err := contentStmt.Exec(id, i, c.Path, c.BeforeTokens, c.AfterTokens,
				gzipText(c.Before), gzipText(c.After), strings.Join(c.Components, ",")); err != nil {
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
				gzipText(x.Before), gzipText(x.After)); err != nil {
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
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&total); err != nil {
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
	if err := d.sql.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := d.sql.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
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
	if err := d.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err == nil && mode == 2 {
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
// Best-effort: a busy checkpoint is not a failure, only a deferral.
func (d *DB) checkpoint() {
	if d.path == "" || d.path == ":memory:" {
		return
	}
	_, _ = d.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
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
func (d *DB) DropOldestSessions(n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	res, err := d.sql.Exec(`DELETE FROM requests WHERE session_id IN (
		SELECT session_id FROM requests GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT ?)`, n)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
	rows, err := d.sql.Query(`SELECT tenant_id, COUNT(*) c FROM requests
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
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE tenant_id = ?`, tenant).Scan(&n)
	return n, err
}

// DropOldestSessionsOfTenant trims one tenant back toward its quota, oldest session
// first, and returns the rows deleted. DESTRUCTIVE, and only correct when there is no
// cold storage to migrate to — Recorder.trimTenantToQuota picks between the two.
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
		res, err := d.sql.Exec(`DELETE FROM requests WHERE tenant_id = ? AND session_id = (
			SELECT session_id FROM requests WHERE tenant_id = ?
			GROUP BY session_id ORDER BY MAX(ts) ASC LIMIT 1)`, tenant, tenant)
		if err != nil {
			return deleted, err
		}
		got, _ := res.RowsAffected()
		if got == 0 {
			return deleted, nil // nothing left to drop; avoid spinning
		}
		deleted += got
	}
	return deleted, nil
}
