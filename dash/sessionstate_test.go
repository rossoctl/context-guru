package dash

import (
	"context"
	"errors"
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

// stateRowsFor counts the state rows under one session or alias id: exactly the two keys
// putState writes, not every key the id is a substring of.
func stateRowsFor(t *testing.T, db *DB, session string) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM session_state WHERE key IN (?, ?)`,
		[]byte(store.SumPrefix+session), []byte(store.BilledPrefix+session)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedAliasedSession writes one session whose rows carry a content-derived alias id.
func seedAliasedSession(t *testing.T, db *DB, tenant, session, alias string, turns int, ageAgo time.Duration) {
	t.Helper()
	base := time.Now().Add(-ageAgo).UnixMilli()
	evs := make([]*Event, 0, turns)
	for i := 0; i < turns; i++ {
		evs = append(evs, &Event{
			TS: base + int64(i)*1000, TenantID: tenant, SessionID: session, SessionAlias: alias,
			Model: "m", Provider: "openai", Status: 200,
			TokensBefore: 100, TokensAfter: 90,
			Components: []CompRow{{Component: "format", Acted: true}},
			Content:    []ContentRow{{Path: "messages.0", Before: "TRANSCRIPT-" + session, After: "short"}},
		})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
}

// Evicting a session deletes the state under its content-derived alias too, when no other
// session uses it. An alias a live session still uses stays (#772): cg:seen: and cg:ttl: are
// keyed by the alias, so before this they outlived the session until retention.
func TestEvictionDeletesAliasState(t *testing.T) {
	db := openTestDB(t)
	seedAliasedSession(t, db, "t1", "sess-old", "alias-solo", 1, 4*time.Hour)
	seedAliasedSession(t, db, "t1", "sess-old", "alias-shared", 1, 3*time.Hour)
	seedAliasedSession(t, db, "t1", "sess-live", "alias-shared", 1, time.Hour)
	for _, s := range []string{"sess-old", "sess-live", "alias-solo", "alias-shared"} {
		putState(t, db, s)
	}
	if _, err := db.DropOldestSessions(1); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"sess-old", "alias-solo"} {
		if n := stateRowsFor(t, db, gone); n != 0 {
			t.Errorf("state rows under %s survived the eviction: %d", gone, n)
		}
	}
	for _, keep := range []string{"sess-live", "alias-shared"} {
		if n := stateRowsFor(t, db, keep); n != 2 {
			t.Errorf("state rows under %s = %d, want 2 (a live session reads them)", keep, n)
		}
	}
}

// A victim whose own id IS a live session's alias keeps its state (#775 review): a seesaw-id
// conversation has headerless rows under the alias as their session id and header rows that
// carry it as session_alias. Evicting the headerless half must not take cg:seen: and cg:ttl:
// away from the header half that is still going. The same eviction covers the other half of
// the in-use check: a victim's alias that is a live headerless session's own id stays too.
func TestEvictionKeepsStateOfAHeaderlessVictim(t *testing.T) {
	db := openTestDB(t)
	// Two victims: a headerless session whose id is the live header session's alias, and an
	// explicit-id session whose alias is a live headerless session's id.
	seedAliasedSession(t, db, "t1", "hdr-alias", "", 1, 5*time.Hour) // headerless victim
	seedAliasedSession(t, db, "t1", "v-old", "live-alias", 1, 4*time.Hour)
	seedAliasedSession(t, db, "t1", "hdr-live", "hdr-alias", 1, time.Hour)
	seedAliasedSession(t, db, "t1", "live-alias", "", 1, 30*time.Minute) // headerless, live
	for _, s := range []string{"hdr-alias", "v-old", "live-alias", "hdr-live"} {
		putState(t, db, s)
	}
	if _, err := db.DropOldestSessions(2); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hdr-alias", "v-old"} {
		var n int
		_ = db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = ?`, id).Scan(&n)
		if n != 0 {
			t.Errorf("victim %s kept %d request rows", id, n)
		}
	}
	// The victims' own ids and aliases that a live session still uses, in either direction.
	for _, keep := range []string{"hdr-alias", "live-alias", "hdr-live"} {
		if n := stateRowsFor(t, db, keep); n != 2 {
			t.Errorf("state rows under %s = %d, want 2 (a live session reads them)", keep, n)
		}
	}
	if n := stateRowsFor(t, db, "v-old"); n != 0 {
		t.Errorf("state rows under v-old = %d, want 0 (no live session uses it)", n)
	}
}

