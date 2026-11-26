package dash

import (
	"fmt"
	"math"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// fbEvent builds an event priced through PriceMode with one mask op. fallback makes the library report the op
// unpriced, so PriceMode books no saving; record says whether the host stores that reason, as the proxy does
// for a request the accountant ran on.
func fbEvent(ts int64, session string, fallback, record bool) *Event {
	e := mkEvent(ts, session, "m", 5000, 4000)
	e.TenantID = ""
	e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens = 100, 1000, 0, 10
	e.Components = []CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 1000, SavedUnique: 1000}}
	res := accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c-" + session,
		Saved: accountant.TierTokens{CacheWrite5m: 3000}}}}
	if fallback {
		res.Unpriced = []accountant.Unpriced{{OpID: "x", Reason: "no_price"}}
	}
	e.SetAccountant(res)
	e.PriceMode(accTestPrice, true)
	if record {
		e.RecordAccountantFallback()
	}
	return e
}

func fbEvents(prefix string, base int64, n int, fallback, record bool) []*Event {
	var evs []*Event
	for i := 0; i < n; i++ {
		evs = append(evs, fbEvent(DayMs+(base+int64(i))*1000, fmt.Sprintf("%s%d", prefix, i), fallback, record))
	}
	return evs
}

// pricedShare is the share the Overview hero shows (dash/ui/savings.js overviewTotals, with no keep-alive
// pings): the priced saving over the priced spend plus the priced saving. Both leave the fallback rows out.
func pricedShare(o *Overview) float64 {
	fb := o.FallbackSaved
	saved := o.TotalSavedUSD - (fb.CompactionUSD + fb.DeferUSD + fb.KeepAliveUSD + fb.ReorderUSD + fb.FilterUSD)
	spent := o.CostUSD + o.CGLLMCostUSD - o.FallbackCostUSD
	if without := spent + saved; without > 0 {
		return 100 * saved / without
	}
	return 0
}

// #1004 (B8 of the #768 review): 10 fallback requests used to drop the share from 93.75% to 88.24%. They book
// cost and no saving. With their cost out of the denominator, the share is the priced requests' share.
func TestFallbackRequestsLeaveTheSavingShareAlone(t *testing.T) {
	db := openTestDB(t)
	priced := fbEvents("p", 0, 10, false, false)
	fell := fbEvents("f", 100, 10, true, true)
	if err := db.insertBatch(append(append([]*Event(nil), priced...), fell...)); err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, e := range priced {
		sessions = append(sessions, e.SessionID)
	}
	alone, err := db.Overview(Filter{TenantAll: true, SessionIn: sessions})
	if err != nil {
		t.Fatal(err)
	}
	all, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if alone.FallbackRequests != 0 || alone.FallbackCostUSD != 0 {
		t.Fatalf("priced only: fallback_requests=%d fallback_cost_usd=%v, want 0", alone.FallbackRequests, alone.FallbackCostUSD)
	}
	if all.FallbackRequests != 10 || all.Fallback[FallbackUnpriced] != 10 || len(all.Fallback) != 1 {
		t.Fatalf("fallback_requests=%d fallback=%v, want 10, all %q", all.FallbackRequests, all.Fallback, FallbackUnpriced)
	}
	var wantUSD float64
	for _, e := range fell {
		wantUSD += e.CostUSD + e.CGLLMCostUSD
	}
	if wantUSD <= 0 || math.Abs(all.FallbackCostUSD-wantUSD) > 1e-9 {
		t.Fatalf("fallback_cost_usd=%v, want %v (> 0)", all.FallbackCostUSD, wantUSD)
	}
	// The fallback rows still count as spend: the bill is the bill.
	if math.Abs(all.CostUSD-alone.CostUSD-wantUSD) > 1e-9 {
		t.Fatalf("cost_usd %v - %v != fallback cost %v", all.CostUSD, alone.CostUSD, wantUSD)
	}
	if a, b := pricedShare(alone), pricedShare(all); a <= 0 || math.Abs(a-b) > 1e-9 {
		t.Fatalf("share priced only %.4f%%, with fallback rows %.4f%%: want equal and > 0", a, b)
	}
}

// A request PriceMode marks as a fallback that the host did not record (no accountant record: no session, or the
// pipeline did not run) had nothing to price. It is not counted, and its cost stays in the share.
func TestUnrecordedFallbackIsNotCounted(t *testing.T) {
	db := openTestDB(t)
	e := fbEvent(DayMs, "n0", true, false)
	e.Components = nil
	if e.AccountantFallback() == "" || e.AccFallback != "" {
		t.Fatalf("setup: AccountantFallback=%q AccFallback=%q", e.AccountantFallback(), e.AccFallback)
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.FallbackRequests != 0 || o.FallbackCostUSD != 0 {
		t.Fatalf("fallback_requests=%d fallback_cost_usd=%v, want 0", o.FallbackRequests, o.FallbackCostUSD)
	}
}

// The stored reason reads back on the event.
func TestAccFallbackRoundTrips(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{fbEvent(DayMs, "r0", true, true)}); err != nil {
		t.Fatal(err)
	}
	pg, err := db.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Requests) != 1 || pg.Requests[0].AccFallback != FallbackUnpriced {
		t.Fatalf("read back %d events; want 1 with acc_fallback %q", len(pg.Requests), FallbackUnpriced)
	}
}

