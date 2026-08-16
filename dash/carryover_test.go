package dash

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// mkV3Database builds a database stamped with an OLD schema version and puts rows in
// the two tables that are not derived, plus one request row that legitimately IS.
func mkV3Database(t *testing.T, path string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1000, "s1", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	if err := db.markArchived(
		coldCandidate{SessionID: "s-cold", TenantID: "acme", FirstTS: 10, LastTS: 20, Requests: 7},
		ArchiveFull, "archive/acme/2026/07/s-cold.full.jsonl.gz", 4096, "box:context-guru"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO tenant_spend(tenant_id,month,usd) VALUES ('acme','2026-08',123.45)`); err != nil {
		t.Fatal(err)
	}
	// Stamp the old version LAST, so everything above was written by the real DDL.
	if _, err := db.sql.Exec(`UPDATE meta SET value='3' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSchemaBumpCarriesNonDerivedTables is the blocker: a schema bump renames the old
// file aside and starts fresh, which is right for metrics and WRONG for the two tables
// nothing can recompute. Losing archived_sessions orphans every object already in cold
// storage; losing tenant_spend resets every tenant's month-to-date spend to zero.
func TestSchemaBumpCarriesNonDerivedTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cg.db")
	mkV3Database(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("a version mismatch must start fresh, not fail: %v", err)
	}
	defer db.Close()

	// The archive index survived, whole.
	rows, err := db.ArchivedSessions(Filter{TenantAll: true}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("archive index lost across the schema bump: %d rows, want 1 — every archived object is now orphaned", len(rows))
	}
	got := rows[0]
	if got.SessionID != "s-cold" || got.TenantID != "acme" || got.Requests != 7 ||
		got.FullPath != "archive/acme/2026/07/s-cold.full.jsonl.gz" || got.FullBytes != 4096 ||
		got.Remote != "box:context-guru" {
		t.Errorf("archive index row came across mangled: %+v", got)
	}

	// The spend rollup survived, to the cent.
	var usd float64
	if err := db.sql.QueryRow(
		`SELECT usd FROM tenant_spend WHERE tenant_id='acme' AND month='2026-08'`).Scan(&usd); err != nil {
		t.Fatalf("spend rollup lost across the schema bump: %v", err)
	}
	if usd != 123.45 {
		t.Errorf("month-to-date spend reset to %v, want 123.45 — the cap is now bypassable by restarting", usd)
	}

	// Metrics genuinely ARE derived, so they are expected to be gone.
	page, err := db.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Errorf("request rows should NOT be carried across; found %d", page.Total)
	}
}

// TestCarryNonDerivedNeverBlocksBoot: a preserved file that is missing, corrupt, or
// simply predates one of these tables must be logged and skipped, never fatal — the
// whole point of the rename-aside design is that the proxy boots.
func TestCarryNonDerivedNeverBlocksBoot(t *testing.T) {
	dir := t.TempDir()

	junk := filepath.Join(dir, "junk.bak")
	if err := os.WriteFile(junk, []byte("this is not a sqlite database at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A database that legitimately predates both tables: only `meta`.
	older := filepath.Join(dir, "older.bak")
	raw, err := sql.Open("sqlite", dsn(older))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO meta(key,value) VALUES ('schema_version','1')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for _, old := range []string{filepath.Join(dir, "gone.bak"), junk, older} {
		fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		carryNonDerived(fresh, old) // must not panic, must not block
		// And the fresh database must still be usable afterwards.
		if err := fresh.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 5)}); err != nil {
			t.Errorf("carrying from %s left the fresh database unusable: %v", old, err)
		}
		fresh.Close()
	}
}

// TestSchemaBumpWithOlderSchemaStillBoots is the same path end to end: a preserved
// file with no archived_sessions and no tenant_spend must still boot clean.
func TestSchemaBumpWithOlderSchemaStillBoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cg.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO meta(key,value) VALUES ('schema_version','2')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("an older schema without the non-derived tables must still boot: %v", err)
	}
	defer db.Close()
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 5)}); err != nil {
		t.Fatal(err)
	}
}