// The tenant row-quota trim deletes a dropped session's state with its rows, through the same
// in-use guard as the other delete paths (#775 review). Driven through trimTenantToQuota with
// no cold storage configured, which is the MaxRowsPerTenant path that reaches it.
func TestTenantQuotaTrimDeletesSessionState(t *testing.T) {
	rec, err := NewRecorder(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	seedAliasedSession(t, rec.db, "t1", "q-old", "q-alias", 3, 48*time.Hour)
	seedAliasedSession(t, rec.db, "t1", "q-live", "q-alias", 1, time.Hour)
	for _, s := range []string{"q-old", "q-live", "q-alias"} {
		putState(t, rec.db, s)
	}
	// The quota leaves exactly the live session's rows, so the oldest session must go.
	if _, err := rec.trimTenantToQuota("t1", 1); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = 'q-old'`).Scan(&n)
	if n != 0 {
		t.Fatalf("the trimmed session kept %d request rows", n)
	}
	_ = rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = 'q-live'`).Scan(&n)
	if n != 1 {
		t.Fatalf("the live session kept %d request rows, want 1", n)
	}
	if got := stateRowsFor(t, rec.db, "q-old"); got != 0 {
		t.Errorf("state rows under the trimmed session = %d, want 0", got)
	}
	for _, keep := range []string{"q-live", "q-alias"} {
		if got := stateRowsFor(t, rec.db, keep); got != 2 {
			t.Errorf("state rows under %s = %d, want 2 (a live session reads them)", keep, got)
		}
	}
}

// A database made before the index existed gets it on open (#775 review): without it, the
// state cleanup's in-use check scans the whole requests table per eviction pass.
func TestOpenAddsSessionAliasIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DROP INDEX IF EXISTS idx_requests_session_alias`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_requests_session_alias'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("an old database did not get idx_requests_session_alias on open")
	}
}

// The disk rule's fallback when a session cannot be archived: the session's rows AND state go,
// including the state under its alias, and an alias a live session uses stays (#770 follow-ups,
// #772). This is the delete branch of evictSessions, which had no test.
func TestEvictFallbackDeletesSessionAndAliasState(t *testing.T) {
	rec, m := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedAliasedSession(t, rec.db, "t1", "sess-old", "alias-solo", 1, 48*time.Hour)
	seedAliasedSession(t, rec.db, "t1", "sess-old", "alias-shared", 1, 47*time.Hour)
	seedAliasedSession(t, rec.db, "t1", "sess-live", "alias-shared", 1, time.Hour)
	for _, s := range []string{"sess-old", "sess-live", "alias-solo", "alias-shared"} {
		putState(t, rec.db, s)
	}
	m.failPut = errors.New("box is down")
	if _, err := rec.evictOldestSessions(1); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = 'sess-old'`).Scan(&left)
	if left != 0 {
		t.Fatalf("the evicted session kept %d request rows", left)
	}
	_ = rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = 'sess-live'`).Scan(&left)
	if left != 1 {
		t.Fatalf("the live session kept %d request rows, want 1", left)
	}
	for _, gone := range []string{"sess-old", "alias-solo"} {
		if n := stateRowsFor(t, rec.db, gone); n != 0 {
			t.Errorf("state rows under %s survived the failed archive: %d", gone, n)
		}
	}
	for _, keep := range []string{"sess-live", "alias-shared"} {
		if n := stateRowsFor(t, rec.db, keep); n != 2 {
			t.Errorf("state rows under %s = %d, want 2 (a live session reads them)", keep, n)
		}
	}
}
