package dash

// Recording WHICH PROJECT each capability was invoked in.
//
// THE BUG THIS EXISTS FOR, reported against the running dashboard: selecting a project showed
// the same usage figures whatever was selected. `tool_uses` is keyed by session and carried no
// project, so the per-project query had to select whole sessions that touched the folder:
//
//	WHERE session_id IN (SELECT session_id FROM requests WHERE project = ?)
//
// Measured on the live store: 8 sessions span more than one folder and one spans 14. Selecting
// `/private/tmp/cgzip` — 6 requests — returned that session's entire 2,626-request tool list, so
// every folder a session passed through reported nearly identical numbers.
//
// The folder was never stored, so this is not a query that can be fixed in place. It needs the
// column, and the column has to be IN THE PRIMARY KEY: one session working in two folders would
// otherwise collapse to a single row whose project is whichever folder was written last.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The column exists, is in the key, and distinguishes two folders in one session.
func TestToolUsesRecordsTheProjectEachCapabilityWasInvokedIn(t *testing.T) {
	db := openTestDB(t)
	// ONE SESSION, TWO FOLDERS — the shape that produced the reported bug.
	insertUse(t, db, "s1", "Skill", "superpowers:brainstorming", "/Users/v/notes", daysAgo(10))
	insertUse(t, db, "s1", "Bash", "", "/Users/v/code", daysAgo(10))

	// Both rows survive. Without `project` in the primary key the second would have replaced
	// nothing — they differ by name — but a session invoking the SAME capability in two folders
	// would collapse, which the next assertion covers.
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d rows; want 2", n)
	}
	var proj string
	if err := db.sql.QueryRow(
		`SELECT project FROM tool_uses WHERE skill = ?`, "superpowers:brainstorming").
		Scan(&proj); err != nil {
		t.Fatalf("no project column to read: %v", err)
	}
	if proj != "/Users/v/notes" {
		t.Errorf("project = %q; want /Users/v/notes — the folder the invocation happened in", proj)
	}
}

// THE KEY ASSERTION, literally: the same capability invoked in two folders in ONE session must
// stay two rows. This is what `project` being part of the primary key buys, and the whole reason
// the table is rebuilt rather than just altered.
func TestTheSameCapabilityInTwoFoldersStaysTwoRows(t *testing.T) {
	db := openTestDB(t)
	insertUse(t, db, "s1", "Bash", "", "/Users/v/notes", daysAgo(10))
	insertUse(t, db, "s1", "Bash", "", "/Users/v/notes", daysAgo(10))
	insertUse(t, db, "s1", "Bash", "", "/Users/v/code", daysAgo(9))
	rows, err := db.sql.Query(`SELECT project, calls FROM tool_uses ORDER BY project`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var p string
		var c int
		if err := rows.Scan(&p, &c); err != nil {
			t.Fatal(err)
		}
		got[p] = c
	}
	if len(got) != 2 {
		t.Fatalf("Bash in two folders collapsed to %d row(s): %v. With `project` outside the "+
			"primary key the folder recorded is whichever was written last, which is how every "+
			"project came to report the same figures", len(got), got)
	}
	if got["/Users/v/notes"] != 2 {
		t.Errorf("notes calls = %d; want 2", got["/Users/v/notes"])
	}
	if got["/Users/v/code"] != 1 {
		t.Errorf("code calls = %d; want 1", got["/Users/v/code"])
	}
}

