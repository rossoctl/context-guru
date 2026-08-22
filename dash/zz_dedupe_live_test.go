package dash

// The prompt-text dedup against a REAL dashboard database, which is the only place its two
// risky properties can actually be judged: that the additive migration touches a
// production-sized v6 file without discarding anything, and that the reveal of 328,236 rows of
// real declarations is byte-identical afterwards.
//
// Skipped unless CG_SNAPSHOT_DB names a copy of one (the same variable TestSnapshotValueNumbers
// uses). The file is copied to a temp dir first, because this test MIGRATES and then REWRITES
// what it opens, and a snapshot someone else is reading must not be either.
//
//	CG_SNAPSHOT_DB=/path/to/copy-of-cg.db go test ./dash -run Dedupe -v
//
// The "before" side is read with the driver directly, without dash.Open, so the comparison is
// against the file as it stands today — not against something this code path has already
// migrated.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// revealHashes is every declaration's readable text, as a digest per row, keyed by the row's
// identity. Digests rather than the text itself: the real corpus is 254 MiB of blobs, and a
// sha256 of each is the same equality test at 1/1500th of the memory.
func revealHashes(t *testing.T, db *sql.DB, textCol, join, has string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT d.tenant_id, d.session_id, d.digest, d.kind, d.name, d.server, ` +
		textCol + ` FROM tool_declarations d ` + join + ` WHERE ` + has)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tenant, sess, dg, kind, name, server string
		var gz []byte
		if err := rows.Scan(&tenant, &sess, &dg, &kind, &name, &server, &gz); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(gunzipText(gz)))
		out[tenant+"\x00"+sess+"\x00"+dg+"\x00"+kind+"\x00"+name+"\x00"+server] =
			hex.EncodeToString(sum[:])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDedupeAgainstADeployedCopy(t *testing.T) {
	src := os.Getenv("CG_SNAPSHOT_DB")
	if src == "" {
		t.Skip("set CG_SNAPSHOT_DB to a copy of a dashboard database")
	}
	path := filepath.Join(t.TempDir(), "dedupe.db")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	// 1. The file as it is TODAY, read without dash.Open so nothing has migrated it: the row
	// counts, the columns, and the text every declaration reveals.
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var version string
	if err := raw.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var reqs, decls, withText int64
	var textBytes int64
	if err := raw.QueryRow(`SELECT (SELECT COUNT(*) FROM requests), COUNT(*),
		COUNT(text_gz), COALESCE(SUM(LENGTH(text_gz)), 0) FROM tool_declarations`).
		Scan(&reqs, &decls, &withText, &textBytes); err != nil {
		t.Fatal(err)
	}
	beforeCols := columnsOf(t, raw, "tool_declarations")
	// The OLD read shape: the text on the declaration row, no join, which is what the deployed
	// build serves. Anything this returns must still be returned, identically, at the end.
	before := revealHashes(t, raw, "d.text_gz", "", "d.text_gz IS NOT NULL")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if version != "6" {
		t.Fatalf("the copy is at schema version %s; this test is about migrating a v6 file", version)
	}
	if withText == 0 {
		t.Skip("this copy carries no declaration text; there is nothing to deduplicate")
	}
	t.Logf("before: %d requests, %d declaration rows, %d carrying text, %s of text, %d readable",
		reqs, decls, withText, mib(textBytes), len(before))

	// 2. Open it with THIS build. The migration must be additive: no version bump, no rename,
	// no lost row, no dropped column.
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path + ".v6.bak"); err == nil {
		t.Fatal("Open renamed the database aside; the migration is not additive")
	}
	var after string
	if err := db.sql.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != "6" {
		t.Errorf("schema version is now %s, want 6 — a bump discards every request row", after)
	}
	var reqs2, decls2 int64
	if err := db.sql.QueryRow(`SELECT (SELECT COUNT(*) FROM requests), COUNT(*)
		FROM tool_declarations`).Scan(&reqs2, &decls2); err != nil {
		t.Fatal(err)
	}
	if reqs2 != reqs || decls2 != decls {
		t.Errorf("opening the copy changed the row counts: %d/%d requests, %d/%d declarations",
			reqs2, reqs, decls2, decls)
	}
	afterCols := columnsOf(t, db.sql, "tool_declarations")
	for c := range beforeCols {
		if !afterCols[c] {
			t.Errorf("column tool_declarations.%s was dropped", c)
		}
	}
	if !afterCols["text_hash"] {
		t.Error("the migration did not add tool_declarations.text_hash")
	}
	// 3. The backfill, run to completion. Batches are larger and unpaced here — the pacing
	// exists to share a live write lock, and this copy has no other writer.
	sizeBefore, _ := db.sizeBytes()
	moved, err := db.dedupeDeclarationText(nil, 2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != withText {
		t.Errorf("migrated %d rows of %d carrying text", moved, withText)
	}

	// 4. THE ACCEPTANCE TEST: every row that revealed text before reveals the same text now.
	got := revealHashes(t, db.sql, declTextCol, declTextJoin, declHasText)
	if len(got) != len(before) {
		t.Errorf("%d rows reveal text now, %d did before", len(got), len(before))
	}
	var changed, lost int
	for k, want := range before {
		switch g, ok := got[k]; {
		case !ok:
			if lost++; lost < 4 {
				t.Errorf("row %q no longer reveals any text", k)
			}
		case g != want:
			if changed++; changed < 4 {
				t.Errorf("row %q reveals different text: %s -> %s", k, want, g)
			}
		}
	}
	if changed > 0 || lost > 0 {
		t.Fatalf("%d rows changed and %d lost their text", changed, lost)
	}

	var rows3, legacy, blobs, blobBytes int64
	if err := db.sql.QueryRow(`SELECT COUNT(*), COUNT(text_gz),
		(SELECT COUNT(*) FROM declaration_text),
		(SELECT COALESCE(SUM(LENGTH(text_gz)), 0) FROM declaration_text)
		FROM tool_declarations`).Scan(&rows3, &legacy, &blobs, &blobBytes); err != nil {
		t.Fatal(err)
	}
	if rows3 != decls {
		t.Errorf("%d declaration rows after the backfill, want %d — nothing may be deleted", rows3, decls)
	}
	if legacy != 0 {
		t.Errorf("%d rows still carry their own blob", legacy)
	}
	if blobBytes >= textBytes {
		t.Errorf("stored bytes did not shrink: %d -> %d", textBytes, blobBytes)
	}
	// And the pages come back: reclaim() is what the recorder calls after the backfill.
	prev := -1
	for i := 0; i < dedupeReclaimPasses; i++ {
		var free int
		if err := db.sql.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
			break
		}
		if free >= prev && prev >= 0 {
			break // incremental_vacuum can only truncate trailing pages; see dedupeLoop
		}
		prev = free
		if err := db.reclaim(); err != nil {
			t.Fatal(err)
		}
	}
	sizeAfter, _ := db.sizeBytes()
	t.Logf("after: %d rows preserved, %d distinct blobs, %s of text (was %s)",
		rows3, blobs, mib(blobBytes), mib(textBytes))
	t.Logf("database: %s -> %s", mib(sizeBefore), mib(sizeAfter))
	if sizeAfter >= sizeBefore {
		t.Errorf("the database did not shrink: %d -> %d bytes", sizeBefore, sizeAfter)
	}
}

func columnsOf(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

func mib(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
