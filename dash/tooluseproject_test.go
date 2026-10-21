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
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(10), isSkill: true,
			project: "/Users/v/notes"},
		{session: "s1", name: "Bash", ts: daysAgo(10), project: "/Users/v/code"},
	}); err != nil {
		t.Fatal(err)
	}

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
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "Bash", ts: daysAgo(10), project: "/Users/v/notes"},
		{session: "s1", name: "Bash", ts: daysAgo(10), project: "/Users/v/notes"},
		{session: "s1", name: "Bash", ts: daysAgo(9), project: "/Users/v/code"},
	}); err != nil {
		t.Fatal(err)
	}
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
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "Bash", ts: daysAgo(10), project: "/Users/v/notes"},
	}); err != nil {
		t.Fatal(err)
	}
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

// THE IMPORTER MUST CARRY THE TURN'S OWN FOLDER through to the invocation row.
//
// A gap the first version of these tests left open: they built `importedUse` values directly, so
// the importer could have gone on discarding `TranscriptTurn.Project` — which is exactly what the
// original defect was — and every assertion would still have passed. This drives the real
// ImportHistory path from transcripts on disk.
func TestAnImportRecordsTheFolderEachInvocationHappenedIn(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// ONE SESSION, TWO FOLDERS, as a real transcript records it: `cwd` is per turn.
	writeJSONL(t, home, "-Users-v-proj", "s-folders",
		turnLineCalling("s-folders", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-folders", "/Users/v/proj/docs", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}`))

	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := db.sql.Query(`SELECT
		CASE WHEN skill <> '' THEN skill ELSE name END, project FROM tool_uses`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cap, proj string
		if err := rows.Scan(&cap, &proj); err != nil {
			t.Fatal(err)
		}
		got[cap] = proj
	}
	if got["superpowers:brainstorming"] != "/Users/v/proj" {
		t.Errorf("the skill's folder = %q; want /Users/v/proj. The transcript records a folder "+
			"per turn and the importer has to carry it, or no per-project report is possible",
			got["superpowers:brainstorming"])
	}
	if got["Bash"] != "/Users/v/proj/docs" {
		t.Errorf("the tool's folder = %q; want /Users/v/proj/docs — the turn's OWN folder, not "+
			"the session's first one", got["Bash"])
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

// BACKFILLING THE FOLDER ONTO INVOCATIONS THAT WERE IMPORTED BEFORE THE COLUMN EXISTED.
//
// WHY THIS IS A SEPARATE OPERATION. ImportHistory dedupes against the `requests` rows it already
// wrote, which is right — re-running it must not double-count spend. But that dedupe also skips
// the turns whose invocations need their folder, so an upgraded database keeps thousands of rows
// with project=” and every per-project list stays empty. Measured on a live store: all 77
// invocation rows, covering 5,379 imported requests, carried an unknown folder after the
// migration, and a re-import skipped every one of them.
//
// So this re-reads the transcripts and repairs `tool_uses` ALONE, writing no request rows at all.
func TestBackfillingFoldersRepairsImportedInvocationsWithoutTouchingSpend(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-bf",
		turnLineCalling("s-bf", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-bf", "/Users/v/proj/docs", daysAgo(8),
			`{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}`))

	// The state an upgraded database is in: requests imported, invocations present, folders blank.
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE tool_uses SET project = ''`); err != nil {
		t.Fatal(err)
	}
	var reqBefore, callsBefore int
	if err := db.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM requests), (SELECT COALESCE(SUM(calls),0) FROM tool_uses)`).
		Scan(&reqBefore, &callsBefore); err != nil {
		t.Fatal(err)
	}

	n, err := db.BackfillUseProjects(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("the backfill repaired nothing; the rows it exists for are still unattributed")
	}

	// THE FOLDERS ARE RIGHT, per turn rather than per session.
	got := map[string]string{}
	rows, err := db.sql.Query(`SELECT
		CASE WHEN skill <> '' THEN skill ELSE name END, project FROM tool_uses`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cap, proj string
		if err := rows.Scan(&cap, &proj); err != nil {
			t.Fatal(err)
		}
		got[cap] = proj
	}
	if got["superpowers:brainstorming"] != "/Users/v/proj" {
		t.Errorf("skill folder = %q; want /Users/v/proj", got["superpowers:brainstorming"])
	}
	if got["Bash"] != "/Users/v/proj/docs" {
		t.Errorf("tool folder = %q; want the turn's own folder /Users/v/proj/docs", got["Bash"])
	}

	// AND SPEND IS UNTOUCHED. The whole reason this is not a re-import: writing request rows
	// again would double-count every one of them.
	var reqAfter, callsAfter int
	if err := db.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM requests), (SELECT COALESCE(SUM(calls),0) FROM tool_uses)`).
		Scan(&reqAfter, &callsAfter); err != nil {
		t.Fatal(err)
	}
	if reqAfter != reqBefore {
		t.Errorf("requests went from %d to %d; a folder backfill must not touch spend at all",
			reqBefore, reqAfter)
	}
	if callsAfter != callsBefore {
		t.Errorf("call total went from %d to %d; repairing a folder must not change how often "+
			"anything was invoked", callsBefore, callsAfter)
	}
}

// Idempotent, and it leaves an already-correct folder alone.
func TestBackfillingFoldersIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	writeJSONL(t, home, "-Users-v-proj", "s-idem",
		turnLineCalling("s-idem", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`))
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.BackfillUseProjects(home, true); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var n, calls int
	var proj string
	if err := db.sql.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(calls),0), COALESCE(MAX(project),'') FROM tool_uses`).
		Scan(&n, &calls, &proj); err != nil {
		t.Fatal(err)
	}
	if n != 1 || calls != 1 || proj != "/Users/v/proj" {
		t.Errorf("after three backfills: %d rows, %d calls, project %q; want 1/1//Users/v/proj",
			n, calls, proj)
	}
}

// A LIVE SESSION'S INVOCATIONS ARE NOT REPAIRED FROM A TRANSCRIPT.
//
// The backfill exists for rows imported before the folder column existed. A measured row is a
// different kind of fact: the proxy watched it, its folder was either recorded or is genuinely
// unrecoverable, and a transcript is not evidence about it. Attributing measured traffic from a
// transcript would quietly mix the two sources on a page whose whole claim is that each figure
// says where it came from.
func TestBackfillingFoldersLeavesMeasuredInvocationsAlone(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// A transcript that mentions BOTH sessions — so the only thing stopping the live one being
	// rewritten is the importer-source check.
	writeJSONL(t, home, "-Users-v-proj", "s-imported",
		turnLineCalling("s-imported", "/Users/v/proj", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`))
	writeJSONL(t, home, "-Users-v-proj", "s-measured",
		turnLineCalling("s-measured", "/Users/v/elsewhere", daysAgo(3),
			`{"type":"tool_use","id":"t2","name":"Grep","input":{"pattern":"x"}}`))

	// THE MEASURED ROW GOES IN FIRST, and that is what makes this session measured: the importer
	// skips every turn at or after the earliest measured request, so this session ends up with a
	// measured row and no imported one. A session holding BOTH is the normal mid-install case,
	// and is also left alone — see BackfillUseProjects.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(4), SessionID: "s-measured", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, skill, calls, first_ts, last_ts, project
	) VALUES ('','s-measured','Grep','',5,1000,2000,'')`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}

	var proj string
	var calls int
	if err := db.sql.QueryRow(
		`SELECT project, calls FROM tool_uses WHERE session_id = 's-measured'`).
		Scan(&proj, &calls); err != nil {
		t.Fatalf("the measured invocation row was deleted by the backfill: %v. Only imported "+
			"rows are its business", err)
	}
	if proj != "" {
		t.Errorf("the measured row's folder was set to %q from a transcript. The proxy watched "+
			"that traffic; a transcript is not evidence about it, and '' means unknown rather "+
			"than wrong", proj)
	}
	if calls != 5 {
		t.Errorf("the measured row's call count changed to %d; want 5 untouched", calls)
	}
}

// A MEASURED INVOCATION IN A MIXED SESSION IS NEVER REWRITTEN.
//
// The companion to TestBackfillingFoldersRepairsThePreMeasurementPartOfAMixedSession: that one
// pins that the pre-proxy turns ARE repaired, this one that the measured turns are NOT. Both
// halves matter, and an earlier rule here got the trade wrong in each direction in turn — first
// skipping the whole session (so a 1,053-request project reported no usage), then deleting every
// unattributed row in it (so a measured count of 99 was replaced by a transcript-derived 1).
//
// The boundary is the session”'s earliest measured request. At or after it, the proxy was
// watching and the transcript does not get to speak.
func TestBackfillingFoldersNeverRewritesAMeasuredInvocation(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// A turn AFTER the proxy started measuring, invoking the same capability as a measured row.
	writeJSONL(t, home, "-Users-v-proj", "s-after",
		turnLineCalling("s-after", "/Users/v/proj", daysAgo(20),
			`{"type":"tool_use","id":"t0","name":"Grep","input":{"pattern":"x"}}`),
		turnLineCalling("s-after", "/Users/v/elsewhere", daysAgo(1),
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`))

	// The proxy has been measuring since day 5, so the day-1 turn is traffic it watched.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(5), SessionID: "s-after", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// The measured invocation of Bash, folder unknown, as the live path left it.
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, skill, calls, first_ts, last_ts, project
	) VALUES ('','s-after','Bash','',99,1,2,'')`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}

	var proj string
	var calls int
	if err := db.sql.QueryRow(`SELECT project, calls FROM tool_uses
		WHERE session_id = 's-after' AND name = 'Bash'`).Scan(&proj, &calls); err != nil {
		t.Fatalf("the measured invocation row was deleted: %v", err)
	}
	if proj != "" || calls != 99 {
		t.Errorf("the measured Bash invocation became folder=%q calls=%d; want empty and 99. "+
			"Its turn is at or after the proxy's first measured request, so the transcript is "+
			"not evidence about it", proj, calls)
	}
}

// THE BOUNDARY TURN BELONGS TO THE MEASURED SIDE.
//
// A turn whose timestamp is exactly the session's first measured request IS that request: the
// proxy recorded it, and the transcript recorded the same event from the other end. So the
// cutoff is `>=`, not `>`. With `>` the two sources both claim one invocation, and the
// transcript's folder overwrites the measured row — the single case where off-by-one silently
// mixes provenance rather than dropping or duplicating a row.
//
// ImportHistory makes the same choice for the same reason; see rule 1 there.
func TestBackfillingFoldersLeavesTheBoundaryTurnToTheMeasuredSide(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	boundary := daysAgo(5)
	writeJSONL(t, home, "-Users-v-proj", "s-edge",
		// Clearly before: must be repaired, so the test cannot pass by repairing nothing.
		turnLineCalling("s-edge", "/Users/v/proj", daysAgo(20),
			`{"type":"tool_use","id":"t0","name":"Grep","input":{"pattern":"x"}}`),
		// EXACTLY at the boundary: the same event the proxy measured.
		turnLineCalling("s-edge", "/Users/v/elsewhere", boundary,
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`))

	if err := db.insertBatch([]*Event{
		{TS: boundary, SessionID: "s-edge", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// The measured row for that same boundary turn, folder unknown.
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, skill, calls, first_ts, last_ts, project
	) VALUES ('','s-edge','Bash','',99,1,2,'')`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}

	var proj string
	var calls int
	if err := db.sql.QueryRow(`SELECT project, calls FROM tool_uses
		WHERE session_id = 's-edge' AND name = 'Bash'`).Scan(&proj, &calls); err != nil {
		t.Fatalf("the boundary invocation row was deleted: %v", err)
	}
	if proj != "" || calls != 99 {
		t.Errorf("the boundary turn was repaired to folder=%q calls=%d; want empty and 99. A "+
			"turn at exactly the first measured timestamp IS that measured request, so the "+
			"cutoff has to be >= rather than >", proj, calls)
	}
	// And the genuinely-old turn still got repaired, so this is not passing vacuously.
	var oldProj string
	if err := db.sql.QueryRow(`SELECT project FROM tool_uses
		WHERE session_id = 's-edge' AND name = 'Grep'`).Scan(&oldProj); err != nil {
		t.Fatal(err)
	}
	if oldProj != "/Users/v/proj" {
		t.Errorf("the pre-measurement turn was not repaired (folder %q), so this test would "+
			"pass even if the backfill did nothing at all", oldProj)
	}
}

// THE BACKFILL MUST NOT INVENT INVOCATIONS FOR TRAFFIC THAT IS NOT IN THE STORE.
//
// A transcript on disk is not the same set as the history this database holds. A session can sit
// in `~/.claude/projects` and have no rows here at all — it predates the `--since` cutoff of the
// import that ran, or its rows were pruned by retention. Rebuilding invocations from every
// transcript it can read would add calls belonging to neither measured nor imported traffic, and
// those calls would then count toward "this capability was used" while no request row explains
// where they came from.
//
// Measured: without the imported-session filter the backfill wrote a row for exactly such a
// session.
func TestBackfillingFoldersInventsNothingForSessionsNotInTheStore(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// Present on disk, absent from the store.
	writeJSONL(t, home, "-Users-v-proj", "s-ghost",
		turnLineCalling("s-ghost", "/Users/v/ghost", daysAgo(9),
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}`))
	// A real imported session, so the backfill has work to do and does not early-return.
	writeJSONL(t, home, "-Users-v-proj", "s-real",
		turnLineCalling("s-real", "/Users/v/real", daysAgo(9),
			`{"type":"tool_use","id":"t2","name":"Grep","input":{"pattern":"x"}}`))
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// Retention, or a narrower import, leaves the transcript on disk and no rows here.
	if _, err := db.sql.Exec(`DELETE FROM requests WHERE session_id = 's-ghost'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM tool_uses WHERE session_id = 's-ghost'`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}

	var ghost int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM tool_uses WHERE session_id = 's-ghost'`).Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if ghost != 0 {
		t.Errorf("the backfill wrote %d invocation row(s) for a session with no request rows. "+
			"Those calls belong to no traffic this store knows about, and would still count "+
			"toward 'this capability was used'", ghost)
	}
	// The real session is still repaired, so the filter is not simply disabling the feature.
	var proj string
	if err := db.sql.QueryRow(
		`SELECT project FROM tool_uses WHERE session_id = 's-real'`).Scan(&proj); err != nil {
		t.Fatal(err)
	}
	if proj != "/Users/v/real" {
		t.Errorf("the real session's folder = %q; want /Users/v/real", proj)
	}
}

// A MOSTLY-IMPORTED SESSION STILL GETS REPAIRED, FOR THE TURNS THE TRANSCRIPT EXPLAINS.
//
// THE LIMITATION THIS FIXES, found on the live store. Session a959e76a held 1,053 requests in
// /Users/vitalyz/Research/notes, of which 17 were measured and 1,036 imported. The first rule
// here — skip any session holding a measured row — meant that whole session was left
// unattributed, and the notes project reported ZERO capabilities used across 1,067 requests of
// real work. The rule was protecting 17 rows at the cost of 1,036.
//
// The right unit is the INVOCATION, not the session. A turn before the proxy's first measured
// request is history the transcript is the only witness to, and repairing it takes nothing from
// the measured side; a turn at or after that point is traffic the proxy watched, and the
// transcript must not speak for it. That is the same cutoff ImportHistory uses to decide which
// turns to import at all, applied to invocations.
func TestBackfillingFoldersRepairsThePreMeasurementPartOfAMixedSession(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// A session with old turns (pre-proxy) and a recent one (measured).
	writeJSONL(t, home, "-Users-v-notes", "s-mixed",
		turnLineCalling("s-mixed", "/Users/v/notes", daysAgo(20),
			`{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"superpowers:brainstorming"}}`),
		turnLineCalling("s-mixed", "/Users/v/notes/server", daysAgo(19),
			`{"type":"tool_use","id":"t2","name":"Grep","input":{"pattern":"x"}}`))

	// The proxy starts measuring on day 2 — long after those turns.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(2), SessionID: "s-mixed", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	// A measured invocation from the recent part, with its own folder already unknown — the state
	// the live path left before the column existed. It must survive untouched.
	if _, err := db.sql.Exec(`INSERT INTO tool_uses(
		tenant_id, session_id, name, skill, calls, first_ts, last_ts, project
	) VALUES ('','s-mixed','Write','',99,1,2,'')`); err != nil {
		t.Fatal(err)
	}
	// And the imported invocations lose their folder, as the upgrade left them.
	if _, err := db.sql.Exec(`UPDATE tool_uses SET project = ''
		WHERE name IN ('Skill','Grep')`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	calls := map[string]int{}
	rows, err := db.sql.Query(`SELECT
		CASE WHEN skill <> '' THEN skill ELSE name END, project, calls FROM tool_uses`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cap, proj string
		var c int
		if err := rows.Scan(&cap, &proj, &c); err != nil {
			t.Fatal(err)
		}
		got[cap] = proj
		calls[cap] = c
	}

	// THE PRE-MEASUREMENT TURNS ARE REPAIRED. Without this the project reports no usage at all.
	if got["superpowers:brainstorming"] != "/Users/v/notes" {
		t.Errorf("the pre-proxy skill invocation was not repaired (folder %q). On the live store "+
			"this left a project with 1,067 requests reporting zero capabilities used",
			got["superpowers:brainstorming"])
	}
	if got["Grep"] != "/Users/v/notes/server" {
		t.Errorf("the pre-proxy tool invocation was not repaired (folder %q)", got["Grep"])
	}
	// AND THE MEASURED INVOCATION IS UNTOUCHED, folder and count alike.
	if got["Write"] != "" || calls["Write"] != 99 {
		t.Errorf("the measured invocation was rewritten to folder %q calls %d; want '' and 99. "+
			"The proxy watched that traffic and a transcript is not evidence about it",
			got["Write"], calls["Write"])
	}
}

// B4: a dry-run backfill reports and writes nothing.
func TestBackfillIsDryUnlessApplied(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	if err := db.insertBatch([]*Event{{TS: daysAgo(9), SessionID: "s1", Model: "m",
		Source: SourceImported}}); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, home, "-p", "s1",
		turnLineCalling("s1", "/Users/v/p", daysAgo(9), `{"type":"tool_use","name":"Bash","input":{}}`))
	n, err := db.BackfillUseProjects(home, false)
	if err != nil || n != 1 {
		t.Fatalf("dry run n=%d err=%v; want 1 reported", n, err)
	}
	var rows int
	db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses`).Scan(&rows)
	if rows != 0 {
		t.Errorf("a dry run wrote %d tool_uses rows", rows)
	}
	if _, err := db.BackfillUseProjects(home, true); err != nil {
		t.Fatal(err)
	}
	db.sql.QueryRow(`SELECT COUNT(*) FROM tool_uses WHERE project='/Users/v/p'`).Scan(&rows)
	if rows != 1 {
		t.Errorf("apply wrote %d rows; want 1", rows)
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
