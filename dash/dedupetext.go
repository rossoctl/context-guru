package dash

// The one-time migration that collapses duplicated prompt text onto declaration_text.
//
// Why a backfill at all, when the new write path already stores each blob once: the rows
// that duplication already produced are the 254 MiB. Measured on the live database —
// 488,324 tool_declarations rows, 328,236 carrying text, 225 distinct blobs among them —
// so nothing here can be recovered by waiting.
//
// Everything about this file is shaped by two constraints the owner set, in this order:
//
//   - NOTHING IS DELETED. The blob on the declaration row is cleared only after the same text
//     is committed to declaration_text AND read back and compared. The old text_gz
//     COLUMN stays in place (dropping a column rewrites the table in SQLite), unread by the
//     writer, still read by the reader for whatever this has not reached.
//   - IT RUNS AGAINST A LIVE SERVICE. So: bounded batches, a pause between them, the
//     recorder's own done channel checked every batch, and no state of its own — the work
//     left is a predicate over the data (`text_gz IS NOT NULL`), which is what makes it
//     both resumable after a crash and idempotent on a second run.
//
// The interruption story, which is the part worth checking rather than trusting: each batch
// is two transactions, and every possible stopping point leaves the text readable.
//
//	before batch      text_gz holds it, text_hash NULL          -> read via COALESCE
//	after tx 1        both hold it, text_hash set               -> read via the join
//	after tx 2        declaration_text holds it, text_gz NULL   -> read via the join
//
// The middle state is the reason tx 1 does not clear the blob: a crash there costs one
// re-scan of that batch, not a lost declaration.

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"
)

// Backfill pacing. Deliberately unhurried: this reclaims bytes on a service whose job is
// proxying, and the whole 328k rows finish inside a few minutes even at this rate.
const (
	// dedupeBatch is how many text-carrying rows one pass moves. 500 rows is ~270 KB of
	// blob per transaction — a write lock held for milliseconds, well inside the
	// busy_timeout(5000) the capture writer would wait out anyway.
	dedupeBatch = 500
	// dedupePause is the gap between batches, so the capture writer always has the lock
	// available and the migration cannot starve it.
	dedupePause = 200 * time.Millisecond
	// dedupeReclaimPasses bounds how many incremental_vacuum rounds run at the end, as a
	// backstop only: the loop stops as soon as a round frees nothing. Each round releases up
	// to 2,000 pages, so 400 of them is ~3 GB at the 4 KB page size.
	dedupeReclaimPasses = 400
)

// dedupeDeclarationText moves every per-row prompt blob into declaration_text and points the
// row at it. Returns the number of rows migrated. It stops early and without error when stop
// is closed, because a partial run is a valid state — the next start continues from the data.
func (d *DB) dedupeDeclarationText(stop <-chan struct{}, batch int, pause time.Duration) (int64, error) {
	if batch <= 0 {
		batch = dedupeBatch
	}
	// A rowid cursor within the run. The predicate alone would be correct — and is what makes
	// this resumable and idempotent — but re-issuing it each pass re-walks every row already
	// migrated, which over 488k rows is quadratic. A partial index on the predicate was the
	// other way to fix that and was measured worse: building it costs 50 seconds inside Open()
	// on a production-sized file, i.e. 50 seconds of proxy startup, to save a scan that happens
	// on this background goroutine. The cursor resets to 0 on the next process, which costs one
	// scan (~3s, measured, off the request path) to conclude there is nothing left to do.
	var cursor, moved int64
	started := time.Now()
	for {
		select {
		case <-stop:
			return moved, nil
		default:
		}
		rows, err := d.pendingDedupe(cursor, batch)
		if err != nil {
			return moved, err
		}
		if len(rows) == 0 {
			break
		}
		cursor = rows[len(rows)-1].rowid
		n, err := d.dedupeOneBatch(rows)
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
		slog.Info("dash: deduplicated stored prompt text", "rows", moved,
			"took", time.Since(started).Round(time.Millisecond))
	}
	return moved, nil
}

// declBlob is one row of work: where it lives and what it holds.
type declBlob struct {
	rowid int64
	gz    []byte
}

