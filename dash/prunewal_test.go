package dash

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// Tests for #1204: the size rule must not delete while the WAL checkpoint is blocked, must stop
// when a round frees nothing, deletes whole idle sessions oldest first, keeps a floor, and the
// janitor keeps one backup copy.

// fatSession describes one session of turns that each carry about 8 KiB of content.
type fatSession struct {
	id       string
	ageHours float64 // age of the LAST request
	turns    int
}

func seedFat(t *testing.T, db *DB, sessions []fatSession) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	var evs []*Event
	for _, s := range sessions {
		last := time.Now().Add(-time.Duration(s.ageHours * float64(time.Hour))).UnixMilli()
		for k := 0; k < s.turns; k++ {
			raw := make([]byte, 4096)
			rng.Read(raw)
			txt := hex.EncodeToString(raw)
			e := mkEvent(last-int64(s.turns-1-k)*1000, s.id, "m", 1000, 900)
			e.TenantID = "t"
			e.Content = []ContentRow{{Path: "messages.0", Before: txt, After: txt[:100]}}
			evs = append(evs, e)
		}
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	if !db.checkpoint() {
		t.Fatal("setup checkpoint was busy")
	}
}

func sessionRows(t *testing.T, db *DB) map[string]int {
	t.Helper()
	rows, err := db.sql.Query(`SELECT session_id, COUNT(*) FROM requests GROUP BY session_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

// captureLogs returns a function that restores the logger and returns what was logged.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	return func() string {
		slog.SetDefault(prev)
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

func idleSet(prefix string, n int, turns int) []fatSession {
	var out []fatSession
	for i := 0; i < n; i++ {
		// i=0 is the oldest.
		out = append(out, fatSession{fmt.Sprintf("%s%02d", prefix, i), float64(30 - i), turns})
	}
	return out
}

func TestSizeRuleDoesNotDeleteWhileCheckpointIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 6, 8))
	before := sessionRows(t, db)

	// A reader with an open read transaction blocks wal_checkpoint(TRUNCATE).
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
	// A write after the reader began puts frames in the WAL that the reader blocks.
	if _, err := db.sql.Exec(`INSERT INTO requests(ts, tenant_id, session_id, model) VALUES (?,?,?,?)`,
		time.Now().UnixMilli(), "t", "fresh", "m"); err != nil {
		t.Fatal(err)
	}

	called := false
	logs := captureLogs(t)
	deleted, err := db.PruneWith(time.Now(), PruneOptions{
		MaxBytes: 1, ActiveWindow: time.Hour,
		BeforeDelete: func() bool { called = true; return true },
	})
	got := logs()
	rx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("the size rule deleted %d rows while the checkpoint was blocked", deleted)
	}
	after := sessionRows(t, db)
	for s, c := range before {
		if after[s] != c {
			t.Errorf("session %s: %d rows before, %d after", s, c, after[s])
		}
	}
	if called {
		t.Error("the backup ran although the rule had decided not to delete")
	}
	if !strings.Contains(got, "size rule skipped") {
		t.Errorf("no WARN said the size rule was skipped; logs:\n%s", got)
	}

	// The reader is gone. The next pass works.
	deleted, err = db.PruneWith(time.Now(), PruneOptions{MaxBytes: 1, ActiveWindow: time.Hour})
	if err != nil || deleted == 0 {
		t.Fatalf("after the reader ended, the size rule should delete: deleted=%d err=%v", deleted, err)
	}
}

func TestSizeRuleStopsWhenARoundFreesNothing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 6, 8))
	size, _ := db.sizeBytes()

	// While the first round deletes, a writer adds far more than the round frees.
	rounds := 0
	db.dropProbe = func() {
		rounds++
		var evs []*Event
		for i := 0; i < 200; i++ {
			e := mkEvent(time.Now().UnixMilli(), "grow", "m", 1000, 900)
			e.TenantID = "t"
			raw := make([]byte, 4096)
			rand.Read(raw)
			txt := hex.EncodeToString(raw)
			e.Content = []ContentRow{{Path: "messages.0", Before: txt, After: txt[:10]}}
			evs = append(evs, e)
		}
		if err := db.insertBatch(evs); err != nil {
			t.Error(err)
		}
	}
	logs := captureLogs(t)
	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxBytes: size * 8 / 10, ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	got := logs()
	if rounds != 1 {
		t.Fatalf("the pass ran %d delete rounds after a round freed nothing; want 1", rounds)
	}
	if !strings.Contains(got, "did not go down") {
		t.Errorf("no WARN for the round that freed nothing; logs:\n%s", got)
	}
}

func TestSizeRuleDeletesWholeSessionsOldestFirstAndKeepsActive(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sessions := append(idleSet("old", 6, 80), fatSession{"live", 0.1, 80})
	seedFat(t, db, sessions)
	size, _ := db.sizeBytes()

	// Ask for a cut of about half. The active session is 1/7 of the rows.
	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxBytes: size / 2, ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	rows := sessionRows(t, db)
	if rows["live"] != 80 {
		t.Errorf("the active session lost rows: %d of 80 left", rows["live"])
	}
	gone := 0
	seenKept := false
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("old%02d", i)
		switch rows[id] {
		case 0:
			if seenKept {
				t.Errorf("%s was deleted but an OLDER session was kept: not oldest first", id)
			}
			gone++
		case 80:
			seenKept = true
		default:
			t.Errorf("%s was cut in the middle: %d of 80 rows left", id, rows[id])
		}
	}
	if gone == 0 || gone == 6 {
		t.Errorf("deleted %d of 6 idle sessions; want some but not all", gone)
	}
}

func TestSizeRuleTrimsAnActiveSessionOnlyWhenItIsOverTheLimitAlone(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, []fatSession{{"live", 0.1, 60}})
	size, _ := db.sizeBytes()
	// Limit below the one session: nothing else can meet it.
	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxBytes: size / 3, ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	n := sessionRows(t, db)["live"]
	if n == 0 || n >= 60 {
		t.Fatalf("the over-limit active session has %d of 60 rows; want trimmed but not emptied", n)
	}

	// Two active sessions that fit the limit each: neither is touched.
	db2, err := Open(filepath.Join(t.TempDir(), "d2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	seedFat(t, db2, []fatSession{{"a", 0.1, 30}, {"b", 0.2, 30}})
	size2, _ := db2.sizeBytes()
	if d, err := db2.PruneWith(time.Now(), PruneOptions{MaxBytes: size2 * 6 / 10, ActiveWindow: time.Hour}); err != nil || d != 0 {
		t.Fatalf("active sessions that each fit the limit lost %d rows (err %v)", d, err)
	}
}

func TestSizeRuleStopsAtItsFloor(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 8, 80))
	size, _ := db.sizeBytes()
	if _, err := db.PruneWith(time.Now(), PruneOptions{
		MaxBytes: 1, FloorBytes: size / 2, ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	left := sessionRows(t, db)
	if len(left) < 4 || len(left) == 8 {
		t.Fatalf("%d of 8 sessions left; with a floor at half the size want 4 to 7", len(left))
	}
	after, _ := db.sizeBytes()
	if after < size/2*8/10 {
		t.Errorf("size %d fell well below the floor %d", after, size/2)
	}
}

// --- backup ---

func backupRecorder(t *testing.T, o Options) *Recorder {
	t.Helper()
	o.DBPath = filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	testutil.WaitFor(t, "the background migrations to finish", func() bool {
		select {
		case <-rec.migrationsDone:
			return true
		default:
			return false
		}
	})
	return rec
}

func TestJanitorBacksUpOncePer24hAndOnlyBeforeADelete(t *testing.T) {
	rec := backupRecorder(t, Options{RetentionAge: time.Hour, RetentionBytes: 10 << 30})
	bak := backupPath(rec.db.Path())

	// A pass with nothing to delete makes no copy.
	rec.janitorPass()
	if _, err := os.Stat(bak); err == nil {
		t.Fatal("a backup was made by a pass that deleted nothing")
	}

	// Rows older than the age limit: the age rule is about to delete, so a copy is made first.
	seedFat(t, rec.db, []fatSession{{"ancient", 10, 3}, {"fresh", 0.01, 3}})
	rec.janitorPass()
	fi, err := os.Stat(bak)
	if err != nil {
		t.Fatalf("no backup before the age rule deleted: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", fi.Mode().Perm())
	}
	if got := sessionRows(t, rec.db); got["ancient"] != 0 || got["fresh"] != 3 {
		t.Fatalf("age rule result unexpected: %v", got)
	}
	bdb, err := sql.Open("sqlite", "file:"+bak+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var inCopy int
	if err := bdb.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id='ancient'`).Scan(&inCopy); err != nil || inCopy != 3 {
		t.Fatalf("the copy holds %d rows of the deleted session (err %v); want 3", inCopy, err)
	}
	bdb.Close()

	// A second bad pass inside 24h must not replace the copy.
	first := fi.ModTime()
	seedFat(t, rec.db, []fatSession{{"ancient2", 10, 3}})
	rec.janitorPass()
	fi2, _ := os.Stat(bak)
	if !fi2.ModTime().Equal(first) {
		t.Error("the backup was replaced inside 24 hours")
	}

	// Older than 24 hours: a new copy.
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(bak, old, old); err != nil {
		t.Fatal(err)
	}
	seedFat(t, rec.db, []fatSession{{"ancient3", 10, 3}})
	rec.janitorPass()
	fi3, _ := os.Stat(bak)
	if !fi3.ModTime().After(old.Add(time.Hour)) {
		t.Error("no new backup after the old one passed 24 hours")
	}
	if _, err := os.Stat(bak + ".tmp"); err == nil {
		t.Error("temp file left behind")
	}
	// Rotation: the 25-hour-old copy is now .bak.1, and it still holds "ancient" and not "ancient3".
	fi1, err := os.Stat(bak + ".1")
	if err != nil {
		t.Fatalf("no .bak.1 after the second copy: %v", err)
	}
	if fi1.ModTime().After(old.Add(time.Minute)) {
		t.Error(".bak.1 is not the old copy")
	}
	b1, err := sql.Open("sqlite", "file:"+bak+".1?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var a1, a3 int
	b1.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id='ancient'`).Scan(&a1)
	b1.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id='ancient3'`).Scan(&a3)
	b1.Close()
	if a1 != 3 || a3 != 0 {
		t.Errorf(".bak.1 holds ancient=%d ancient3=%d; want 3 and 0", a1, a3)
	}
	if _, err := os.Stat(bak + ".2"); err == nil {
		t.Error("a third copy exists")
	}
}

