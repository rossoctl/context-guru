package dash

// The prompt-text dedup, from both ends: what the writer stores now, and what the backfill
// does to what the old writer stored.
//
// Every assertion here is about a TEXT, never about a column, and the reads go through the
// same declTextCol/declTextJoin/declHasText fragments the API uses. That is deliberate: the
// defect this change could cause is not "the table is wrong", it is "the reveal is empty or
// different", and a test that queried text_gz directly would have kept passing through exactly
// the mistake being guarded against.

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyRow writes a declaration the OLD way: the text on the row itself, no hash. This is the
// shape of the 328,236 rows on the live database, and the input the backfill exists for.
func legacyRow(t *testing.T, db *DB, session, name, text string) {
	t.Helper()
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts, text_gz)
		VALUES ('t1',?,'dg',?,?,'',10,1000,?)`,
		session, KindTool, name, gzipText(text)); err != nil {
		t.Fatal(err)
	}
}

// declRevealed is every declaration's text as the read side sees it, keyed by the row's
// identity. The equivalence check before and after the backfill is a comparison of two of
// these, and the API-level reveal (PromptViewFor) is one caller of the same fragments.
func declRevealed(t *testing.T, db *DB) map[string]string {
	t.Helper()
	rows, err := db.sql.Query(`SELECT d.session_id, d.digest, d.kind, d.name, d.server, ` +
		declTextCol + ` FROM tool_declarations d ` + declTextJoin + ` WHERE ` + declHasText)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sess, dg, kind, name, server string
		var gz []byte
		if err := rows.Scan(&sess, &dg, &kind, &name, &server, &gz); err != nil {
			t.Fatal(err)
		}
		out[strings.Join([]string{sess, dg, kind, name, server}, "\x00")] = gunzipText(gz)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// emptyDB is a file-backed store with no recorder, so a test can drive the backfill directly
// instead of racing the goroutine NewRecorder starts.
func emptyDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The whole point of the change: N sessions declaring the same tool store its schema ONCE.
func TestDeclarationTextIsStoredOncePerDistinctText(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	inv := ScanInventory("anthropic", sysBody(t, "You are Claude Code.",
		[]string{tool("Bash", "run a command")}, skillsReminder))
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	const sessions = 25
	for i := 0; i < sessions; i++ {
		rec.RecordInventory("t1", "s"+itoa(int64(i)), 1000, inv, true)
	}
	db := rec.DB()
	// Wait for the last session's rows rather than for a fixed time: this is the writer
	// goroutine, and the count below is only meaningful once all of them have landed.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(DISTINCT session_id) FROM tool_declarations`).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == sessions {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d sessions recorded", n, sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, withText := textRows(t, db)
	if rows < sessions || withText < sessions {
		t.Fatalf("%d rows, %d with text, want at least one per session", rows, withText)
	}
	// One blob per DISTINCT text. Every session declared the same set, so the number of stored
	// blobs must not grow with the number of sessions — that growth was 250 MB a day.
	var blobs int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM declaration_text`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs >= withText {
		t.Errorf("%d stored blobs for %d text-carrying rows: the text is still per row", blobs, withText)
	}
	if blobs > rows/sessions+2 {
		t.Errorf("%d stored blobs for %d distinct declarations in one set", blobs, rows/sessions)
	}
	// And nothing is written to the legacy column any more.
	var legacy int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations
		WHERE text_gz IS NOT NULL`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Errorf("%d rows still carry their own copy of the text", legacy)
	}
	// The reveal still resolves for every session, which is what the join is for.
	seedRequest(t, db, "t1", "s3")
	v, err := db.PromptViewFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Captured {
		t.Fatal("Captured false with text stored in declaration_text")
	}
	var bash string
	for _, r := range v.Regions {
		if r.Name == "Bash" {
			bash = r.Text
		}
	}
	if !strings.Contains(bash, "run a command") {
		t.Errorf("the Bash region came back as %q; the join does not resolve the text", bash)
	}
}

// The key is the sha256 of the text, which is what makes a collision the argument rather than a
// case to handle: two texts sharing a key would need a sha256 collision. The test pins the
// construction (a full 256-bit digest of the stored bytes) and proves distinct texts stay
// distinct and each resolves to its own.
func TestDeclarationTextKeyIsAFullContentHash(t *testing.T) {
	const a = "the first tool schema"
	const b = "the second tool schema"
	hash, gz := declText(a, true)
	if want := sha256.Sum256([]byte(a)); hash != hex.EncodeToString(want[:]) {
		t.Fatalf("key = %q, want the sha256 of the stored text", hash)
	}
	if len(hash) != 64 {
		t.Errorf("key is %d hex chars; a truncated digest is a collision surface", len(hash))
	}
	if gunzipText(gz) != a {
		t.Errorf("the blob stored under the key is not the text it hashes")
	}
	if h2, _ := declText(b, true); h2 == hash {
		t.Fatal("two different texts hashed to one key")
	}
	// Same text, same key — the property the dedup depends on, and not a given: the digest
	// column on tool_declarations is a maphash under a per-process seed and does NOT have it.
	if again, _ := declText(a, true); again != hash {
		t.Errorf("the same text hashed twice gave %q then %q", hash, again)
	}
	// And consent still decides whether there is a key at all.
	if h, gz := declText(a, false); h != "" || gz != nil {
		t.Errorf("declText returned a key without consent: %q", h)
	}
}

