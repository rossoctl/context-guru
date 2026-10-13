package dash

// The one-time backfill that recovers daily_rollup for requests rows written before #502's
// table existed. Without this, a deployment that already had history would see its lean
// lifetime read (dash/overview_lean.go) answer only from the day this shipped onward — the
// right NUMBER would be wrong, not merely late, which is worse than the slow query it replaces.
//
// Same shape as dash/keepalivestrategybackfill.go: a predicate over the data (id > cursor),
// naturally idempotent and resumable, batched and paused so it never contends the capture
// writer's lock for long, with a meta marker so a restart does not re-scan a table nothing new
// could ever match once the backfill has reached the end.

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// dailyRollupBackfillDoneKey is the `meta` row this backfill sets once it has summed every
// request row written before daily_rollup existed into their (tenant, day) buckets.
const dailyRollupBackfillDoneKey = "daily_rollup_backfill_done"

// dailyRollupLiveFromIDKey is the `meta` row dash/store.go's insertBatch writes once, ever,
// recording the first request id this database ever live-rolled-up into daily_rollup. The
// backfill below must never touch an id at or above it — those rows were already counted, in
// the SAME transaction, by insertBatch's own loop; re-summing them here is exactly how a
// brand-new database (nothing BUT live-rolled-up rows, nothing to backfill at all) got every
// number in daily_rollup doubled before this watermark existed.
const dailyRollupLiveFromIDKey = "daily_rollup_live_from_id"

// dailyRollupBackfillBatch bounds how many requests rows one pass sums. Larger than
// keepAliveStrategyBackfillBatch's 500: this is one GROUP BY over a contiguous id range, not
// one UPDATE per candidate row, so the per-batch cost is one aggregate query, not `batch`
// prepared-statement executions.
const dailyRollupBackfillBatch = 20_000

// dailyRollupBackfillPause is the gap between batches — see
// keepAliveStrategyBackfillPause's own comment; same reason, same shared goroutine.
const dailyRollupBackfillPause = 200 * time.Millisecond

// dailyRollupBackfillLoop runs the one-time sum. Called from backgroundMigrations
// (dash/capture.go), on the shared migrations goroutine, after the other two: this is the
// only one of the three whose batches can be large on a deployment with real history, so it
// runs last, after the smaller, faster ones have already finished.
func (r *Recorder) dailyRollupBackfillLoop() {
	if _, err := r.db.backfillDailyRollup(r.done, dailyRollupBackfillBatch, dailyRollupBackfillPause); err != nil {
		// A partial run is resumable, exactly like the backfill above it: the next start
		// picks up from the last id this one reached, not from a cursor of its own.
		slog.Warn("dash: daily-rollup backfill stopped early", "err", err)
	}
}