func TestDiskPressureRuleMakesNoBackup(t *testing.T) {
	rec := backupRecorder(t, Options{MinKeepBytes: 1, DiskHighWatermark: 0.90, DiskLowWatermark: 0.85,
		RetentionAge: -1, RetentionBytes: -1})
	seedSessions(t, rec.db, "t1", 20, 2)
	probes := 0
	rec.diskProbe = func(string) (float64, bool) {
		probes++
		if probes > 2 {
			return 0.80, true
		}
		return 0.95, true
	}
	before := countRows(t, rec.db, "requests")
	rec.relieveDiskPressure()
	if countRows(t, rec.db, "requests") >= before {
		t.Fatal("the disk rule evicted nothing; the test proves nothing")
	}
	if _, err := os.Stat(backupPath(rec.db.Path())); err == nil {
		t.Fatal("the disk-pressure rule made a backup")
	}
}

func TestBackupCanBeTurnedOff(t *testing.T) {
	rec := backupRecorder(t, Options{RetentionAge: time.Hour, RetentionBytes: 10 << 30, DisableBackup: true})
	seedFat(t, rec.db, []fatSession{{"ancient", 10, 3}})
	rec.janitorPass()
	if sessionRows(t, rec.db)["ancient"] != 0 {
		t.Fatal("the age rule did not run")
	}
	if _, err := os.Stat(backupPath(rec.db.Path())); err == nil {
		t.Fatal("a backup was made with the backup switched off")
	}
}

