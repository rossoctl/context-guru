package dash

import (
	"fmt"
	"testing"
	"time"
)

// DropOldestSessions binds its victim list in batches of sqlChunk ids, so a disk-rule pass
// that asks for more victims than SQLite's variable limit never fails on "too many SQL
// variables" (#775 review). The chunk size is shrunk here to exercise the batch boundaries:
// three batches, the last one partial.
func TestDropOldestSessionsChunksTheVictimList(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	evs := make([]*Event, 0, 7)
	for i := 0; i < 7; i++ {
		evs = append(evs, &Event{TS: now - int64(7-i)*3600_000,
			SessionID: fmt.Sprintf("s%d", i), Model: "m", Status: 200})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		putState(t, db, fmt.Sprintf("s%d", i))
	}
	old := sqlChunk
	sqlChunk = 3
	defer func() { sqlChunk = old }()
	n, err := db.DropOldestSessions(7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("deleted %d rows, want 7", n)
	}
	if got := countRows(t, db, "requests"); got != 0 {
		t.Errorf("%d request rows survived a chunked delete", got)
	}
	for i := 0; i < 7; i++ {
		if got := stateRowsFor(t, db, fmt.Sprintf("s%d", i)); got != 0 {
			t.Errorf("state rows under s%d survived: %d", i, got)
		}
	}
}

// DropOldestSessions removes exactly the sessions it picked, also when a request lands between
// the pick and the delete (#772). The probe lands one for the victim, newer than everything,
// so the victim is no longer the oldest session in the table: the old delete re-ran the
// ordering subquery itself and then removed a DIFFERENT session, while the state cleanup still
// acted on the picked one.
//
// This file is separate from sessionstate_test.go on purpose: it needs the dropProbe and
// sqlChunk seams, which origin/main lacks, so it cannot build there. The other session-state
// tests must keep building against origin/main, to show they fail on it by assertion and not
// by build.
func TestDropOldestSessionsDeletesExactlyThePickedSet(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	if err := db.insertBatch([]*Event{
		{TS: now - 2*3600_000, SessionID: "sess-victim", Model: "m", Status: 200},
		{TS: now - 60_000, SessionID: "sess-live", Model: "m", Status: 200},
	}); err != nil {
		t.Fatal(err)
	}
	putState(t, db, "sess-victim")
	putState(t, db, "sess-live")
	db.dropProbe = func() {
		if _, err := db.sql.Exec(`INSERT INTO requests(ts, session_id, model, status) VALUES(?,?,?,?)`,
			now, "sess-victim", "m", 200); err != nil {
			t.Errorf("probe insert: %v", err)
		}
	}
	if _, err := db.DropOldestSessions(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sess-victim", "sess-live"} {
		var n int
		_ = db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE session_id = ?`, id).Scan(&n)
		if id == "sess-victim" && n != 0 {
			t.Errorf("the picked session kept %d request rows", n)
		}
		if id == "sess-live" && n != 1 {
			t.Errorf("the live session kept %d request rows, want 1: the delete removed a set other than the picked one", n)
		}
	}
	if n := stateRowsFor(t, db, "sess-victim"); n != 0 {
		t.Errorf("the picked session kept %d state rows", n)
	}
	if n := stateRowsFor(t, db, "sess-live"); n != 2 {
		t.Errorf("the live session kept %d state rows, want 2", n)
	}
}