// A row written before acc_fallback existed is found from its components (priced_by 'none'), once.
func TestBackfillAccFallbackMarksOldFallbackRows(t *testing.T) {
	db := openTestDB(t)
	old := fbEvent(DayMs, "o0", true, false) // a fallback written by a build that stored no reason
	ok := fbEvent(DayMs+1000, "o1", false, false)
	if err := db.insertBatch([]*Event{old, ok}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM meta WHERE key = 'acc_fallback_backfilled'`); err != nil {
		t.Fatal(err)
	}
	if err := backfillAccFallback(db.sql); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.FallbackRequests != 1 || o.Fallback[FallbackUnrecorded] != 1 {
		t.Fatalf("after backfill: fallback_requests=%d fallback=%v, want 1 %q", o.FallbackRequests, o.Fallback, FallbackUnrecorded)
	}
	// Recorded in meta: a second run changes nothing, even for a row that would match.
	late := fbEvent(DayMs+2000, "o2", true, false)
	if err := db.insertBatch([]*Event{late}); err != nil {
		t.Fatal(err)
	}
	if err := backfillAccFallback(db.sql); err != nil {
		t.Fatal(err)
	}
	if o, _ = db.Overview(Filter{TenantAll: true}); o.FallbackRequests != 1 {
		t.Fatalf("second run marked %d rows, want 1: the backfill is once", o.FallbackRequests)
	}
}

// A bypassed request books no saving because the client asked for none, not because the library could not price
// it. RecordAccountantFallback stores no reason for it, so its cost stays in the share.
func TestBypassedRequestIsNotAFallback(t *testing.T) {
	e := fbEvent(DayMs, "b0", true, false)
	e.Bypassed = true
	e.RecordAccountantFallback()
	if e.AccountantFallback() == "" || e.AccFallback != "" {
		t.Fatalf("AccountantFallback %q, AccFallback %q; want a reason and none stored", e.AccountantFallback(), e.AccFallback)
	}
}

// Review of #1173: a fallback row is not always a $0 saving. It can keep a deferral figure and the measured
// filter credit, and its own model calls make its compaction net negative. Left in the numerator, these moved
// the share either way, up to 100%. The reads return them by part, so the share is the priced requests' share.
func TestFallbackSavingIsLeftOutToo(t *testing.T) {
	db := openTestDB(t)
	priced := fbEvents("p", 0, 10, false, false)
	fell := fbEvents("f", 100, 10, true, false)
	for _, e := range fell {
		e.CGLLMCostUSD = 0.002
		e.DeferNetUSD = 0.004 // a figure set before PriceMode, which a fallback leaves alone
		e.FilteredDeclTokens = 2000
		e.RecordAccountantFallback()
	}
	all := append(append([]*Event(nil), priced...), fell...)
	if err := db.insertBatch(all); err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, e := range priced {
		sessions = append(sessions, e.SessionID)
	}
	price := func(string) (modelinfo.Price, bool) { return accTestPrice, true }
	full := func(f Filter) *Overview {
		o, err := db.Overview(f)
		if err != nil {
			t.Fatal(err)
		}
		c, err := db.DeclCreditFor(f, price)
		if err != nil {
			t.Fatal(err)
		}
		o.SetDeclCredit(c)
		return o
	}
	alone := full(Filter{TenantAll: true, SessionIn: sessions})
	o := full(Filter{TenantAll: true})
	fb := o.FallbackSaved
	if math.Abs(fb.DeferUSD-0.04) > 1e-9 || math.Abs(fb.CompactionUSD+0.02) > 1e-9 || fb.FilterUSD <= 0 {
		t.Fatalf("fallback_saved %+v: want defer 0.04, compaction -0.02 (the model calls), filter > 0", fb)
	}
	if math.Abs(fb.FilterUSD-o.DeclFilterUSD) > 1e-9 {
		t.Fatalf("filter part %v, want the whole credit %v: only the fallback rows filtered", fb.FilterUSD, o.DeclFilterUSD)
	}
	if a, b := pricedShare(alone), pricedShare(o); a <= 0 || math.Abs(a-b) > 1e-9 {
		t.Fatalf("share priced only %.4f%%, with fallback rows %.4f%%: want equal and > 0", a, b)
	}
	// The lean read (the hero's first paint) carries the same count, cost and SQL parts. It has no filter part,
	// because it has no filter credit either.
	lean, err := db.OverviewLean(Filter{TenantAll: true, Since: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lean.FallbackRequests != o.FallbackRequests || math.Abs(lean.FallbackCostUSD-o.FallbackCostUSD) > 1e-9 ||
		math.Abs(lean.FallbackSaved.DeferUSD-fb.DeferUSD) > 1e-9 || math.Abs(lean.FallbackSaved.CompactionUSD-fb.CompactionUSD) > 1e-9 ||
		lean.FallbackSaved.FilterUSD != 0 {
		t.Fatalf("lean fallback %d %v %+v, full %d %v %+v", lean.FallbackRequests, lean.FallbackCostUSD, lean.FallbackSaved,
			o.FallbackRequests, o.FallbackCostUSD, fb)
	}
}
