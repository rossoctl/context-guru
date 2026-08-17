package dash

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Erasing one tenant, across the local database and cold storage.
//
// The property under test is not "the rows are gone" — that part is a DELETE. It is that
// NOTHING is left: no child rows whose parent went, no archive index rows pointing at
// objects that are still there, no objects with no index pointing at them, and nothing at
// all belonging to anybody else.

// A purge takes a tenant's requests, their component and content rows, their spend rollup,
// their archive index and the objects it names — and touches no other tenant.
func TestPurgeTenantClearsEverythingItOwns(t *testing.T) {
	rec, m := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 3, 48*time.Hour)
	seedSessionWithContent(t, rec.db, "t1", "sess-2", 2, 48*time.Hour)
	seedSessionWithContent(t, rec.db, "t2", "sess-other", 2, 48*time.Hour)

	// Move one of t1's sessions to cold storage, so the purge has an object to delete and
	// an index row that must not outlive it.
	cands, err := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveFull, 10)
	if err != nil {
		t.Fatal(err)
	}
	var moved bool
	for _, c := range cands {
		if c.SessionID == "sess-1" {
			if _, err := rec.ArchiveSessionFull(context.Background(), c); err != nil {
				t.Fatalf("ArchiveSessionFull: %v", err)
			}
			moved = true
		}
	}
	if !moved {
		t.Fatal("fixture did not archive sess-1")
	}
	if len(m.objects) != 1 {
		t.Fatalf("cold storage holds %d objects, want 1", len(m.objects))
	}

	res, err := rec.PurgeTenant(context.Background(), "t1")
	if err != nil {
		t.Fatalf("PurgeTenant: %v", err)
	}
	if res.Objects != 1 || res.Archives != 1 {
		t.Errorf("purge reported %d objects and %d archive rows, want 1 and 1: %+v",
			res.Objects, res.Archives, res)
	}
	// sess-1's rows left with the archive; sess-2's two requests were still local.
	if res.Requests != 2 {
		t.Errorf("purge reported %d requests, want 2 (sess-1 was already archived): %+v",
			res.Requests, res)
	}
	if len(m.objects) != 0 {
		t.Errorf("cold storage still holds %d of the tenant's objects", len(m.objects))
	}
	if has, err := rec.db.TenantHasRows("t1"); err != nil || has {
		t.Errorf("t1 still has rows (has=%v err=%v)", has, err)
	}
	comps, content, err := rec.db.OrphanRows()
	if err != nil {
		t.Fatal(err)
	}
	if comps != 0 || content != 0 {
		t.Errorf("purge left %d orphan component rows and %d orphan content rows", comps, content)
	}
	// The other tenant is entirely untouched, rows and content alike.
	if has, err := rec.db.TenantHasRows("t2"); err != nil || !has {
		t.Errorf("the purge took t2's rows too (has=%v err=%v)", has, err)
	}
	evs, err := rec.db.SessionEvents(Filter{Tenant: "t2"}, "sess-other", true)
	if err != nil || len(evs) != 2 {
		t.Fatalf("t2's session = %d events, %v", len(evs), err)
	}
	if len(evs[0].Content) == 0 || !strings.HasPrefix(evs[0].Content[0].Before, "TRANSCRIPT-") {
		t.Error("the purge removed another tenant's transcripts")
	}
}

// A cold-storage object that cannot be deleted must NOT have its index row removed. The
// index is the only record of what is in the bucket, so deleting it first would leave the
// object costing money forever with nothing able to find it.
func TestPurgeKeepsTheIndexWhenTheObjectSurvives(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{CaptureContent: true, ContentCap: 4096})
	seedSessionWithContent(t, rec.db, "t1", "sess-1", 2, 48*time.Hour)
	cands, err := rec.db.coldSessions(time.Now().UnixMilli(), ArchiveFull, 10)
	if err != nil || len(cands) == 0 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	if _, err := rec.ArchiveSessionFull(context.Background(), cands[0]); err != nil {
		t.Fatal(err)
	}
	// The condition is produced the way it actually happens in production: an unreachable
	// remote, which the host leaves nil after a failed boot probe. memRemote.Delete cannot
	// fail, so there is nothing to fake here.
	rec.remote = nil

	res, err := rec.PurgeTenant(context.Background(), "t1")
	if err == nil {
		t.Fatal("a purge that could not delete a cold object reported success")
	}
	if len(res.ObjectErrors) != 1 || !strings.Contains(err.Error(), "cold-storage") {
		t.Errorf("the failure does not name the object: %v / %+v", err, res.ObjectErrors)
	}
	if res.Archives != 0 {
		t.Errorf("the index row was deleted anyway, orphaning the object: %+v", res)
	}
	rows, err := rec.db.ArchivedSessions(Filter{Tenant: "t1"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("archive index has %d rows, want the one that could not be purged", len(rows))
	}
	// The tenant still "has rows" — the surviving archive row is one — which is exactly what
	// makes a retry the right next step rather than a confusing no-op.
	if has, err := rec.db.TenantHasRows("t1"); err != nil || !has {
		t.Errorf("the surviving archive row should still count as data (has=%v err=%v)", has, err)
	}
}

// "" is a legitimate tenant id in this schema — every single-tenant and pre-tenancy row
// carries it — so a purge with no tenant named must refuse rather than erase the
// deployment's own history.
func TestPurgeRefusesAnEmptyTenant(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{})
	seedSessionWithContent(t, rec.db, "", "local-sess", 2, time.Hour)

	if _, err := rec.PurgeTenant(context.Background(), ""); !errors.Is(err, ErrPurgeNoTenant) {
		t.Fatalf("purge with no tenant = %v, want ErrPurgeNoTenant", err)
	}
	if got := countRows(t, rec.db, "requests"); got != 2 {
		t.Errorf("%d single-tenant rows survived, want 2", got)
	}
}

// Purging a tenant with nothing stored is a success, not an error: it is what a manager
// pressing the button on a quiet account should get.
func TestPurgeTenantWithNothingStored(t *testing.T) {
	rec, _ := archiveRecorder(t, Options{})
	res, err := rec.PurgeTenant(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("PurgeTenant: %v", err)
	}
	if res.Removed() {
		t.Errorf("purge of an empty tenant reported removals: %+v", res)
	}
}

// The cascade this package relies on has to be ON, including for the in-memory databases
// every test here uses. It was not: the memory DSN omitted the pragma, so deleting a
// request left its children behind in tests while the real deployment removed them — the
// exact difference that hides an orphan bug.
func TestForeignKeyCascadeIsEnabledInMemory(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedSessionWithContent(t, db, "t1", "sess-1", 1, time.Hour)
	if _, err := db.sql.Exec(`DELETE FROM requests`); err != nil {
		t.Fatal(err)
	}
	comps, content, err := db.OrphanRows()
	if err != nil {
		t.Fatal(err)
	}
	if comps != 0 || content != 0 {
		t.Errorf("ON DELETE CASCADE is off in memory: %d component rows and %d content rows "+
			"outlived their request", comps, content)
	}
}
