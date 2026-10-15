package dash

import (
	"path/filepath"
	"testing"
)

func lifeRows(t *testing.T, db *DB) int64 { return countRows(t, db, "request_lifecycle") }

// A lifecycle row dies with its request (ON DELETE CASCADE), and the per-tenant quota counts
// requests rows only: lifecycle rows neither count toward it nor survive the trim.
func TestLifecycleRowsFollowRequestsAndQuotaCountsRequestsOnly(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), MaxRowsPerTenant: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	seedSessions(t, rec.db, "heavy", 20, 2) // 40 requests
	if _, err := rec.db.sql.Exec(`INSERT INTO request_lifecycle(request_id) SELECT id FROM requests`); err != nil {
		t.Fatal(err)
	}
	counts, err := rec.db.TenantRowCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["heavy"] != 40 {
		t.Fatalf("quota counted %d rows, want the 40 requests (lifecycle rows must not count)", counts["heavy"])
	}
	rec.enforceQuotas()
	reqs, life := countRows(t, rec.db, "requests"), lifeRows(t, rec.db)
	if reqs > 10 || life != reqs {
		t.Errorf("after trim: %d requests, %d lifecycle rows; want lifecycle == requests <= 10", reqs, life)
	}
}

// An existing database written before the table existed gets it from the additive DDL on open.
func TestLifecycleTableAddedToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedSessions(t, db, "t", 1, 1)
	if _, err := db.sql.Exec(`DROP TABLE request_lifecycle`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := lifeRows(t, db); n != 0 {
		t.Errorf("new table has %d rows", n)
	}
	if n := countRows(t, db, "requests"); n != 1 {
		t.Errorf("upgrade lost request rows: %d", n)
	}
}