// pendingDedupe reads the next batch of rows whose text is still on the row itself. rowid is
// the table's own key, so `rowid > ?` is a seek rather than a scan of what came before it.
func (d *DB) pendingDedupe(after int64, limit int) ([]declBlob, error) {
	rows, err := d.sql.Query(`SELECT rowid, text_gz FROM tool_declarations
		WHERE rowid > ? AND text_gz IS NOT NULL ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []declBlob
	for rows.Next() {
		var b declBlob
		if err := rows.Scan(&b.rowid, &b.gz); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// dedupeOneBatch stores the batch's distinct blobs and then, only for the rows it can prove
// are stored, clears the per-row copy.
//
// Two transactions, in this order, is the whole safety argument — see the file comment. The
// second one re-reads what the first wrote and compares the TEXT, rather than trusting that the
// insert it just issued did what it said: this is the one place in the package that removes
// data, and "the row is there" is a weaker fact than "the row is there and reads the same".
func (d *DB) dedupeOneBatch(batch []declBlob) (int64, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	put, err := tx.Prepare(`INSERT INTO declaration_text(hash, text_gz, ts)
		VALUES (?,?,0) ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer put.Close()
	point, err := tx.Prepare(`UPDATE tool_declarations SET text_hash = ? WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer point.Close()
	hashes := make([]string, len(batch))
	texts := make([]string, len(batch))
	for i, b := range batch {
		// Keyed by the hash of the TEXT, which is what declText keys new writes by: one
		// convention in the table, so a blob stored by the writer and the same blob found by
		// this backfill land on one row. The alternative — hashing the gzip bytes, which needs
		// no decompression — would make the key an identity of an ENCODING, so two runs of a
		// different compressor over the same schema would file it twice.
		texts[i] = gunzipText(b.gz)
		if texts[i] == "" {
			// An unreadable blob (gunzipText cannot distinguish empty from corrupt, and the
			// writer never stores an empty one). Leave it exactly where it is: hashing "" would
			// collapse every such row onto one key and then pass verification, which is how a
			// migration turns unreadable bytes into deleted bytes.
			continue
		}
		sum := sha256.Sum256([]byte(texts[i]))
		hashes[i] = hex.EncodeToString(sum[:])
		if _, err := put.Exec(hashes[i], b.gz); err != nil {
			return 0, err
		}
		if _, err := point.Exec(hashes[i], b.rowid); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// Committed. Now verify, then clear.
	tx2, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx2.Rollback() //nolint:errcheck // no-op after a successful Commit
	read, err := tx2.Prepare(`SELECT text_gz FROM declaration_text WHERE hash = ?`)
	if err != nil {
		return 0, err
	}
	defer read.Close()
	// The predicate repeats text_hash = ?: between the two transactions the row may have been
	// deleted by the GC trigger, or (in a second process) already migrated. Neither is an
	// error, and neither may clear a blob whose hash is not the one just verified.
	clear, err := tx2.Prepare(`UPDATE tool_declarations SET text_gz = NULL
		WHERE rowid = ? AND text_hash = ?`)
	if err != nil {
		return 0, err
	}
	defer clear.Close()
	var moved int64
	for i, b := range batch {
		if hashes[i] == "" {
			continue // skipped above: nothing was stored for this row
		}
		var stored []byte
		if err := read.QueryRow(hashes[i]).Scan(&stored); err != nil {
			return 0, err
		}
		// Compared as TEXT, not as bytes: the row's own blob and the one under this hash are
		// two encodings of the same string, and equal text is exactly the property the reveal
		// must keep. Comparing bytes would refuse to migrate a row whose gzip differs while
		// its text is identical — the legitimate case a content key exists to collapse.
		if got := gunzipText(stored); got != texts[i] {
			// Unreachable unless sha256 collided or something else wrote this hash. Leave the
			// blob where it is and say so loudly: the original is still readable, which is the
			// whole reason the clear is a second step.
			slog.Error("dash: stored declaration text does not match the row it came from; "+
				"keeping the original", "hash", hashes[i], "row_bytes", len(b.gz),
				"stored_bytes", len(stored), "text_bytes", len(texts[i]), "got_bytes", len(got))
			continue
		}
		res, err := clear.Exec(b.rowid, hashes[i])
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			moved += n
		}
	}
	return moved, tx2.Commit()
}

// dedupeLoop is the recorder's hook: run the backfill once, then reclaim what it freed.
//
// On its own goroutine and not on the writer's, for the reason archiveLoop is: the writer owes
// the request path a fast insert, and anything measured in minutes on that goroutine means a
// full queue and dropped events.
func (r *Recorder) dedupeLoop() {
	defer r.wg.Done()
	moved, err := r.db.dedupeDeclarationText(r.done, dedupeBatch, dedupePause)
	if err != nil {
		// A partial migration is readable and resumable, so this is a warning, not a fatal:
		// the rows it did not reach still serve their text from text_gz.
		slog.Warn("dash: deduplicating stored prompt text stopped early", "rows", moved, "err", err)
	}
	if moved == 0 {
		return
	}
	// Give the pages back — but only where that is a bounded operation. reclaim() falls back
	// to a full VACUUM on an auto_vacuum=NONE file, which rewrites the whole database with the
	// writer waiting on it: the exact stall the batching above exists to avoid. On such a file
	// the freed pages stay on the freelist and are reused by new rows, which is enough for
	// what this fixes — the file stops GROWING — and an operator who wants the bytes back can
	// VACUUM at a quiet moment.
	var mode int
	if err := r.db.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		slog.Info("dash: freed pages stay on the freelist (auto_vacuum is not incremental); " +
			"they are reused by new rows, and a manual VACUUM would return them to the disk")
		return
	}
	before, _ := r.db.sizeBytes()
	// Until it stops helping, not until the freelist is empty. incremental_vacuum can only
	// truncate free pages at the END of the file, and the pages this migration frees are
	// scattered through it — so most of them stay on the freelist to be REUSED by new rows,
	// and a loop waiting for freelist_count to reach zero would spin its whole bound doing
	// nothing. Reuse is the fix for growth; returning every byte to the filesystem needs a
	// full VACUUM, which is the operator's call and not something to start under live traffic.
	prev := -1
	for i := 0; i < dedupeReclaimPasses; i++ {
		select {
		case <-r.done:
			return
		default:
		}
		var free int
		if err := r.db.sql.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
			break
		}
		if free >= prev && prev >= 0 {
			break
		}
		prev = free
		if err := r.db.reclaim(); err != nil {
			slog.Warn("dash: reclaiming the freed pages failed", "err", err)
			break
		}
	}
	after, _ := r.db.sizeBytes()
	var free int
	_ = r.db.sql.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	slog.Info("dash: reclaimed what the duplicated prompt text held",
		"before_bytes", before, "after_bytes", after, "free_pages_kept_for_reuse", free)
}
