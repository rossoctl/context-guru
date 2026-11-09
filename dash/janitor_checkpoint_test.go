package dash

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestJanitorPassCheckpointsWAL guards the /metrics hang: janitorPass must checkpoint
// the WAL on every pass, not only as a side effect of an actual retention deletion.
// A deployment comfortably inside its retention budget never deletes anything, so
// without an unconditional checkpoint here the WAL is left to grow until SQLite's own
// default wal_autocheckpoint (1000 pages) forces an inline checkpoint on an ordinary
// writer commit — which pays off the whole accumulated backlog inside that commit and
// blocks any reader sharing this *sql.DB (this driver gives no non-blocking WAL reads
// across it) for as long as that checkpoint takes.
// bloatWAL builds a non-trivial WAL directly against the store, bypassing the async
// writer so a test controls exactly how much has accumulated before checking.
// One pooled connection for the pragma and every insert: wal_autocheckpoint is a per-
// connection setting, so on a pool the inserts could land on a connection that still
// auto-checkpoints and the bloat would vary from run to run.
func bloatWAL(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4096)
	for b := 0; b < 40; b++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO requests(ts, tenant_id, session_id, model) VALUES (?,?,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			if _, err := stmt.Exec(time.Now().UnixMilli(), "t1", "s1", string(blob[:8])); err != nil {
				t.Fatal(err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_autocheckpoint=1000`); err != nil {
		t.Fatal(err)
	}
	conn.Close() // back to the pool
}

func TestJanitorPassCheckpointsWAL(t *testing.T) {
	path := t.TempDir() + "/janitor.db"
	rec, err := NewRecorder(Options{
		DBPath: path,
		// Generous retention on purpose: this reproduces a healthy deployment that
		// never hits Prune's delete path, the case the old code silently depended on
		// reclaim() for and never got.
		RetentionAge:   365 * 24 * time.Hour,
		RetentionBytes: 10 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	// The one-time migrations start with the recorder and touch the same tables. A checkpoint
	// cannot truncate the WAL while one of them has a statement open, and one writing during the
	// measurement grows it, so the test starts only once they are done.
	select {
	case <-rec.migrationsDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the background migrations did not finish")
	}

	bloatWAL(t, rec.db)

	before, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	const bloatFloor = 200 << 10 // 200KB: proves the setup actually built up a WAL worth checkpointing
	if before.Size() < bloatFloor {
		t.Fatalf("test setup did not bloat the WAL enough to be meaningful: %d bytes", before.Size())
	}

	rec.janitorPass()

	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WAL size before janitorPass: %d bytes, after: %d bytes", before.Size(), after.Size())
	const wantMax = 64 << 10 // small: a checkpoint just ran and truncated it
	if after.Size() > wantMax {
		t.Fatalf("janitorPass left the WAL at %d bytes (want <= %d): it did not checkpoint on a pass with nothing to prune",
			after.Size(), wantMax)
	}
}

// TestCheckpointRetriesWhileAReaderHoldsTheLock pins the fix for the silent skip (#166):
// wal_checkpoint(TRUNCATE) reports a busy checkpoint as a result ROW, not an error, and the
// old code discarded it — so the WAL-bloat protection ran as a no-op exactly when a reader
// was active, which on a real deployment is when the WAL is growing. The retry itself lives
// inside the call: the pool's busy_timeout(5000) makes SQLite re-ask for the lock for up to
// 5s, which is why a reader that holds the read lock across the WHOLE pass costs that pass
// ~5s and then still fails. What the fix adds is the visible half: the WAL untruncated
// (nothing else can happen while the lock is held) and a WARN saying so — the half the old
// code swallowed, and the only way an operator reading the logs can tell a checkpoint that
// ran from one that was silently skipped. Once the reader lets go, the next pass must
// truncate for real.
//
// It fails on the single-attempt code at the log assertion: the WAL is left untruncated
// there too, but silently, which is precisely the defect.
func TestCheckpointRetriesWhileAReaderHoldsTheLock(t *testing.T) {
	path := t.TempDir() + "/reader-lock.db"
	rec, err := NewRecorder(Options{
		DBPath:         path,
		RetentionAge:   365 * 24 * time.Hour,
		RetentionBytes: 10 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	select {
	case <-rec.migrationsDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the background migrations did not finish")
	}

	bloatWAL(t, rec.db)

	before, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	const bloatFloor = 200 << 10
	if before.Size() < bloatFloor {
		t.Fatalf("test setup did not bloat the WAL enough to be meaningful: %d bytes", before.Size())
	}

	// A second connection, as a dashboard reader would have: an open read transaction that
	// has read holds the WAL read lock until it ends, which is what makes
	// wal_checkpoint(TRUNCATE) answer busy instead of running.
	reader, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rx.QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	// Capture what the pass logs. Locked, because the recorder's own background goroutines
	// share the default logger and a plain buffer would race under -race.
	var mu sync.Mutex
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil)))
	rec.janitorPass()
	slog.SetDefault(prev)

	mu.Lock()
	got := logs.String()
	mu.Unlock()
	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() < bloatFloor {
		t.Fatalf("the reader held the lock across the whole pass, so the WAL must still hold its "+
			"backlog (size %d) — if it was truncated the fixture held no lock at all", after.Size())
	}
	if !strings.Contains(got, "WAL checkpoint did not complete") {
		t.Fatal("a checkpoint that stayed busy for the whole retry budget left no trace: the old " +
			"code discarded the busy row, and an operator cannot tell a checkpoint that ran from " +
			"one that was silently skipped (#166)")
	}

	// The reader lets go, and the next pass must truncate for real: the retry exists so a
	// short reader costs a checkpoint nothing, and a released lock must not be remembered as
	// busy.
	if err := rx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rec.janitorPass()
	after, err = os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WAL size before the passes: %d bytes, after the reader let go: %d bytes",
		before.Size(), after.Size())
	const wantMax = 64 << 10
	if after.Size() > wantMax {
		t.Fatalf("janitorPass left the WAL at %d bytes (want <= %d) after the reader released "+
			"the lock: the busy result is being remembered past the reader that produced it",
			after.Size(), wantMax)
	}
}

// lockedWriter is a bytes.Buffer that concurrent loggers may share.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestOpenCheckpointsInheritedWAL guards the other half of the /metrics hang: a
// process that inherits an already-bloated WAL from a PREVIOUS run (a restart, not a
// clean shutdown -- SQLite's own auto-checkpoint-on-close never got a chance to run)
// must not leave that backlog sitting unpaid, silently, for whatever query or commit
// happens to touch it first. Open must checkpoint it proactively.
func TestOpenCheckpointsInheritedWAL(t *testing.T) {
	path := t.TempDir() + "/inherited.db"

	// Simulate the previous process: open plainly (bypassing Open/NewRecorder),
	// disable autocheckpoint, write enough to build a non-trivial WAL, then close
	// WITHOUT letting SQLite's own close-time auto-checkpoint run -- Close() on a
	// clean *sql.DB does exactly that, which is not what an abrupt restart leaves
	// behind, so a bare low-level connection is used instead and simply abandoned.
	sdb, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated table, not "requests": this connection is deliberately opened
	// without going through Open/migrate, so it must not collide with the schema
	// migrate() expects to find once the real Open below runs against this file.
	if _, err := sdb.Exec(`CREATE TABLE bloat_probe(id INTEGER PRIMARY KEY, tenant_id TEXT, ts INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	for b := 0; b < 30; b++ {
		tx, err := sdb.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO bloat_probe(tenant_id, ts) VALUES (?, ?)`)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 200; i++ {
			if _, err := stmt.Exec("t1", time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately never sdb.Close(): SQLite auto-checkpoints when the LAST
	// connection to a WAL database closes cleanly, which is exactly what an abrupt
	// process restart (systemd SIGTERM/SIGKILL) does not get a chance to do. The
	// leaked connection is harmless -- this process exits at the end of the test.

	before, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	const bloatFloor = 200 << 10
	if before.Size() < bloatFloor {
		t.Skipf("test setup did not bloat the WAL enough to be meaningful (%d bytes); SQLite's own close-time checkpoint likely folded it in, which is exactly the case this test cannot exercise", before.Size())
	}

	// The fresh open a restarted process performs.
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	after, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WAL size before Open: %d bytes, after: %d bytes", before.Size(), after.Size())
	const wantMax = 64 << 10
	if after.Size() > wantMax {
		t.Fatalf("Open left an inherited WAL at %d bytes (want <= %d): it did not checkpoint the backlog left by the previous process",
			after.Size(), wantMax)
	}
}
