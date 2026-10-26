package dash

import "testing"

// The history import is removed. An install that ran it still holds its rows, which mix transcript
// spend into the totals and never expire. Opening the database deletes them once.
//
// Seeded with raw SQL, as an old install left them: the importer itself no longer exists. The purge
// runs once and is recorded in meta, so a database written by an old install has no marker yet.
func seedOldImport(t *testing.T, path string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		// A measured session, and a session that was ONLY imported.
		`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source) VALUES (100, 't1', 'measured', 'm', 2, '')`,
		`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source) VALUES (50, 't1', 'imported', 'm', 0, 'imported')`,
		`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source) VALUES (60, 't1', 'imported', 'm', 0, 'imported')`,
		// A mixed session: began before the proxy (imported), continued through it (measured).
		`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source) VALUES (10, 't1', 'mixed', 'm', 0, 'imported')`,
		`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source) VALUES (200, 't1', 'mixed', 'm', 2, '')`,
		`INSERT INTO tool_uses(tenant_id, session_id, name, server, skill, calls, first_ts, last_ts, project)
		 VALUES ('t1', 'imported', 'Skill', '', 'dataviz', 3, 50, 60, '')`,
		`INSERT INTO tool_uses(tenant_id, session_id, name, server, skill, calls, first_ts, last_ts, project)
		 VALUES ('t1', 'measured', 'Bash', '', '', 1, 100, 100, '')`,
		// An old install never ran the purge, so it has no marker. Open just wrote one (it found
		// nothing to delete on this fresh database), so remove it to look like the old install.
		`DELETE FROM meta WHERE key = 'imported_history_purged'`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatalf("%v: %s", err, q)
		}
	}
}

func count(t *testing.T, db *DB, q string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%v: %s", err, q)
	}
	return n
}

func TestOpeningADatabaseDeletesImportedRows(t *testing.T) {
	path := t.TempDir() + "/d.db"
	seedOldImport(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if n := count(t, db, `SELECT COUNT(*) FROM requests WHERE source = 'imported'`); n != 0 {
		t.Errorf("%d imported requests survived the upgrade", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM requests WHERE source = ''`); n != 2 {
		t.Errorf("measured requests = %d, want 2 (the measured and the mixed session's measured row)", n)
	}
	// The imported-only session's calls go with its last request (trg_tool_inventory_gc).
	if n := count(t, db, `SELECT COUNT(*) FROM tool_uses WHERE session_id = 'imported'`); n != 0 {
		t.Errorf("the imported-only session kept %d tool_uses rows", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM tool_uses WHERE session_id = 'measured'`); n != 1 {
		t.Errorf("the measured session's tool_uses = %d, want 1", n)
	}
}

func TestDeletingImportedRowsIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/d.db"
	seedOldImport(t, path)
	for i := 0; i < 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		db.Close()
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := count(t, db, `SELECT COUNT(*) FROM requests`); n != 2 {
		t.Errorf("requests = %d after three opens, want 2", n)
	}
}

// Once done, the purge is not run again: a later open finds the marker and skips the scan. An
// imported row that appeared afterwards (it cannot, with the importer gone) would stay.
func TestThePurgeRunsOnlyOnce(t *testing.T) {
	path := t.TempDir() + "/d.db"
	seedOldImport(t, path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source)
		VALUES (300, 't1', 'late', 'm', 0, 'imported')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()

	if n := count(t, again, `SELECT COUNT(*) FROM requests WHERE session_id = 'late'`); n != 1 {
		t.Errorf("the purge ran again on a later open (late row count = %d, want 1)", n)
	}
}

// The purge is an optional cleanup, so it must never fail Open: a failed Open closes the database and
// the recorder falls back to an in-memory dashboard that records nothing to disk. Two processes that
// open the same old database at once both see no marker; the second one's marker INSERT used to hit
// the unique key and fail its Open.
func TestThePurgeNeverFailsOpenWhenTheMarkerAlreadyExists(t *testing.T) {
	path := t.TempDir() + "/d.db"
	seedOldImport(t, path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The race, after the fact: the other process already wrote the marker, this one then runs the
	// purge as if it had read none (its read of meta came first).
	if err := runImportedHistoryPurge(db.sql); err != nil {
		t.Errorf("a purge that finds the marker already written failed: %v", err)
	}
}

// A purge that cannot run (a locked or broken requests table) is logged, and Open still returns a
// file-backed database. The next open retries, because no marker was written.
func TestAFailedPurgeStillOpensTheDatabase(t *testing.T) {
	path := t.TempDir() + "/d.db"
	seedOldImport(t, path)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Make the purge's DELETE fail: a trigger that aborts any delete of an imported row.
	if _, err := db.sql.Exec(`DELETE FROM meta WHERE key = 'imported_history_purged'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER block_purge BEFORE DELETE ON requests
		WHEN OLD.source = 'imported' BEGIN SELECT RAISE(ABORT, 'locked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model, tools, source)
		VALUES (5, 't1', 'again', 'm', 0, 'imported')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	again, err := Open(path)

	if err != nil {
		t.Fatalf("a failed purge failed Open, which sends the recorder to an in-memory dashboard: %v", err)
	}
	defer again.Close()
	if again.path != path {
		t.Errorf("path = %q, want the file %q", again.path, path)
	}
	if n := count(t, again, `SELECT COUNT(*) FROM meta WHERE key = 'imported_history_purged'`); n != 0 {
		t.Errorf("the marker was written after a failed purge, so it would never retry")
	}
}
