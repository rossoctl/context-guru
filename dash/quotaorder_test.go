package dash

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedAt writes sessions x turns rows for one tenant at an explicit base time, so a
// test can decide WHICH tenant owns the oldest rows in the table — the thing the
// global byte rule (ORDER BY ts ASC, no tenant column) selects on.
func seedAt(t *testing.T, db *DB, tenant string, sessions, turns int, base time.Time) {
	t.Helper()
	ms := base.UnixMilli()
	evs := make([]*Event, 0, sessions*turns)
	for s := 0; s < sessions; s++ {
		for k := 0; k < turns; k++ {
			evs = append(evs, &Event{
				TS:        ms + int64(s)*60_000 + int64(k)*1000,
				TenantID:  tenant,
				SessionID: fmt.Sprintf("%s:sess-%03d", tenant, s),
				Model:     "m", Provider: "openai", Status: 200,
				TokensBefore: 100, TokensAfter: 90,
				Components: []CompRow{{Component: "format", Acted: true}},
			})
		}
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatalf("insertBatch: %v", err)
	}
}

func tenantRows(t *testing.T, db *DB, tenant string) int64 {
	t.Helper()
	n, err := db.tenantRowCount(tenant)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Finding C. A janitor pass must trim the tenant over its row quota BEFORE the global
// byte rule, because the byte rule has no tenant column: it deletes the oldest rows in
// the whole table, so the tenant that filled the database keeps its recent history and
// the quiet tenant — whose rows are the oldest — loses all of it. Fairness first is what
// the rule order in janitor.go's doc comment has always claimed.
func TestPerTenantQuotaRunsBeforeTheGlobalByteRule(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		MaxRowsPerTenant: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	// The quiet tenant's rows are the OLDEST in the table; the heavy tenant's are
	// recent and 20x as many. Age retention is off so this is purely the byte rule.
	seedAt(t, rec.db, "light", 2, 10, time.Now().Add(-48*time.Hour))
	seedAt(t, rec.db, "heavy", 40, 10, time.Now().Add(-2*time.Hour))
	if got := tenantRows(t, rec.db, "light"); got != 20 {
		t.Fatalf("seeded %d rows for the quiet tenant, want 20", got)
	}

	// A byte budget the database is already over, so the global rule definitely fires.
	size, err := rec.db.sizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	rec.opts.RetentionAge, rec.opts.RetentionBytes = 0, size-1
	rec.diskProbe = func(string) (float64, bool) { return 0, false } // disk rule out of the picture

	rec.janitorPass()

	if light := tenantRows(t, rec.db, "light"); light != 20 {
		t.Errorf("the quiet tenant holds %d of its 20 rows: another tenant's overuse "+
			"evicted its history through the global byte rule", light)
	}
	if heavy := tenantRows(t, rec.db, "heavy"); heavy > 50 {
		t.Errorf("the heavy tenant still holds %d rows, quota is 50", heavy)
	}
}
