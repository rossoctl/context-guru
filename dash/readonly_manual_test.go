package dash

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A reader beside an OLDER proxy (the cache-campaign tool opens live databases read-only) must
// still read: the file lacks the Ping button's columns and a read-only handle cannot add them, so
// the connection shims them as constants. Nothing is written to the file.
func TestOpenReadOnlyReadsAFileThatPredatesTheManualPingColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ping := mkEvent(2, "s", "aws/claude-sonnet-5", 10, 10)
	ping.KeepAlive, ping.CostUSD = true, 0.02
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "aws/claude-sonnet-5", 10, 10), ping}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"manual_ping", "manual_saved_usd", "cache_miss_cause", "agent_compaction", "billed_token_factor"} {
		if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly on an older file: %v", err)
	}
	defer ro.Close()
	// Every ledger the new columns touch, over rows that predate them.
	if _, err := ro.Overview(Filter{}); err != nil {
		t.Fatalf("Overview: %v", err)
	}
	s, err := ro.SavingsTotals(Filter{TenantAll: true})
	if err != nil || s.ManualPings.Count != 0 || s.KeepAlivePingUSD != 0.02 {
		t.Fatalf("SavingsTotals = %+v, %v; want the automatic ping and no manual ones", s, err)
	}
	if _, err := ro.KeepAliveLedger(Filter{TenantAll: true}); err != nil {
		t.Fatalf("KeepAliveLedger: %v", err)
	}
	p, err := ro.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil || len(p.Requests) != 2 {
		t.Fatalf("Requests = %v, %v", p, err)
	}
	for _, e := range p.Requests {
		if e.ManualPing || e.ManualSavedUSD != 0 {
			t.Errorf("a row from before the columns reads manual_ping=%v saved=%v, want the defaults", e.ManualPing, e.ManualSavedUSD)
		}
	}
	// Still read-only: no write gets through, and the file kept its old shape.
	if _, err := ro.sql.Exec(`DELETE FROM main.requests`); err == nil {
		t.Error("a read-only handle accepted a write")
	}
	chk, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer chk.Close()
	var n int
	if err := chk.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name = 'manual_ping'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the file was altered (manual_ping present: %d, %v)", n, err)
	}

	// A current file gets no shim and reads as before.
	cur := filepath.Join(t.TempDir(), "cur.db")
	d, _ := Open(cur)
	d.Close()
	r2, err := OpenReadOnly(cur)
	if err != nil {
		t.Fatalf("a current file: %v", err)
	}
	r2.Close()
}
