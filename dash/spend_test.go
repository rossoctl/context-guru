package dash

import (
	"path/filepath"
	"testing"
	"time"
)

// spendEvents builds n priced requests for one tenant, split across sessions so the
// quota path has whole sessions to evict.
//
// Spread one hour apart by default — comfortably distinguishable, and far enough in the
// past to satisfy every retention-age check these tests also run. MonthToDateUSD always
// asks for the CURRENT real calendar month (time.Now(), not an injectable clock — see its
// own comment on why the rollup has to be real-time), so the naive version of this that
// unconditionally went `sessions` hours into the past would occasionally split its own
// fixture across two different tenant_spend rows: measured failing, deterministically,
// the first ~10 hours of a new calendar month, because the oldest sessions land in the
// PREVIOUS month while the newest still land in this one. Clamped to the room actually
// available since local UTC midnight on the 1st, so this is exact for the ~99.9% of the
// month that isn't within `sessions` hours of the boundary, and still correct — just more
// tightly packed — for the sliver that is.
func spendEvents(tenant string, sessions, turns int, usdEach float64) []*Event {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spacing := time.Hour
	if avail := now.Sub(monthStart); avail < time.Duration(sessions)*spacing {
		if spacing = avail / time.Duration(sessions+1); spacing < 10*time.Second {
			spacing = 10 * time.Second // floor: keep sessions and their turns from overlapping
		}
	}
	base := now.Add(-time.Duration(sessions) * spacing).UnixMilli()
	spacingMs := spacing.Milliseconds()
	var evs []*Event
	for s := 0; s < sessions; s++ {
		for k := 0; k < turns; k++ {
			evs = append(evs, &Event{
				TS: base + int64(s)*spacingMs + int64(k)*1000, TenantID: tenant,
				SessionID: tenant + ":s" + string(rune('a'+s)), Model: "m", Status: 200,
				CostUSD: usdEach, TokenAccounting: AccountingComplete,
			})
		}
	}
	return evs
}

// The cap must not be resettable by VOLUME. A tenant that spends its budget and then
// generates enough traffic to evict its own oldest rows used to see month-to-date spend
// fall back under the cap and its traffic resume.
func TestSpendSurvivesRowEviction(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.insertBatch(spendEvents("t1", 10, 4, 0.25)); err != nil {
		t.Fatal(err)
	}
	before, err := db.MonthToDateUSD("t1")
	if err != nil {
		t.Fatal(err)
	}
	if want := 10.0; before < want-1e-9 || before > want+1e-9 {
		t.Fatalf("month-to-date = %v, want %v", before, want)
	}
	// Evict most of the history, exactly as the row quota and the disk rule do.
	if _, err := db.DropOldestSessionsOfTenant("t1", 4); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows > 4 {
		t.Fatalf("the fixture did not evict anything (%d rows left)", rows)
	}
	after, err := db.MonthToDateUSD("t1")
	if err != nil {
		t.Fatal(err)
	}
	if after < before-1e-9 {
		t.Errorf("spend fell from $%.2f to $%.2f after retention deleted rows: "+
			"the cap can be reset by generating traffic", before, after)
	}
	// And retention by age must not refund either.
	if _, err := db.Prune(time.Now(), time.Minute, -1); err != nil {
		t.Fatal(err)
	}
	if usd, err := db.MonthToDateUSD("t1"); err != nil || usd < before-1e-9 {
		t.Errorf("spend after an age prune = $%.2f (err %v), want at least $%.2f", usd, err, before)
	}
	// A tenant with no traffic is $0, not an error.
	if usd, err := db.MonthToDateUSD("nobody"); err != nil || usd != 0 {
		t.Errorf("unknown tenant = %v, %v, want 0, nil", usd, err)
	}
}

// The row quota must MIGRATE, not destroy: with cold storage configured, a tenant
// trimmed to its quota has its oldest sessions uploaded and verified first — the same
// rule the disk-pressure path already followed.
func TestQuotaTrimArchivesInsteadOfDeleting(t *testing.T) {
	rec, m := archiveRecorder(t, Options{MaxRowsPerTenant: 8})
	if err := rec.db.insertBatch(spendEvents("heavy", 10, 4, 0.01)); err != nil {
		t.Fatal(err)
	}
	rec.enforceQuotas()

	rows, err := rec.db.tenantRowCount("heavy")
	if err != nil {
		t.Fatal(err)
	}
	if rows > 8 {
		t.Errorf("the tenant still holds %d rows, quota is 8", rows)
	}
	if m.puts == 0 {
		t.Error("the quota trim deleted rows without uploading anything to cold storage")
	}
	var archived int64
	if err := rec.db.sql.QueryRow(
		`SELECT COUNT(*) FROM archived_sessions WHERE full_path <> ''`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Error("nothing was recorded in the cold-storage index, so the history is unfindable")
	}
}

// A per-tenant quota a manager set must actually bind. It was audit-logged, rendered in
// the UI, and read by nothing: only the one global option was enforced.
func TestPerTenantRowQuotaIsEnforced(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db")}) // no global cap
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if err := rec.db.insertBatch(spendEvents("capped", 10, 4, 0)); err != nil {
		t.Fatal(err)
	}
	if err := rec.db.insertBatch(spendEvents("uncapped", 10, 4, 0)); err != nil {
		t.Fatal(err)
	}
	rec.SetTenantQuota(func(id string) int64 {
		if id == "capped" {
			return 5
		}
		return 0 // no quota of its own; no global default either
	})
	rec.enforceQuotas()

	capped, err := rec.db.tenantRowCount("capped")
	if err != nil {
		t.Fatal(err)
	}
	if capped > 5 {
		t.Errorf("the tenant holds %d rows, but a manager set its quota to 5", capped)
	}
	uncapped, err := rec.db.tenantRowCount("uncapped")
	if err != nil {
		t.Fatal(err)
	}
	if uncapped != 40 {
		t.Errorf("a tenant with no quota lost rows (%d of 40)", uncapped)
	}
}
