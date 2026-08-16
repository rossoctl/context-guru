package dash

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedSessions writes n sessions of turns each, oldest session first, and returns
// the database once every row is durable.
func seedSessions(t *testing.T, db *DB, tenant string, n, turns int) {
	t.Helper()
	base := time.Now().Add(-time.Duration(n) * time.Hour).UnixMilli()
	evs := make([]*Event, 0, n*turns)
	for s := 0; s < n; s++ {
		for k := 0; k < turns; k++ {
			evs = append(evs, &Event{
				TS:        base + int64(s)*3600_000 + int64(k)*1000,
				TenantID:  tenant,
				SessionID: fmt.Sprintf("%s:sess-%03d", tenant, s),
				Model:     "m", Provider: "openai", Status: 200,
				TokensBefore: 100, TokensAfter: 90,
				Components: []CompRow{{Component: "format", Acted: true}},
			})
		}
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func sessionIDs(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.sql.Query(`SELECT DISTINCT session_id FROM requests ORDER BY session_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func countRows(t *testing.T, db *DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Eviction is session-granular: whole conversations disappear, oldest first, and
// nothing is left half-deleted.
func TestDropOldestSessionsIsWholeSessions(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedSessions(t, db, "t1", 5, 4)
	if got := countRows(t, db, "requests"); got != 20 {
		t.Fatalf("seeded %d rows, want 20", got)
	}

	n, err := db.DropOldestSessions(2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("deleted %d rows, want 8 (two whole 4-turn sessions)", n)
	}
	left := sessionIDs(t, db)
	if len(left) != 3 {
		t.Fatalf("sessions left = %v, want 3", left)
	}
	for _, want := range []string{"t1:sess-002", "t1:sess-003", "t1:sess-004"} {
		found := false
		for _, s := range left {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was evicted but is newer than the ones kept: %v", want, left)
		}
	}
	// Children must go with their parent, or the UI joins against orphans.
	if got := countRows(t, db, "request_components"); got != 12 {
		t.Errorf("component rows = %d, want 12 — CASCADE left orphans", got)
	}
}

// "Oldest" must mean last activity, not first: a long-running session still in use
// must not be evicted because it started a week ago.
func TestDropOldestSessionsUsesLastActivity(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	// "old-but-active" starts earliest and is still being used; "recent-and-done"
	// started later but stopped.
	if err := db.insertBatch([]*Event{
		{TS: now - 10*3600_000, SessionID: "old-but-active", Model: "m"},
		{TS: now - 60_000, SessionID: "old-but-active", Model: "m"},
		{TS: now - 5*3600_000, SessionID: "recent-and-done", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DropOldestSessions(1); err != nil {
		t.Fatal(err)
	}
	left := sessionIDs(t, db)
	if len(left) != 1 || left[0] != "old-but-active" {
		t.Fatalf("evicted the wrong session; left = %v", left)
	}
}

// The disk rule fires above the high watermark and stops at the low one — it must
// not keep grinding once pressure is relieved.
func TestDiskPressureEvictsWithHysteresis(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(Options{DBPath: filepath.Join(dir, "d.db"),
		MinKeepBytes: 1, DiskHighWatermark: 0.90, DiskLowWatermark: 0.85})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	seedSessions(t, rec.db, "t1", 40, 2)
	before := countRows(t, rec.db, "requests")

	// A disk that reports full until three eviction passes have happened.
	probes := 0
	rec.diskProbe = func(string) (float64, bool) {
		probes++
		if probes > 3 {
			return 0.80, true // relieved
		}
		return 0.95, true
	}
	rec.relieveDiskPressure()
	after := countRows(t, rec.db, "requests")
	if after >= before {
		t.Fatalf("nothing was evicted under disk pressure (%d -> %d)", before, after)
	}
	if after == 0 {
		t.Error("evicted everything; the low watermark did not stop the loop")
	}

	// Below the high watermark, a pass must do nothing at all.
	rec.diskProbe = func(string) (float64, bool) { return 0.87, true }
	mid := countRows(t, rec.db, "requests")
	rec.relieveDiskPressure()
	if got := countRows(t, rec.db, "requests"); got != mid {
		t.Errorf("evicted between the watermarks (%d -> %d); hysteresis is not working", mid, got)
	}
}

// At the floor, the janitor stops instead of emptying itself for a filesystem that
// is full for reasons unrelated to us.
func TestDiskPressureRespectsFloor(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(Options{DBPath: filepath.Join(dir, "d.db"),
		MinKeepBytes: 1 << 40}) // floor far above anything we could reach
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	seedSessions(t, rec.db, "t1", 10, 2)
	before := countRows(t, rec.db, "requests")
	rec.diskProbe = func(string) (float64, bool) { return 0.99, true } // permanently full
	rec.relieveDiskPressure()
	if got := countRows(t, rec.db, "requests"); got != before {
		t.Errorf("evicted below the floor (%d -> %d)", before, got)
	}
}

// A probe that cannot read the filesystem must disable the rule, not evict blindly.
func TestDiskPressureNoProbeNoEviction(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), MinKeepBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	seedSessions(t, rec.db, "t1", 5, 2)
	before := countRows(t, rec.db, "requests")
	rec.diskProbe = func(string) (float64, bool) { return 0, false }
	rec.relieveDiskPressure()
	if got := countRows(t, rec.db, "requests"); got != before {
		t.Errorf("evicted with no usable disk reading (%d -> %d)", before, got)
	}
}

// Nonsensical watermarks disable the rule rather than being acted on.
func TestDiskWatermarkValidation(t *testing.T) {
	for _, c := range []struct {
		high, low float64
		want      bool
	}{
		{0, 0, true},      // defaults
		{0.9, 0.85, true}, // sane
		{-1, 0, false},    // explicitly disabled
		{0.5, 0.9, false}, // low above high
		{0.8, 0.8, false}, // equal
		{1.5, 0.9, false}, // above 100%
	} {
		rec := &Recorder{opts: Options{DiskHighWatermark: c.high, DiskLowWatermark: c.low}}
		if _, _, ok := rec.diskWatermarks(); ok != c.want {
			t.Errorf("watermarks(high=%v low=%v) enabled=%v, want %v", c.high, c.low, ok, c.want)
		}
	}
}

// Fairness: a tenant over its quota is trimmed, and nobody else is touched.
func TestTenantRowQuotaTrimsOnlyTheOffender(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		MaxRowsPerTenant: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	seedSessions(t, rec.db, "heavy", 20, 2) // 40 rows, way over
	seedSessions(t, rec.db, "light", 2, 2)  // 4 rows, under

	counts, err := rec.db.TenantRowCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts["heavy"] <= 10 {
		t.Fatalf("the heavy tenant holds %d rows, expected to be over the quota of 10", counts["heavy"])
	}
	if counts["light"] > 10 {
		t.Fatalf("the light tenant holds %d rows, expected to be under the quota", counts["light"])
	}

	rec.enforceQuotas()

	var heavy, light int64
	if err := rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE tenant_id='heavy'`).Scan(&heavy); err != nil {
		t.Fatal(err)
	}
	if err := rec.db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE tenant_id='light'`).Scan(&light); err != nil {
		t.Fatal(err)
	}
	if heavy > 10 {
		t.Errorf("heavy tenant still holds %d rows, quota is 10", heavy)
	}
	if light != 4 {
		t.Errorf("light tenant lost rows (%d of 4) to another tenant's overuse", light)
	}
}

// A fresh database gets INCREMENTAL auto-vacuum, so reclaiming pages never needs a
// whole-file rewrite.
func TestFreshDatabaseUsesIncrementalAutoVacuum(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode int
	if err := db.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (INCREMENTAL); Prune would need a full VACUUM", mode)
	}
	seedSessions(t, db, "t", 4, 2)
	if _, err := db.DropOldestSessions(2); err != nil {
		t.Fatal(err)
	}
	if err := db.reclaim(); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}

// The size budget must account for the write-ahead log, which is real disk.
func TestSizeBytesIncludesWAL(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedSessions(t, db, "t", 30, 4)
	size, err := db.sizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	var pages, ps int64
	if err := db.sql.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA page_size`).Scan(&ps); err != nil {
		t.Fatal(err)
	}
	if size < pages*ps {
		t.Errorf("sizeBytes %d is below the main file's %d", size, pages*ps)
	}
}
