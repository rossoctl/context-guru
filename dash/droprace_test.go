package dash

import (
	"testing"
	"time"
)

// DropOldestSessions removes exactly the sessions it picked, also when a request lands between
// the pick and the delete (#772). The probe lands one for the victim, newer than everything,
// so the victim is no longer the oldest session in the table: the old delete re-ran the
// ordering subquery itself and then removed a DIFFERENT session, while the state cleanup still
// acted on the picked one.
//
// This file is separate from sessionstate_test.go on purpose: it needs the dropProbe seam,
// which origin/main lacks, so it cannot build there. The other session-state tests must keep
// building against origin/main, to show they fail on it by assertion and not by build.
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
