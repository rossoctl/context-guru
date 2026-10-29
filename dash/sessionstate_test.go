package dash

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

func stateRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM session_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func putState(t *testing.T, db *DB, session string) {
	t.Helper()
	d, err := store.OpenDisk(db.SQL(), store.DiskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := store.NewMemory(store.Options{})
	m.AttachDisk(d)
	m.Put(store.SumPrefix+session, []byte("CHECKPOINT "+session))
	m.Put(store.BilledPrefix+session, []byte("1000"))
	d.Close() // flushes
}

// A database made before the table existed gets it on open.
func TestOldDatabaseGetsSessionStateTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DROP TABLE session_state`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := stateRows(t, db); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

// The dashboard's retention prune never deletes state rows: state has its own retention.
func TestPruneKeepsSessionState(t *testing.T) {
	db := openTestDB(t)
	seedSessions(t, db, "t1", 3, 2)
	putState(t, db, "sess-old")
	if _, err := db.Prune(time.Now().Add(365*24*time.Hour), time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "requests"); got != 0 {
		t.Fatalf("requests left = %d, the prune did not run", got)
	}
	if n := stateRows(t, db); n != 2 {
		t.Fatalf("state rows after the prune = %d, want 2", n)
	}
}

// The store's own sweep deletes only its own table.
func TestStateSweepLeavesDashboardTables(t *testing.T) {
	db := openTestDB(t)
	seedSessions(t, db, "t1", 2, 2)
	d, err := store.OpenDisk(db.SQL(), store.DiskOptions{Retention: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if got := countRows(t, db, "requests"); got != 4 {
		t.Fatalf("requests = %d, want 4", got)
	}
}

// Evicting a session deletes its state rows; other sessions keep theirs.
func TestEvictionDeletesSessionState(t *testing.T) {
	db := openTestDB(t)
	seedSessions(t, db, "t1", 2, 2) // sessions "t1:sess-000" (older) and "t1:sess-001"
	putState(t, db, "t1:sess-000")
	putState(t, db, "t1:sess-001")
	if _, err := db.DropOldestSessions(1); err != nil {
		t.Fatal(err)
	}
	if n := stateRows(t, db); n != 2 {
		t.Fatalf("state rows = %d, want 2 (only the survivor's)", n)
	}
	var left int
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM session_state WHERE CAST(key AS TEXT) LIKE '%sess-000%'`).Scan(&left)
	if left != 0 {
		t.Fatal("the evicted session kept its state")
	}
}

// Archiving a session to cold storage deletes its state rows.
func TestArchiveDeletesSessionState(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 3, 48*time.Hour)
	putState(t, rec.db, "sess-1")
	cands, err := rec.db.oldestLocalSessions(10)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	if _, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err != nil {
		t.Fatal(err)
	}
	if n := stateRows(t, rec.db); n != 0 {
		t.Fatalf("state rows after the archive = %d", n)
	}
}

// The dashboard writer and the state flush run at the same time on one handle: no error, no lost
// row. Run it under -race.
func TestDashboardWriterAndStateFlushTogether(t *testing.T) {
	db := openTestDB(t)
	d, err := store.OpenDisk(db.SQL(), store.DiskOptions{FlushEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m := store.NewMemory(store.Options{})
	m.AttachDisk(d)

	const sessions, turns = 40, 5
	var wg sync.WaitGroup
	errs := make(chan error, 2*sessions)
	wg.Add(2)
	go func() { // the dashboard: a batch per turn, as the recorder's writer does
		defer wg.Done()
		for s := 0; s < sessions; s++ {
			evs := make([]*Event, 0, turns)
			for k := 0; k < turns; k++ {
				evs = append(evs, &Event{TS: time.Now().UnixMilli() + int64(k), SessionID: fmt.Sprintf("s%d", s),
					Model: "m", Provider: "openai", Status: 200, TokensBefore: 10, TokensAfter: 9,
					Components: []CompRow{{Component: "format", Acted: true}}})
			}
			if err := db.insertBatch(evs); err != nil {
				errs <- err
			}
		}
	}()
	go func() { // the store: a state write per turn, flushed every millisecond
		defer wg.Done()
		for s := 0; s < sessions; s++ {
			m.Put(store.SumPrefix+fmt.Sprintf("s%d", s), []byte("cp"))
			d.Flush()
		}
	}()
	wg.Wait()
	d.Close()
	close(errs)
	for err := range errs {
		t.Errorf("write error: %v", err)
	}
	if n := stateRows(t, db); n != sessions {
		t.Errorf("state rows = %d, want %d", n, sessions)
	}
	if n := countRows(t, db, "requests"); n != sessions*turns {
		t.Errorf("request rows = %d, want %d", n, sessions*turns)
	}
	if d.Errors() != 0 {
		t.Errorf("state flush errors = %d", d.Errors())
	}
}

// A schema bump carries the session state across, like the archive index.
func TestSchemaBumpCarriesSessionState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cg.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	putState(t, db, "s1")
	if _, err := db.sql.Exec(`UPDATE meta SET value='3' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := stateRows(t, db); n != 2 {
		t.Fatalf("state rows after the schema bump = %d, want 2", n)
	}
}

// Deleting one session's state leaves the state of a session whose id only starts the same way.
func TestDeleteSessionStateKeepsPrefixSessions(t *testing.T) {
	db := openTestDB(t)
	putState(t, db, "abc")
	putState(t, db, "abcdef")
	if _, err := store.DeleteSessionState(db.SQL(), "abc"); err != nil {
		t.Fatal(err)
	}
	var kept int
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM session_state WHERE CAST(key AS TEXT) LIKE '%abcdef'`).Scan(&kept)
	if kept != 2 || stateRows(t, db) != 2 {
		t.Fatalf("rows = %d, abcdef rows = %d, want 2 and 2", stateRows(t, db), kept)
	}
}
