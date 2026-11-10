package dash

import (
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
	build := func(ts int64, ledgerNet, search float64, heldReq, library bool) *Event {
		e := switchEvent()
		e.TS, e.SessionID = ts, "s"
		e.DeferTokens = 4000
		e.DeferGrossUSD, e.DeferNetUSD, e.DeferSearchUSD = ledgerNet+search, ledgerNet, search
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
	return build(1, 0.0016, 0, false, true), build(2, 0.0016, 0, true, true), build(3, 0.0016, 0, false, false)
}

func TestTheDeferralSavingIsOnTheDeferralCardAndTheTotalIsUnchanged(t *testing.T) {
	priced, held, unpriced := deferSplitEvents(t)
	if fb := priced.AccountantFallback(); fb != "" {
		t.Fatalf("the priced request fell back: %q", fb)
	}
	if !held.EstimateHeld {
		t.Fatal("the held request was not held")
	}
	if unpriced.AccountantFallback() == "" && unpriced.DeferNetUSD != 0.0016 {
		t.Fatalf("the unpriced request's lever %v, want the ledger's 0.0016", unpriced.DeferNetUSD)
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
	// requests keep the ledger's net, 0.0016 each. None of the three is zero or lost.
	wantLib := 5000 * 0.3e-6
	wantLever := wantLib + 0.0016 + 0.0016
	if !accNear(o.DeferNetUSD, wantLever) {
		t.Errorf("deferral lever %v, want %v: the library's %v plus the ledger's net on the held and unpriced requests",
			o.DeferNetUSD, wantLever, wantLib)
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
	if want := (1000+5000)*0.3e-6 + 0.0016 + 0.0016; !accNear(o.TotalSavedUSD, want) {
		t.Errorf("total %v, want %v", o.TotalSavedUSD, want)
	}
	if !accNear(s.TotalSavedUSD, o.TotalSavedUSD) {
		t.Errorf("SavingsTotals total %v != Overview %v", s.TotalSavedUSD, o.TotalSavedUSD)
	}
}