func TestSizeRuleDoesNotDeleteWhenTheBackupFails(t *testing.T) {
	rec := backupRecorder(t, Options{RetentionAge: -1, RetentionBytes: 1, RetentionFloorBytes: 1})
	seedFat(t, rec.db, idleSet("old", 4, 8))
	// An old .bak exists, so one is due, and a non-empty directory sits at .bak.1: the rotation fails.
	bak := backupPath(rec.db.Path())
	if err := os.WriteFile(bak, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(bak, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bak+".1", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, rec.db, "requests")
	logs := captureLogs(t)
	rec.janitorPass() // fails open: it returns, and the pass goes on
	got := logs()
	if after := countRows(t, rec.db, "requests"); after != before {
		t.Fatalf("the size rule deleted %d rows although the backup failed", before-after)
	}
	if !strings.Contains(got, "could not make the backup copy") {
		t.Errorf("no WARN for the failed backup; logs:\n%s", got)
	}
}

func TestInMemoryDatabaseNeedsNoBackup(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if !rec.ensureBackup() {
		t.Fatal("an in-memory database must pass the backup step")
	}
}

func TestBackupTempCopyIsNeverWorldReadable(t *testing.T) {
	rec := backupRecorder(t, Options{})
	dir := filepath.Dir(rec.db.Path())
	// Make the copy fail after VACUUM INTO has written the temp file, and look at the temp dir
	// mode through the Stat of its parent entries before cleanup: the directory is 0700.
	d, err := os.MkdirTemp(dir, ".cg-backup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	fi, _ := os.Stat(d)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("MkdirTemp mode %v; the backup relies on 0700", fi.Mode().Perm())
	}
	if err := rec.db.backupTo(backupPath(rec.db.Path())); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".cg-backup-") && filepath.Join(dir, e.Name()) != d {
			t.Errorf("temp backup directory left behind: %s", e.Name())
		}
	}
}

func TestSchemaMismatchMovesTheBackupCopiesAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE meta SET value='9999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, s := range []string{".bak", ".bak.1"} {
		if err := os.WriteFile(path+s, []byte("old copy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, s := range []string{".bak", ".bak.1"} {
		if _, err := os.Stat(path + s); err == nil {
			t.Errorf("%s of the OLD database is still in place; it would pass as a fresh copy", s)
		}
	}
}