// The backfill's contract: the reveal is byte-identical afterwards, the bytes are stored once,
// and no row is lost.
func TestBackfillIsEquivalentAndCollapsesDuplicates(t *testing.T) {
	db := emptyDB(t)
	const schema = `{"name":"Bash","description":"run a command"}`
	for i := 0; i < 40; i++ {
		legacyRow(t, db, "s"+itoa(int64(i)), "Bash", schema)
		legacyRow(t, db, "s"+itoa(int64(i)), "Read", `{"name":"Read"}`)
	}
	// One row whose text is unique, so the test cannot pass by collapsing everything.
	legacyRow(t, db, "s0", "Odd", "a one-off declaration")

	before := declRevealed(t, db)
	if len(before) != 81 {
		t.Fatalf("fixture has %d revealed rows, want 81", len(before))
	}
	moved, err := db.dedupeDeclarationText(nil, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 81 {
		t.Errorf("migrated %d rows, want 81", moved)
	}
	after := declRevealed(t, db)
	if len(after) != len(before) {
		t.Fatalf("%d revealed rows before, %d after", len(before), len(after))
	}
	for k, want := range before {
		if got, ok := after[k]; !ok {
			t.Errorf("row %q lost its text", k)
		} else if got != want {
			t.Errorf("row %q: text changed\n before %q\n  after %q", k, want, got)
		}
	}
	var rows, legacy, blobs int
	if err := db.sql.QueryRow(`SELECT COUNT(*), COUNT(text_gz), (SELECT COUNT(*) FROM declaration_text)
		FROM tool_declarations`).Scan(&rows, &legacy, &blobs); err != nil {
		t.Fatal(err)
	}
	if rows != 81 {
		t.Errorf("%d declaration rows after the backfill, want all 81", rows)
	}
	if legacy != 0 {
		t.Errorf("%d rows still hold their own blob", legacy)
	}
	if blobs != 3 {
		t.Errorf("%d distinct blobs stored, want 3", blobs)
	}
}

// Interrupted, then resumed, then run again — and the reveal is identical at every step.
//
// This is the property the live migration is judged on: the process can stop anywhere, and what
// is on disk must still read the same. The stop channel is closed before the first batch, then
// the run is repeated one batch at a time.
func TestBackfillIsResumableAndIdempotent(t *testing.T) {
	db := emptyDB(t)
	for i := 0; i < 12; i++ {
		legacyRow(t, db, "s"+itoa(int64(i)), "Bash", `{"name":"Bash"}`)
	}
	want := declRevealed(t, db)

	// Stopped before it starts: nothing moved, everything readable.
	stopped := make(chan struct{})
	close(stopped)
	if moved, err := db.dedupeDeclarationText(stopped, 5, 0); err != nil || moved != 0 {
		t.Fatalf("a stopped run migrated %d rows (err %v)", moved, err)
	}
	assertRevealUnchanged(t, db, want, "after a run that was stopped immediately")

	// One batch at a time, with a stop between each: the interruption pattern a restart
	// produces. Every intermediate state is checked, because the mixed state — some rows
	// migrated, some not — is the one a single end-to-end run never exercises.
	var total int64
	for i := 0; i < 5; i++ {
		stop := make(chan struct{})
		// Closed after the pause begins, so exactly one batch runs per iteration.
		go func() { time.Sleep(20 * time.Millisecond); close(stop) }()
		n, err := db.dedupeDeclarationText(stop, 3, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		assertRevealUnchanged(t, db, want, "mid-migration")
	}
	if total == 0 || total > 12 {
		t.Fatalf("interrupted runs migrated %d rows of 12", total)
	}
	// Finish, then run twice more: the second and third runs must find nothing and change
	// nothing, which is what makes re-running it after a deploy safe.
	if _, err := db.dedupeDeclarationText(nil, 5, 0); err != nil {
		t.Fatal(err)
	}
	assertRevealUnchanged(t, db, want, "after finishing")
	for i := 0; i < 2; i++ {
		moved, err := db.dedupeDeclarationText(nil, 5, 0)
		if err != nil {
			t.Fatal(err)
		}
		if moved != 0 {
			t.Errorf("re-run %d migrated %d rows; nothing was left to move", i+1, moved)
		}
		assertRevealUnchanged(t, db, want, "after a re-run")
	}
	var blobs int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM declaration_text`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Errorf("%d blobs stored for 12 rows of one text", blobs)
	}
}

func assertRevealUnchanged(t *testing.T, db *DB, want map[string]string, when string) {
	t.Helper()
	got := declRevealed(t, db)
	if len(got) != len(want) {
		t.Fatalf("%s: %d revealed rows, want %d", when, len(got), len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Fatalf("%s: row %q reads %q (present=%v), want %q", when, k, g, ok, w)
		}
	}
}

// A blob the migration cannot read is LEFT ALONE. The rule the owner set is that nothing is
// deleted, and the failure mode it forbids is subtle: an unreadable blob decompresses to "",
// so hashing the decompressed text would file every corrupt row under one key, verify
// successfully against it, and clear real bytes it never understood.
func TestBackfillKeepsABlobItCannotRead(t *testing.T) {
	db := emptyDB(t)
	legacyRow(t, db, "s1", "Bash", `{"name":"Bash"}`)
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts, text_gz)
		VALUES ('t1','s2','dg',?, 'Corrupt','',10,1000,?)`,
		KindTool, []byte("this is not gzip")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.dedupeDeclarationText(nil, 10, 0); err != nil {
		t.Fatal(err)
	}
	var kept []byte
	if err := db.sql.QueryRow(`SELECT text_gz FROM tool_declarations
		WHERE name = 'Corrupt'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if string(kept) != "this is not gzip" {
		t.Errorf("the unreadable blob was changed to %q", kept)
	}
	// And the readable one beside it still migrated: one bad row must not stall the rest.
	var legacy int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations
		WHERE name = 'Bash' AND text_gz IS NOT NULL`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Error("the readable row was not migrated")
	}
}

// The wiring: opening a recorder on a database that still holds per-row text migrates it.
//
// Worth its own test because everything above drives dedupeDeclarationText directly, so all of
// it would keep passing if the recorder never called it — and then the 267 MB would sit there
// through every deploy.
func TestRecorderMigratesLegacyPromptTextOnStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		legacyRow(t, db, "s"+itoa(int64(i)), "Bash", `{"name":"Bash"}`)
	}
	want := declRevealed(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := NewRecorder(Options{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var legacy int
		if err := rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM tool_declarations
			WHERE text_gz IS NOT NULL`).Scan(&legacy); err != nil {
			t.Fatal(err)
		}
		if legacy == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d rows still carry their own text; the recorder never ran the backfill", legacy)
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertRevealUnchanged(t, rec.DB(), want, "after the recorder migrated on start")
}

// A transaction that fails must leave the writer's memo empty, or the NEXT one files a
// declaration against bytes that were rolled back.
//
// invWriter.storedText exists so the second session declaring Bash does not re-send its whole
// schema for a conflict to discard. That makes the memo a claim about what is ON DISK, and the
// only way it can lie is by remembering an insert whose transaction never committed — after
// which the reveal for those rows is empty and nothing anywhere is an error. The failure is
// injected with a trigger, because there is no other seam: every statement in write() succeeds
// on a healthy database, which is exactly why this branch would otherwise never be exercised.
func TestWriterForgetsTextFromARolledBackTransaction(t *testing.T) {
	db := emptyDB(t)
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	inv := ScanInventory("anthropic", sysBody(t, "You are Claude Code.",
		[]string{tool("Bash", "run a command"), tool("Boom", "the failing one")}, skillsReminder))
	if inv == nil {
		t.Fatal("no inventory scanned")
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER t_boom BEFORE INSERT ON tool_declarations
		WHEN NEW.name = 'Boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := w.write([]invMsg{{tenant: "t1", session: "s1", ts: 1000, inv: inv, text: true}}); err == nil {
		t.Fatal("the write succeeded; the fault was not injected")
	}
	if n := len(w.storedText); n != 0 {
		t.Errorf("the writer remembers %d blobs from a rolled-back transaction", n)
	}
	var blobs int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM declaration_text`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 0 {
		t.Errorf("%d blobs survived the rollback", blobs)
	}

	// Healthy again, and a fresh writer state for the session (the digest memo was not rolled
	// back either — but that only costs a re-write, where the text memo costs the text).
	if _, err := db.sql.Exec(`DROP TRIGGER t_boom`); err != nil {
		t.Fatal(err)
	}
	w.seen = map[string]*invSession{}
	if err := w.write([]invMsg{{tenant: "t1", session: "s1", ts: 1000, inv: inv, text: true}}); err != nil {
		t.Fatal(err)
	}
	texts := storedTexts(t, db)
	if len(texts) == 0 {
		t.Fatal("nothing readable after the retry")
	}
	for _, got := range texts {
		if got == "" {
			t.Error("a declaration reveals nothing: it points at bytes the rollback removed")
		}
	}
	var bash bool
	for _, got := range texts {
		if strings.Contains(got, "run a command") {
			bash = true
		}
	}
	if !bash {
		t.Error("the Bash schema is not readable after the retry")
	}
}