// backfillDailyRollup sums requests rows into daily_rollup, oldest id first, until none are
// left or stop closes. Returns the number of rows it accounted for.
func (d *DB) backfillDailyRollup(stop <-chan struct{}, batch int, pause time.Duration) (int64, error) {
	if batch <= 0 {
		batch = dailyRollupBackfillBatch
	}
	if done, err := d.dailyRollupBackfillDone(); err != nil {
		slog.Warn("dash: could not read the daily-rollup-backfill marker; sweeping as if it were unset", "err", err)
	} else if done {
		return 0, nil
	}
	// The ceiling is dailyRollupLiveFromIDKey minus one, NOT maxRequestID(): every id at or
	// above that watermark was already folded into daily_rollup by insertBatch's own live
	// rollup, in the SAME transaction as its own insert (dash/store.go). Using maxRequestID()
	// here — the ids rows LIVE-ROLLED-UP are already a subset of — summed every one of them a
	// SECOND time: a brand-new database, where every row goes through the live path and
	// literally nothing predates daily_rollup, got every number in it exactly doubled. ids
	// only grow, so a watermark read once is never stale: nothing the backfill will ever see
	// can cross it later.
	liveFrom, err := d.dailyRollupLiveFromID()
	if err != nil {
		return 0, err
	}
	if liveFrom == 0 {
		// Nobody has set the watermark yet — meaning no insertBatch call has EVER run on this
		// database since daily_rollup started existing. CLAIM one, racing insertBatch's own
		// attempt to set the SAME key (ON CONFLICT DO NOTHING on both sides): whichever of the
		// two gets there first wins, and the loser's write is a no-op that changes nothing,
		// because insertBatch's own live rollup never reads this key — only the backfill does.
		// Reading maxRequestID() and claiming maxID+1 BEFORE summing anything (rather than
		// summing up to maxRequestID() directly) is what closes the startup race: a row
		// inserted between this read and the claim lands ABOVE the claimed watermark, so it is
		// correctly left for the live path even though this backfill pass could not have seen
		// it yet.
		maxID, err := d.maxRequestID()
		if err != nil {
			return 0, err
		}
		if _, err := d.sql.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`,
			dailyRollupLiveFromIDKey, fmt.Sprint(maxID+1)); err != nil {
			return 0, err
		}
		liveFrom, err = d.dailyRollupLiveFromID()
		if err != nil {
			return 0, err
		}
	}
	ceiling := liveFrom - 1
	var cursor, moved int64
	started := time.Now()
	for cursor < ceiling {
		select {
		case <-stop:
			return moved, nil
		default:
		}
		upto := cursor + int64(batch)
		if upto > ceiling {
			upto = ceiling
		}
		n, err := d.backfillOneDailyRollupBatch(cursor, upto)
		if err != nil {
			return moved, err
		}
		moved += n
		cursor = upto // advance past the whole range even if n==0 (a gap of evicted rows)
		if pause > 0 {
			select {
			case <-stop:
				return moved, nil
			case <-time.After(pause):
			}
		}
	}
	if merr := d.markDailyRollupBackfillDone(); merr != nil {
		slog.Warn("dash: could not persist the daily-rollup-backfill marker; "+
			"the next restart will re-sweep to confirm nothing is left", "err", merr)
	}
	if moved > 0 {
		slog.Info("dash: backfilled daily_rollup from requests written before it existed",
			"rows", moved, "took", time.Since(started).Round(time.Millisecond))
	}
	return moved, nil
}

// maxRequestID is the id this backfill treats as "everything up to here is history, not a
// row insertBatch's own live rollup update might also be counting right now".
func (d *DB) maxRequestID() (int64, error) {
	var max sql.NullInt64
	if err := d.sql.QueryRowContext(d.readCtx(), `SELECT MAX(id) FROM requests`).Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64, nil // NULL (empty table) scans as 0, which is the correct "nothing to do"
}

// backfillOneDailyRollupBatch sums one id range (after, upto] of requests, grouped by
// tenant/day/keepalive exactly like addToDailyRollup splits a live batch, and upserts the
// result. Returns how many rows it summed — 0 is a valid answer (every id in range was
// evicted since), and the caller advances its cursor to `upto` regardless, so a gap can never
// stall it.
func (d *DB) backfillOneDailyRollupBatch(after, upto int64) (int64, error) {
	// kaSaved("r."), not a bare `keepalive_saved_usd` SUM: that column alone overcounts, the
	// same way addToDailyRollup's own raw column would (see insertBatch's #502 comment) —
	// kaSaved is the SQL form of the same EXISTS gate reachableKeepAliveSavedUSD runs per row
	// at write time, so the backfill must apply it too or the two paths would disagree on
	// history written before this table existed.
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT r.tenant_id, strftime('%Y-%m-%d', r.ts/1000, 'unixepoch'),
			r.keepalive,
			COUNT(*), COALESCE(SUM(r.tokens_before),0), COALESCE(SUM(r.tokens_after),0), COALESCE(SUM(r.saved_unique),0),
			COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
			COALESCE(SUM(r.cache_saved_usd),0), COALESCE(SUM(r.cachesplit_saved_usd),0),
			COALESCE(SUM(`+kaSaved("r.")+`),0), COALESCE(SUM(CASE WHEN `+kaSaved("r.")+` > 0 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.upstream_ms > 0 THEN r.upstream_ms ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.upstream_ms > 0 THEN 1 ELSE 0 END),0)
		FROM requests r WHERE r.id > ? AND r.id <= ?
		GROUP BY 1, 2, 3`, after, upto)
	if err != nil {
		return 0, err
	}
	type bucket struct {
		tenant, day       string
		keepalive         bool
		n                 int64
		tb, ta, su        int64
		cost, base, cgllm float64
		cacheSaved, split float64
		kaSaved           float64
		kaMissed          int64
		upSum             float64
		upCount           int64
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		var ka int
		if err := rows.Scan(&b.tenant, &b.day, &ka, &b.n, &b.tb, &b.ta, &b.su,
			&b.cost, &b.base, &b.cgllm, &b.cacheSaved, &b.split, &b.kaSaved, &b.kaMissed,
			&b.upSum, &b.upCount); err != nil {
			rows.Close()
			return 0, err
		}
		b.keepalive = ka != 0
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	if len(buckets) == 0 {
		return 0, nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	var total int64
	for _, b := range buckets {
		total += b.n
		if b.keepalive {
			if _, err := tx.Exec(`INSERT INTO daily_rollup (tenant_id, day, keepalive_pings, keepalive_ping_usd)
				VALUES (?,?,?,?)
				ON CONFLICT(tenant_id, day) DO UPDATE SET
					keepalive_pings = keepalive_pings + excluded.keepalive_pings,
					keepalive_ping_usd = keepalive_ping_usd + excluded.keepalive_ping_usd`,
				b.tenant, b.day, b.n, b.cost); err != nil {
				return total, err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO daily_rollup (
				tenant_id, day, requests, tokens_before, tokens_after, saved_unique,
				cost_usd, baseline_cost_usd, cg_llm_cost_usd, cache_saved_usd, cachesplit_saved_usd,
				keepalive_saved_usd, keepalive_misses_avoided, upstream_ms_sum, upstream_ms_count
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(tenant_id, day) DO UPDATE SET
				requests = requests + excluded.requests,
				tokens_before = tokens_before + excluded.tokens_before,
				tokens_after = tokens_after + excluded.tokens_after,
				saved_unique = saved_unique + excluded.saved_unique,
				cost_usd = cost_usd + excluded.cost_usd,
				baseline_cost_usd = baseline_cost_usd + excluded.baseline_cost_usd,
				cg_llm_cost_usd = cg_llm_cost_usd + excluded.cg_llm_cost_usd,
				cache_saved_usd = cache_saved_usd + excluded.cache_saved_usd,
				cachesplit_saved_usd = cachesplit_saved_usd + excluded.cachesplit_saved_usd,
				keepalive_saved_usd = keepalive_saved_usd + excluded.keepalive_saved_usd,
				keepalive_misses_avoided = keepalive_misses_avoided + excluded.keepalive_misses_avoided,
				upstream_ms_sum = upstream_ms_sum + excluded.upstream_ms_sum,
				upstream_ms_count = upstream_ms_count + excluded.upstream_ms_count`,
			b.tenant, b.day, b.n, b.tb, b.ta, b.su, b.cost, b.base, b.cgllm, b.cacheSaved, b.split,
			b.kaSaved, b.kaMissed, b.upSum, b.upCount); err != nil {
			return total, err
		}
	}
	return total, tx.Commit()
}

// dailyRollupBackfillDone reports whether a previous run already summed every row that
// existed before daily_rollup did.
func (d *DB) dailyRollupBackfillDone() (bool, error) {
	var v string
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT value FROM meta WHERE key = ?`, dailyRollupBackfillDoneKey).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// markDailyRollupBackfillDone persists that the backfill reached the end of the table.
func (d *DB) markDailyRollupBackfillDone() error {
	_, err := d.sql.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, dailyRollupBackfillDoneKey)
	return err
}

// dailyRollupLiveFromID reads dailyRollupLiveFromIDKey — the first request id ever
// live-rolled-up on this database (dash/store.go's insertBatch) — or 0 if nothing has set it
// yet (no insertBatch call has run since daily_rollup started existing).
func (d *DB) dailyRollupLiveFromID() (int64, error) {
	var v string
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT value FROM meta WHERE key = ?`, dailyRollupLiveFromIDKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var id int64
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		return 0, err
	}
	return id, nil
}
