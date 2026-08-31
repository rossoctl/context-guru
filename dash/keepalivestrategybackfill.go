package dash

// The one-time backfill that recovers keepalive_strategy_id on REAL rows written before
// dashcapture.go learned to tag them (2026-08-30). It was reported as "every strategy shows
// $0 Saved, including last week's" — which turned out to be true for exactly the rows this
// file fixes, and only those: dash/keepalivestrategy.go's StrategyLedger filters on
// keepalive_saved_usd > 0 AND keepalive_strategy_id = ?, and a real row credited before the
// real-row tagging code existed has NULL there, permanently, with no way for the live code to
// ever set it after the fact — the moment arrive() had the strategy in scope is long gone.
//
// The information is not lost, though: the PING rows that rescued those real rows were
// already tagged (this half of the feature is older, PR #114), and a real row's own
// keepalive_pings > 0 says a ping preceded it. So for a real row missing the tag, the ping(s)
// that immediately preceded it in the SAME session — after that session's own previous real
// row, so this never reaches into an EARLIER, already-consumed idle span — carry exactly the
// strategy id arrive() would have reported at the time, had this code existed then. Measured
// on the live corpus: 109 of 130 such rows recover a tag this way; the other 21 have no
// matching tagged ping at all, which means the rescue came from plain account config or a
// session override with no strategy involved — correctly left NULL, not guessed at.
//
// NOTHING IS DELETED and nothing already tagged is touched — this only ever moves a NULL to a
// value it can prove from data the table already holds. Same shape as dedupetext.go's
// migration: a predicate over the data, so it is naturally idempotent and resumable, with a
// meta marker recording it has run so a restart does not repeat a full-table predicate check
// once nothing new could ever match it (new rescues are tagged directly at write time now;
// this file has no ongoing job once the historical gap is closed).

import (
	"database/sql"
	"log/slog"
	"time"
)

// keepAliveStrategyBackfillDoneKey is the `meta` row this backfill sets once it has swept the
// whole table and found nothing left to recover.
const keepAliveStrategyBackfillDoneKey = "keepalive_strategy_backfill_done"

// keepAliveStrategyBackfillBatch bounds how many candidate rows one pass considers. The live
// corpus this was measured against had 130 total candidates ever — this is not a table-sized
// migration, but it still batches and pauses like one, so a future deployment with a much
// larger backlog of idle real rows behaves the same way dedupetext.go's migration does rather
// than assuming today's small scale forever.
const keepAliveStrategyBackfillBatch = 500

// keepAliveStrategyBackfillPause is the gap between batches, so the capture writer always has
// the lock available and this cannot starve it even on a much larger backlog.
const keepAliveStrategyBackfillPause = 200 * time.Millisecond

// keepAliveStrategyBackfillLoop runs the recovery sweep once. Called from
// backgroundMigrations (capture.go), not given its own goroutine — see that function's
// comment for why every one-time migration shares one.
func (r *Recorder) keepAliveStrategyBackfillLoop() {
	if _, err := r.db.backfillKeepAliveStrategyID(r.done, keepAliveStrategyBackfillBatch, keepAliveStrategyBackfillPause); err != nil {
		// A partial run is readable and resumable, so this is a warning, not a fatal: the
		// rows it did not reach stay exactly as informative as they were before this ran.
		slog.Warn("dash: keepalive-strategy backfill stopped early", "err", err)
	}
}

// backfillKeepAliveStrategyID recovers keepalive_strategy_id on real rows a ping rescued
// before the real-row tagging code existed. Returns the number of rows it filled. Stops early
// and without error when stop is closed, exactly like dedupeDeclarationText — a partial run is
// a valid state, and the next start continues from the data, not from any cursor of its own.
func (d *DB) backfillKeepAliveStrategyID(stop <-chan struct{}, batch int, pause time.Duration) (int64, error) {
	if batch <= 0 {
		batch = keepAliveStrategyBackfillBatch
	}
	if done, err := d.keepAliveStrategyBackfillDone(); err != nil {
		slog.Warn("dash: could not read the keepalive-strategy-backfill marker; sweeping as if it were unset", "err", err)
	} else if done {
		return 0, nil
	}
	var cursor, moved int64
	started := time.Now()
	for {
		select {
		case <-stop:
			return moved, nil
		default:
		}
		ids, err := d.pendingKeepAliveStrategyBackfill(cursor, batch)
		if err != nil {
			return moved, err
		}
		if len(ids) == 0 {
			if merr := d.markKeepAliveStrategyBackfillDone(); merr != nil {
				slog.Warn("dash: could not persist the keepalive-strategy-backfill marker; "+
					"the next restart will re-sweep to confirm nothing is left", "err", merr)
			}
			break
		}
		cursor = ids[len(ids)-1]
		n, err := d.backfillOneKeepAliveStrategyBatch(ids)
		moved += n
		if err != nil {
			return moved, err
		}
		if pause > 0 {
			select {
			case <-stop:
				return moved, nil
			case <-time.After(pause):
			}
		}
	}
	if moved > 0 {
		slog.Info("dash: recovered keepalive_strategy_id on real rows a ping rescued before the real-row tagging code existed",
			"rows", moved, "took", time.Since(started).Round(time.Millisecond))
	}
	return moved, nil
}

