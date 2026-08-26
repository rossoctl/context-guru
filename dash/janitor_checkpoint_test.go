package dash

import (
	"database/sql"
	"os"
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

	// Build up a non-trivial WAL directly against the store, bypassing the async
	// writer so the test controls exactly how much has accumulated before checking.
	if _, err := rec.db.sql.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4096)
	for b := 0; b < 40; b++ {
		tx, err := rec.db.sql.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO requests(ts, tenant_id, session_id, model) VALUES (?,?,?,?)`)
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
	if _, err := rec.db.sql.Exec(`PRAGMA wal_autocheckpoint=1000`); err != nil {
		t.Fatal(err)
	}

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
