package dash

import (
	"os"
	"testing"
)

// TestOpenDeployedCopy opens a SCRATCH COPY of the deployed database (never the live
// file) and asserts the two things that matter for shipping this to a running service:
// the additive tables and the GC trigger appear on an existing v6 file without
// discarding a single request row, and 14k rows of history with no names report as NOT
// CAPTURED rather than as "nothing unused".
func TestOpenDeployedCopy(t *testing.T) {
	const path = "/tmp/cgscratch/cg.db"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no scratch copy of a deployed database")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reqs, sess int
	if err := db.sql.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT session_id) FROM requests`).Scan(&reqs, &sess); err != nil {
		t.Fatal(err)
	}
	var trig int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger'
		AND name='trg_tool_inventory_gc'`).Scan(&trig); err != nil {
		t.Fatal(err)
	}
	if trig != 1 {
		t.Fatal("the GC trigger was not created on an existing database")
	}
	rep, err := db.ToolReportFor(Filter{TenantAll: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deployed copy: %d requests, %d sessions; report coverage %+v totals %+v",
		reqs, sess, rep.Coverage, rep.Totals)
	if reqs == 0 {
		t.Fatal("opening the copy discarded its request rows")
	}
	if rep.Coverage.Captured != 0 {
		t.Errorf("history cannot carry an inventory; got %d captured", rep.Coverage.Captured)
	}
	if rep.Coverage.Sessions == 0 || rep.Coverage.NotCaptured != rep.Coverage.Sessions {
		t.Errorf("every historical session must read as not captured: %+v", rep.Coverage)
	}
	if rep.Totals.UnusedTokens != 0 || rep.Totals.UnusedReads != 0 || rep.Totals.Priced {
		t.Errorf("history must claim nothing rather than zero: %+v", rep.Totals)
	}
}