// pendingKeepAliveStrategyBackfill finds the next batch of real rows a ping rescued
// (keepalive_pings > 0) that have never been tagged with the strategy that sent the ping.
// id is the table's own rowid, so `id > ?` is a seek rather than a rescan of what came before.
func (d *DB) pendingKeepAliveStrategyBackfill(after int64, limit int) ([]int64, error) {
	rows, err := d.sql.Query(`SELECT id FROM requests
		WHERE id > ? AND keepalive = 0 AND keepalive_pings > 0 AND keepalive_strategy_id IS NULL
		ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// backfillOneKeepAliveStrategyBatch fills in keepalive_strategy_id for exactly the rows named,
// each from the strategy id of the ping(s) that immediately preceded it in the SAME session —
// after that session's own previous real row, which is what keeps this from reaching into an
// earlier, already-consumed idle span. A row with no matching tagged ping (plain account
// config or a session override rescued it, never a strategy) is left NULL, not guessed at.
func (d *DB) backfillOneKeepAliveStrategyBatch(ids []int64) (int64, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	// The matching-ping predicate is repeated in an EXISTS rather than trusted to the SET
	// subquery alone: SQLite counts a row as affected once the UPDATE touches it, even when
	// the value it writes is the same NULL that was already there — a row with no matching
	// tagged ping would otherwise report as "moved" while staying NULL, overstating how much
	// this recovered.
	stmt, err := tx.Prepare(`UPDATE requests SET keepalive_strategy_id = (
			SELECT p.keepalive_strategy_id FROM requests p
			WHERE p.tenant_id = requests.tenant_id AND p.session_id = requests.session_id
			  AND p.keepalive = 1 AND p.keepalive_strategy_id IS NOT NULL
			  AND p.ts <= requests.ts
			  AND p.ts > COALESCE((
			      SELECT MAX(q.ts) FROM requests q
			      WHERE q.tenant_id = requests.tenant_id AND q.session_id = requests.session_id
			        AND q.keepalive = 0
			        AND (q.ts < requests.ts OR (q.ts = requests.ts AND q.id < requests.id))
			  ), 0)
			ORDER BY p.ts DESC LIMIT 1
		)
		WHERE id = ? AND keepalive_strategy_id IS NULL
		  AND EXISTS (
			SELECT 1 FROM requests p
			WHERE p.tenant_id = requests.tenant_id AND p.session_id = requests.session_id
			  AND p.keepalive = 1 AND p.keepalive_strategy_id IS NOT NULL
			  AND p.ts <= requests.ts
			  AND p.ts > COALESCE((
			      SELECT MAX(q.ts) FROM requests q
			      WHERE q.tenant_id = requests.tenant_id AND q.session_id = requests.session_id
			        AND q.keepalive = 0
			        AND (q.ts < requests.ts OR (q.ts = requests.ts AND q.id < requests.id))
			  ), 0)
		  )`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var moved int64
	for _, id := range ids {
		res, err := stmt.Exec(id)
		if err != nil {
			return moved, err
		}
		if n, err := res.RowsAffected(); err == nil {
			moved += n
		}
	}
	return moved, tx.Commit()
}

// keepAliveStrategyBackfillDone reports whether a previous run already swept the whole table
// and found nothing left to recover. Best read as "no" on error, matching dedupetext.go's own
// marker — a mis-read should cost one redundant sweep, not skip a backfill that has real,
// recoverable rows.
func (d *DB) keepAliveStrategyBackfillDone() (bool, error) {
	var v string
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key = ?`, keepAliveStrategyBackfillDoneKey).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// markKeepAliveStrategyBackfillDone persists that a sweep found nothing left to recover, so
// the next process start can skip straight past the predicate check.
func (d *DB) markKeepAliveStrategyBackfillDone() error {
	_, err := d.sql.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, keepAliveStrategyBackfillDoneKey)
	return err
}
