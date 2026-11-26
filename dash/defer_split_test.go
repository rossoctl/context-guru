package dash

import (
	"path/filepath"
	"testing"

	"github.com/rossoctl/accountant"
)

// The review of #1003 measured one real dashboard in three groups of requests with deferral: priced by the
// accountant, priced with the estimate held, and not priced by it. This builds one of each, with the deferral
// figure the library books and the ledger's net, and reads them back through the Overview, SavingsTotals and
// /api/tool-defer. The figures are small, made up, and only meant to give each group a distinct, checkable
// deferral amount; they are not measurements.
func deferSplitEvents(t *testing.T) (priced, held, unpriced *Event) {
	t.Helper()
	build := func(ts int64, heldReq, library bool) *Event {
		e := switchEvent()
		e.TS, e.SessionID = ts, "s"
		e.DeferTokens = 4000
		if library {
			e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{
				{Operator: "context-guru.mask", ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 1000}},
				{Operator: "context-guru.tooldefer", ChangeID: "cg:tooldefer:x", Saved: accountant.TierTokens{CacheRead: 5000}},
			}})
			if heldReq {
				e.accRes.Unpriced = []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}
			}
		}
		e.PriceMode(accTestPrice, library)
		return e
	}
	return build(1, false, true), build(2, true, true), build(3, false, false)
}

func TestTheDeferralSavingIsOnTheDeferralCardAndTheTotalIsUnchanged(t *testing.T) {
	priced, held, unpriced := deferSplitEvents(t)
	if fb := priced.AccountantFallback(); fb != "" {
		t.Fatalf("the priced request fell back: %q", fb)
	}
	if !held.EstimateHeld {
		t.Fatal("the held request was not held")
	}
	if unpriced.AccountantFallback() == "" || unpriced.DeferNetUSD != 0 {
		t.Fatalf("the unpriced request: fallback %q, lever %v, want a fallback and no lever", unpriced.AccountantFallback(), unpriced.DeferNetUSD)
	}
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{priced, held, unpriced}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.SavingsTotals(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.ToolDeferReportFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}

	// The priced request's deferral figure is the library's 5000 cache-read tokens. The held and the unpriced
	// requests book none: one figure, the accountant's, and they have no figure.
	wantLib := 5000 * 0.3e-6
	if !accNear(o.DeferNetUSD, wantLib) {
		t.Errorf("deferral lever %v, want the library's %v on the priced request alone", o.DeferNetUSD, wantLib)
	}
	// One column, one meaning: /api/tool-defer and SavingsTotals read the very figure the Overview lever does.
	if !accNear(r.Total.NetUSD, o.DeferNetUSD) || !accNear(s.DeferNetUSD, o.DeferNetUSD) {
		t.Errorf("tool-defer net %v, savings %v, overview %v: they must agree", r.Total.NetUSD, s.DeferNetUSD, o.DeferNetUSD)
	}
	// The preset card is baseline - cost: the deferral figure left it.
	if wantPreset := 1000 * 0.3e-6; !accNear(o.BaselineCostUSD-o.CostUSD, wantPreset) {
		t.Errorf("baseline - cost = %v, want the preset's %v alone (the held and unpriced requests book none)",
			o.BaselineCostUSD-o.CostUSD, wantPreset)
	}
	// The total is the same as if the library's figure had stayed in the baseline.
	if want := (1000 + 5000) * 0.3e-6; !accNear(o.TotalSavedUSD, want) {
		t.Errorf("total %v, want %v", o.TotalSavedUSD, want)
	}
	if !accNear(s.TotalSavedUSD, o.TotalSavedUSD) {
		t.Errorf("SavingsTotals total %v != Overview %v", s.TotalSavedUSD, o.TotalSavedUSD)
	}
}

// The Tools page shows what the names list and the searches cost: gross less net, both from the accountant's one
// figure (#1441). A search turn is a spend on the tooldefer operator, so the cost column holds it, and the stored net
// is the component figure.
func TestTheToolsPageCostIsGrossLessTheOneFigure(t *testing.T) {
	const search = 10 * 15e-6
	e := deferralEvent(10, false)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	gross := 5000 * 0.3e-6
	e.TS, e.SessionID, e.DeferTokens = 1, "s", 4000
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	r, err := db.ToolDeferReportFor(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !accNear(r.Total.CostUSD, search) || !accNear(r.Total.GrossUSD, gross) || !accNear(r.Total.NetUSD, gross-search) {
		t.Errorf("gross %v cost %v net %v, want %v, %v and %v", r.Total.GrossUSD, r.Total.CostUSD, r.Total.NetUSD, gross, search, gross-search)
	}
}

// A row written before defer_cost_usd existed has the ledger's net in defer_net_usd, so its cost is exactly gross
// less net. The migration fills it, and a second run changes nothing.
func TestTheLedgerCostOfAnOlderRowIsFilledByTheMigrationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := switchEvent()
	e.TS, e.SessionID = 1, "s"
	e.DeferTokens, e.DeferGrossUSD, e.DeferNetUSD = 4000, 0.0030, 0.0021
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	// Put the row in the shape an older build left: the cost column is 0.
	if _, err := db.sql.Exec(`UPDATE requests SET defer_cost_usd = 0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := range 2 { // the second open is the idempotence check
		db, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var got float64
		if err := db.sql.QueryRow(`SELECT defer_cost_usd FROM requests`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !accNear(got, 0.0009) {
			t.Errorf("open %d: defer_cost_usd = %v, want gross less net, 0.0009", i+1, got)
		}
		db.Close()
	}
}
