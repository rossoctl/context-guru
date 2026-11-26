package dash

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// declaration_text is customer data, so it goes when the last declaration that names it goes
// (#689). These tests drive each deletion path and check both halves of the rule: an orphaned
// text row is removed, and a text row another session or account still names is kept.

// declTextWriteFor stores the inventory of one session with its prompt text.
func declTextWriteFor(t *testing.T, db *DB, tenant, session string, tools ...string) {
	t.Helper()
	inv := ScanInventory("anthropic", sysBody(t, "You are Claude Code.", tools, skillsReminder))
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: tenant, session: session, ts: 1000, inv: inv, text: true}}); err != nil {
		t.Fatal(err)
	}
}

func declTextRevealed(t *testing.T, db *DB) string {
	t.Helper()
	return strings.Join(storedTexts(t, db), "\n")
}

func declTextRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM declaration_text`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Retention removes the text of an expired session and keeps what a live session still names.
func TestRetentionRemovesDeclarationTextButKeepsSharedText(t *testing.T) {
	db := emptyDB(t)
	declTextWriteFor(t, db, "t1", "old", tool("Bash", "run a command"), tool("OnlyOld", "unique-old-text"))
	declTextWriteFor(t, db, "t1", "live", tool("Bash", "run a command"))
	seedSessionWithContent(t, db, "t1", "old", 2, 48*time.Hour)
	seedSessionWithContent(t, db, "t1", "live", 2, time.Minute)

	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	got := declTextRevealed(t, db)
	if strings.Contains(got, "unique-old-text") {
		t.Error("the expired session's own tool text survived retention")
	}
	if !strings.Contains(got, "run a command") {
		t.Error("retention removed Bash text that the live session still names")
	}
	var live int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations WHERE session_id = 'live' AND text_hash IS NOT NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live == 0 {
		t.Fatal("the live session lost its declarations")
	}
}

// A session that still has request rows keeps its text, even when a trim removes some of them.
func TestPartialTrimKeepsDeclarationText(t *testing.T) {
	db := emptyDB(t)
	declTextWriteFor(t, db, "t1", "s1", tool("Bash", "run a command"))
	seedSessionWithContent(t, db, "t1", "s1", 3, time.Minute)
	if _, err := db.sql.Exec(`DELETE FROM requests WHERE id = (SELECT MIN(id) FROM requests)`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(declTextRevealed(t, db), "run a command") {
		t.Error("deleting one request row removed text the session still names")
	}
}

// An account purge removes that account's text, keeps text another account shares, and a second
// purge removes the rest.
func TestPurgeTenantRemovesDeclarationText(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{})
	db := rec.db
	declTextWriteFor(t, db, "t1", "s1", tool("Bash", "run a command"), tool("OnlyOne", "unique-t1-text"))
	declTextWriteFor(t, db, "t2", "s1", tool("Bash", "run a command"))
	seedSessionWithContent(t, db, "t1", "s1", 2, time.Minute)
	seedSessionWithContent(t, db, "t2", "s1", 2, time.Minute)

	if _, err := rec.PurgeTenant(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	got := declTextRevealed(t, db)
	if strings.Contains(got, "unique-t1-text") {
		t.Error("the purged account's tool text is still stored")
	}
	if !strings.Contains(got, "run a command") {
		t.Error("the purge removed text the other account still names")
	}

	if _, err := rec.PurgeTenant(context.Background(), "t2"); err != nil {
		t.Fatal(err)
	}
	if n := declTextRows(t, db); n != 0 {
		t.Errorf("%d declaration_text rows remain after both accounts were purged", n)
	}
}

// After the trigger removed a text row, the writer stores it again. A memo of "already stored"
// would point the new rows at bytes the purge removed.
func TestWriterRestoresTextAfterItWasPurged(t *testing.T) {
	db := emptyDB(t)
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	inv := ScanInventory("anthropic", sysBody(t, "You are Claude Code.", []string{tool("Bash", "run a command")}, skillsReminder))
	write := func(session string) {
		t.Helper()
		if err := w.write([]invMsg{{tenant: "t1", session: session, ts: 1000, inv: inv, text: true}}); err != nil {
			t.Fatal(err)
		}
	}
	write("s1")
	seedSessionWithContent(t, db, "t1", "s1", 1, time.Minute)
	if _, err := db.sql.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	if n := declTextRows(t, db); n != 0 {
		t.Fatalf("%d text rows remain after the session went", n)
	}
	write("s3")
	texts := storedTexts(t, db)
	if len(texts) == 0 {
		t.Fatal("nothing readable after the new write")
	}
	for _, got := range texts {
		if got == "" {
			t.Error("a declaration names text that is not stored")
		}
	}
}

// The start-up sweep removes text rows that no declaration names, and only those.
func TestSweepRemovesOnlyOrphanedDeclarationText(t *testing.T) {
	db := emptyDB(t)
	declTextWriteFor(t, db, "t1", "s1", tool("Bash", "run a command"))
	if _, err := db.sql.Exec(`INSERT INTO declaration_text(hash, text_gz, ts) VALUES ('orphan', x'00', 1)`); err != nil {
		t.Fatal(err)
	}
	before := declTextRows(t, db)
	n, err := db.sweepOrphanDeclarationText()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the sweep removed %d rows, want 1", n)
	}
	if after := declTextRows(t, db); after != before-1 {
		t.Errorf("rows %d -> %d, want one fewer", before, after)
	}
	if !strings.Contains(declTextRevealed(t, db), "run a command") {
		t.Error("the sweep removed text that a declaration names")
	}
}

// A database opened by an older build has the old trigger. Opening it again installs the new one.
func TestOpenReplacesTheOldInventoryTrigger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DROP TRIGGER trg_tool_inventory_gc;
		CREATE TRIGGER trg_tool_inventory_gc AFTER DELETE ON requests
		WHEN NOT EXISTS (SELECT 1 FROM requests WHERE session_id = OLD.session_id AND tenant_id = OLD.tenant_id)
		BEGIN DELETE FROM tool_declarations WHERE session_id = OLD.session_id AND tenant_id = OLD.tenant_id; END`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sqlText string
	if err := db.sql.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'trg_tool_inventory_gc'`).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlText, "declaration_text") {
		t.Error("the reopened database still has the old inventory trigger")
	}
}