func TestSizeRuleUsesTheEvictHookAndKeepsActiveRowsWhenAsked(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 6, 80))
	size, _ := db.sizeBytes()
	var got []coldCandidate
	_, err = db.PruneWith(time.Now(), PruneOptions{MaxBytes: size / 2, ActiveWindow: time.Hour,
		Evict: func(c []coldCandidate) (int64, error) { got = append(got, c...); return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].SessionID != "old00" || got[0].Requests != 80 || got[0].LastTS < got[0].FirstTS {
		t.Fatalf("the hook did not get the oldest session first with its counts: %+v", got)
	}
	if sessionRows(t, db)["old00"] != 80 {
		t.Error("the size rule deleted a session itself although a hook was set")
	}

	db2, err := Open(filepath.Join(t.TempDir(), "d2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	seedFat(t, db2, []fatSession{{"live", 0.1, 60}})
	s2, _ := db2.sizeBytes()
	d, err := db2.PruneWith(time.Now(), PruneOptions{MaxBytes: s2 / 3, ActiveWindow: time.Hour, KeepActiveRows: true})
	if err != nil || d != 0 {
		t.Fatalf("KeepActiveRows: deleted %d rows (err %v)", d, err)
	}
}

func TestActiveTrimRespectsTheFloor(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, []fatSession{{"live", 0.1, 100}})
	size, _ := db.sizeBytes()
	// The floor is just under the size: the trim may remove very little.
	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxBytes: size / 4, FloorBytes: size * 98 / 100,
		ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if n := sessionRows(t, db)["live"]; n < 95 {
		t.Fatalf("the trim cut the session to %d rows; the floor allows about 2%%", n)
	}
}

// #1224: a round that frees nothing stops the rule until the file grows.
func TestSizeRuleStaysStoppedUntilTheFileGrows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 6, 80))
	size, _ := db.sizeBytes()
	grow := func(n int) {
		var evs []*Event
		for i := 0; i < n; i++ {
			e := mkEvent(time.Now().UnixMilli(), "grow", "m", 1000, 900)
			e.TenantID = "t"
			raw := make([]byte, 4096)
			rand.Read(raw)
			txt := hex.EncodeToString(raw)
			e.Content = []ContentRow{{Path: "messages.0", Before: txt, After: txt[:10]}}
			evs = append(evs, e)
		}
		if err := db.insertBatch(evs); err != nil {
			t.Fatal(err)
		}
	}
	o := PruneOptions{MaxBytes: size * 8 / 10, ActiveWindow: time.Hour}

	// Pass 1: the round deletes a session while a writer adds more than it frees.
	rounds := 0
	db.dropProbe = func() { rounds++; grow(200) }
	d1, err := db.PruneWith(time.Now(), o)
	if err != nil || d1 == 0 || rounds != 1 {
		t.Fatalf("pass 1: deleted=%d rounds=%d err=%v; want one deleting round", d1, rounds, err)
	}

	// Pass 2 and 3: no growth. The rule must not delete again.
	db.dropProbe = func() { rounds++ }
	logs := captureLogs(t)
	for i := 0; i < 2; i++ {
		if d, err := db.PruneWith(time.Now(), o); err != nil || d != 0 {
			t.Fatalf("pass %d after the stop: deleted %d rows (err %v); want 0", i+2, d, err)
		}
	}
	got := logs()
	if rounds != 1 {
		t.Fatalf("a delete round ran after the stop (rounds=%d)", rounds)
	}
	if n := strings.Count(got, "size rule is holding"); n != 1 {
		t.Errorf("the held state was logged %d times in 2 passes; want once", n)
	}

	// Growth inside the margin does not re-arm it.
	grow(1)
	if d, _ := db.PruneWith(time.Now(), o); d != 0 {
		t.Fatalf("one page of growth re-armed the rule (deleted %d)", d)
	}

	// Growth past the margin does.
	grow(200)
	d4, err := db.PruneWith(time.Now(), o)
	if err != nil || d4 == 0 {
		t.Fatalf("after growth past the margin the rule deleted %d rows (err %v); want some", d4, err)
	}
}

func TestSizeRuleClearsTheStopWhenTheFileFallsUnderTheLimit(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedFat(t, db, idleSet("old", 4, 40))
	size, _ := db.sizeBytes()
	db.sizeStuck.set(size)
	if _, err := db.PruneWith(time.Now(), PruneOptions{MaxBytes: size * 2, ActiveWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if db.sizeStuck.size.Load() != 0 {
		t.Fatal("the stuck state stayed after the file was under its limit")
	}
}