// THE MIGRATION MUST NOT LOSE A ROW.
//
// SQLite cannot add a column to a primary key, so this is a table rebuild: create, copy, drop,
// rename. A rebuild that drops rows would discard the 5,379 invocations just imported from the
// user's own history — the data the whole feature exists to surface.
func TestTheProjectMigrationPreservesEveryExistingRow(t *testing.T) {
	db := openTestDB(t)
	// Rows as the OLD schema wrote them: no project.
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, server, skill, calls, first_ts, last_ts
	) VALUES ('','s-old','Bash','','',99,1000,2000),
	         ('','s-old','Skill','','superpowers:brainstorming',7,1500,2500)`); err != nil {
		t.Fatal(err)
	}
	// Re-running the migration is what an upgrade does.
	if err := migrateToolUsesProject(db.sql); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	var n, totalCalls int
	if err := db.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(calls),0) FROM tool_uses`).
		Scan(&n, &totalCalls); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("%d rows after migrating; want 2 — a rebuild that loses rows discards the "+
			"user's imported history", n)
	}
	if totalCalls != 106 {
		t.Errorf("total calls = %d; want 106 (99+7) — the counts have to survive intact",
			totalCalls)
	}
	// AN OLD ROW READS AS UNKNOWN, NOT AS A FOLDER. '' means "written before the column existed",
	// which is a different claim from "invoked outside any project" and must not be presented as
	// one.
	var blank int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses WHERE project = ''`).
		Scan(&blank); err != nil {
		t.Fatal(err)
	}
	if blank != 2 {
		t.Errorf("%d of 2 migrated rows have an empty project; an old row's folder is unknown "+
			"and has to stay that way rather than being guessed", blank)
	}
}

// Idempotent: an upgrade runs migrations on every open.
func TestTheProjectMigrationIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	insertUse(t, db, "s1", "Bash", "", "/Users/v/notes", daysAgo(10))
	for i := 0; i < 3; i++ {
		if err := migrateToolUsesProject(db.sql); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var n int
	var proj string
	if err := db.sql.QueryRow(`SELECT COUNT(*), COALESCE(MAX(project),'') FROM tool_uses`).
		Scan(&n, &proj); err != nil {
		t.Fatal(err)
	}
	if n != 1 || proj != "/Users/v/notes" {
		t.Errorf("after three migrations: %d rows, project %q; want 1 row keeping its folder",
			n, proj)
	}
}

// THE LIVE PATH MUST RECORD THE FOLDER TOO, or new traffic keeps accruing the defect the
// migration just fixed and the page is right about history and wrong about today.
//
// Through the real path — recorder queue, writer goroutine, shutdown drain — as
// TestRecordInventoryEndToEnd does, because the project has to survive the hand-off between
// them and a direct insert would not exercise that.
func TestTheLiveInventoryWriterRecordsTheProject(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	inv := ScanInventory("anthropic", ccBody(t, []string{tool("Bash", "b")}, skillsReminder,
		[]map[string]any{{"name": "Bash", "input": map[string]any{}}}))
	rec.RecordInventory("t1", "s-live", 1000, inv, true, "/Users/v/notes")
	db := rec.DB()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the live path wrote no invocation row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var proj string
	if err := db.sql.QueryRow(
		`SELECT project FROM tool_uses WHERE name = 'Bash'`).Scan(&proj); err != nil {
		t.Fatal(err)
	}
	if proj != "/Users/v/notes" {
		t.Errorf("project = %q; want the request's own folder. Without it, traffic measured "+
			"from now on cannot be scoped to a project either", proj)
	}
}

// AN UPGRADE MUST NOT BREAK THE OPEN.
//
// THE FAILURE THIS CAUGHT, on the live database. The project index was first written into
// `additiveDDL`, which runs on every open BEFORE the migration — and since
// `CREATE TABLE IF NOT EXISTS` is a no-op on an existing database, the index ran against the OLD
// table and failed with "no such column: project". That failed the whole open, and the proxy fell
// back to an in-memory database: the dashboard came up looking fine with every row of history
// gone. A migration that cannot be applied to a real database is worse than no migration.
func TestAnExistingDatabaseStillOpensAfterTheProjectMigration(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/d.db"
	// A database at the CURRENT schema version but with the OLD tool_uses shape — exactly what an
	// upgrade meets.
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.sql.Exec(`DROP TABLE tool_uses`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.sql.Exec(`CREATE TABLE tool_uses (
		tenant_id  TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		name       TEXT NOT NULL DEFAULT '',
		server     TEXT NOT NULL DEFAULT '',
		skill      TEXT NOT NULL DEFAULT '',
		calls      INTEGER NOT NULL DEFAULT 0,
		first_ts   INTEGER NOT NULL DEFAULT 0,
		last_ts    INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, session_id, name, skill)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, skill, calls, first_ts, last_ts
	) VALUES ('','s-old','Bash','',42,1000,2000)`); err != nil {
		t.Fatal(err)
	}
	first.Close()

	// THE MIGRATION DIRECTLY, not via a fresh Open. On a database already at the current schema
	// version `ddl` is skipped — which is the whole point of this path — so running Open on a
	// NEW file would recreate the trigger afterwards and mask a migration that dropped it. This
	// test did exactly that and passed while the trigger was being lost.
	up, err := Open(path)
	if err != nil {
		t.Fatalf("an existing database no longer opens after this migration: %v.\nIn the proxy "+
			"this does not surface as an error — it falls back to an in-memory database, so the "+
			"dashboard comes up looking healthy with every row of history gone", err)
	}
	defer up.Close()

	var calls int
	var proj string
	if err := up.sql.QueryRow(
		`SELECT calls, project FROM tool_uses WHERE name='Bash'`).Scan(&calls, &proj); err != nil {
		t.Fatalf("the pre-existing row did not survive the upgrade: %v", err)
	}
	if calls != 42 {
		t.Errorf("calls = %d; want 42 — the row's data has to survive the rebuild", calls)
	}
	if proj != "" {
		t.Errorf("project = %q; an old row's folder is unknown and must stay empty rather than "+
			"being attributed to a project the user may never have touched", proj)
	}
	// And the index the migration owns is actually there, or every per-project query scans.
	var idx int
	if err := up.sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tooluses_project'`).
		Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Error("idx_tooluses_project is missing; the per-project lookups this column exists " +
			"for would scan the table")
	}

	// AND THE GC TRIGGER IS STILL THERE. The rebuild must drop it — SQLite refuses to drop a
	// table a trigger references — so this pins that something puts it back. It does: the
	// trigger lives in additiveDDL2, which migrate() runs on both paths AFTER this migration.
	// That ordering is load-bearing and invisible, which is why it is asserted rather than
	// trusted: move the migration below additiveDDL2 and an upgraded database silently stops
	// garbage-collecting a purged session'''s inventory.
	var trg int
	if err := up.sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='trg_tool_inventory_gc'`).
		Scan(&trg); err != nil {
		t.Fatal(err)
	}
	if trg != 1 {
		t.Error("trg_tool_inventory_gc did not survive the rebuild. Without it a purged " +
			"session's tool rows outlive its request rows, which is the leak that trigger " +
			"exists to prevent")
	}
}

// S3c: the project filter on requests has an index, so a single-tenant, unbounded query is not a
// table scan.
func TestRequestsProjectIsIndexed(t *testing.T) {
	db := openTestDB(t)
	var n int
	db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_requests_project'`).Scan(&n)
	if n != 1 {
		t.Error("idx_requests_project is missing")
	}
}

// S2(2): the rebuild must not fail the open when tool_uses has no columns to probe.
func TestMigrateToolUsesProjectWithoutTableIsANoop(t *testing.T) {
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := migrateToolUsesProject(raw); err != nil {
		t.Fatalf("migrating a database with no tool_uses failed: %v", err)
	}
}
