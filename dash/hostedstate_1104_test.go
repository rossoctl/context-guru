package dash

import (
	"context"
	"testing"
	"time"
)

// #1104: hosted session keys are tenant-scoped ("<tenant>:<base>"). A purge or a quota trim of
// tenant t1 removes t1's session_state rows and leaves t2's, also for the same client thread id.
func TestPurgeTenantRemovesOnlyThatTenantsSessionState(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{})
	seedAliasedSession(t, rec.db, "t1", "t1:thread", "", 1, time.Hour)
	seedAliasedSession(t, rec.db, "t2", "t2:thread", "", 1, time.Hour)
	for _, s := range []string{"t1:thread", "t2:thread", "t10:thread"} {
		putState(t, rec.db, s)
	}
	if has, _ := rec.db.TenantHasRows("t1"); !has {
		t.Fatal("TenantHasRows must count t1's state rows")
	}
	res, err := rec.PurgeTenant(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionState != 2 {
		t.Errorf("SessionState = %d, want 2: %+v", res.SessionState, res)
	}
	if n := stateRowsFor(t, rec.db, "t1:thread"); n != 0 {
		t.Errorf("t1 kept %d state rows", n)
	}
	for _, other := range []string{"t2:thread", "t10:thread"} {
		if n := stateRowsFor(t, rec.db, other); n != 2 {
			t.Errorf("%s has %d state rows, want 2", other, n)
		}
	}
	if has, _ := rec.db.TenantHasRows("t1"); has {
		t.Error("t1 still has rows")
	}
}

func TestQuotaTrimOfATenantRemovesOnlyItsSessionState(t *testing.T) {
	db := openTestDB(t)
	seedAliasedSession(t, db, "t1", "t1:thread", "", 3, 2*time.Hour)
	seedAliasedSession(t, db, "t2", "t2:thread", "", 3, 3*time.Hour)
	putState(t, db, "t1:thread")
	putState(t, db, "t2:thread")
	if _, err := db.DropOldestSessionsOfTenant("t1", 0); err != nil {
		t.Fatal(err)
	}
	if n := stateRowsFor(t, db, "t1:thread"); n != 0 {
		t.Errorf("t1 kept %d state rows", n)
	}
	if n := stateRowsFor(t, db, "t2:thread"); n != 2 {
		t.Errorf("t2 has %d state rows, want 2", n)
	}
}
