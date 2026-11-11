package dash

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// switchEvent is a priced-looking event with a mask component that acted and a library Result
// that saved 1000 cache-read tokens for it.
func switchEvent() *Event {
	e := &Event{Model: "m", FreshInput: 100, CacheRead: 1000, OutputTokens: 10, TokensBefore: 5000,
		Components: []CompRow{
			{Component: "mask", Acted: true, Mutated: true, SavedGross: 9999, SavedUnique: 9999},
			{Component: "idle"},
		}}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c",
		Saved: accountant.TierTokens{CacheRead: 1000}}}})
	return e
}

// costOnlyCopy is a copy priced with Price alone: its cost and no saving, which is what an event the
// accountant library could not price carries.
func costOnlyCopy(e *Event) *Event {
	c := *e
	c.Components = append([]CompRow(nil), e.Components...)
	c.Price(accTestPrice, true)
	return &c
}

func TestPriceMode_AccountantOverridesComponentsAndBaseline(t *testing.T) {
	e := switchEvent()
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("a qualifying event fell back: %q", fb)
	}
	want := 1000 * 0.3e-6
	if !accNear(e.Components[0].SavedUSD, want) || e.Components[1].SavedUSD != 0 {
		t.Fatalf("components = %+v, want mask %v", e.Components, want)
	}
	if !accNear(e.BaselineCostUSD-e.CostUSD, want) {
		t.Fatalf("baseline delta = %v want %v", e.BaselineCostUSD-e.CostUSD, want)
	}
	var sum float64
	for _, c := range e.Components {
		sum += c.SavedUSD
	}
	if d := sum - (e.BaselineCostUSD - e.CostUSD); d > 1e-9 || d < -1e-9 {
		t.Fatalf("components sum %v != baseline delta %v", sum, e.BaselineCostUSD-e.CostUSD)
	}
	l := costOnlyCopy(switchEvent())
	if e.CacheSavedUSD != l.CacheSavedUSD || e.CostUSD != l.CostUSD {
		t.Fatal("cache-saved or cost changed by the override")
	}
}

// The keep-alive rescue and the split saving come from the library, in their own columns.
func TestPriceMode_KeepAliveAndSplitComeFromTheLibrary(t *testing.T) {
	e := switchEvent()
	e.KeepAlivePings, e.SinceLastMs, e.SplitStableTokens = 1, 1<<40, 500 // legacy would credit both
	e.accRes.CacheEffects = []accountant.CacheEffect{
		{Kind: accountant.EffectRescue, ChangeID: "ka:main:w", Saved: accountant.TierTokens{CacheWrite5m: 200}, Caused: accountant.TierTokens{CacheRead: 200}},
		{Kind: accountant.EffectSplit, ChangeID: "cg:cachesplit:x", Saved: accountant.TierTokens{CacheWrite5m: 100}, Caused: accountant.TierTokens{CacheRead: 100}},
	}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	if !accNear(e.KeepAliveSavedUSD, 200*(3.75e-6-0.3e-6)) || !accNear(e.CachesplitSavedUSD, 100*(3.75e-6-0.3e-6)) {
		t.Fatalf("keep-alive %v split %v", e.KeepAliveSavedUSD, e.CachesplitSavedUSD)
	}
	if !accNear(e.BaselineCostUSD-e.CostUSD, 1000*0.3e-6) {
		t.Fatalf("the rescue or the split leaked into the baseline: delta %v", e.BaselineCostUSD-e.CostUSD)
	}
}

// With no rescue from the library, the keep-alive column is 0, whatever the gap and the pings.
func TestPriceMode_NoRescueMeansNoKeepAliveCredit(t *testing.T) {
	e := switchEvent()
	e.KeepAlivePings, e.SinceLastMs, e.KeepAliveRefreshed = 1, 1<<40, 1000 // the gaps the legacy rule credited on
	e.PriceMode(accTestPrice, true)
	if e.KeepAliveSavedUSD != 0 {
		t.Fatalf("keep-alive %v, want 0: the library found no rescue", e.KeepAliveSavedUSD)
	}
}

// The head-TTL legs go on the headttl row, inside the baseline delta.
func TestPriceMode_HeadTTLIsAComponentFigure(t *testing.T) {
	e := switchEvent()
	e.accRes.CacheEffects = []accountant.CacheEffect{{Kind: accountant.EffectTTLPremium, ChangeID: "headttl:main:x",
		Saved: accountant.TierTokens{CacheWrite5m: 100}, Caused: accountant.TierTokens{CacheWrite1h: 100}}}
	e.PriceMode(accTestPrice, true)
	var got float64
	var sum float64
	for _, c := range e.Components {
		sum += c.SavedUSD
		if c.Component == "headttl" {
			got = c.SavedUSD
		}
	}
	want := 100*3.75e-6 - 100*2*3e-6
	if !accNear(got, want) || !accNear(sum, e.BaselineCostUSD-e.CostUSD) {
		t.Fatalf("headttl %v want %v; rows %v delta %v", got, want, sum, e.BaselineCostUSD-e.CostUSD)
	}
}

func TestPriceMode_FallsBackToNoSaving(t *testing.T) {
	cases := map[string]struct {
		mut    func(*Event)
		reason string
	}{
		"no result":   {func(e *Event) { e.accRes = nil }, FallbackNoResult},
		"ops dropped": {func(e *Event) { e.SetAccountantContext(1, false) }, FallbackOpsDropped},
		"observe":     {func(e *Event) { e.SetAccountantContext(0, true) }, FallbackObserve},
		"unpriced": {func(e *Event) {
			e.accRes.Unpriced = []accountant.Unpriced{{OpID: "x", Reason: "r"}}
		}, FallbackUnpriced},
		"acted without figure": {func(e *Event) { e.Components[1].Acted = true }, FallbackActedWithoutFigure},
		"figure without row":   {func(e *Event) { e.Components = e.Components[1:] }, FallbackFigureWithoutRow},
		"saved above size":     {func(e *Event) { e.TokensBefore = 999 }, FallbackImplausibleSize},
		"size unknown":         {func(e *Event) { e.TokensBefore = 0 }, FallbackSizeUnknown},
		"duplicate names": {func(e *Event) {
			e.Components = append(e.Components, CompRow{Component: "mask"})
		}, FallbackDuplicateName},
		"check sum": {func(e *Event) {
			e.accRes.Checks = []accountant.CheckFailure{{Check: "sum", Detail: "section=messages ops=1800 bodies=900 bytes"}}
		}, FallbackCheckSum},
		"check unique_change": {func(e *Event) {
			e.accRes.Checks = []accountant.CheckFailure{{Check: "unique_change", Operator: "context-guru.mask"}}
		}, FallbackCheckUniqueChange},
		"check spans": {func(e *Event) {
			e.accRes.Checks = []accountant.CheckFailure{{Check: "spans", Operator: "context-guru.mask"}}
		}, FallbackCheckSpans},
	}
	for name, c := range cases {
		e := switchEvent()
		c.mut(e)
		want := costOnlyCopy(e)
		e.PriceMode(accTestPrice, true)
		if got := e.AccountantFallback(); got != c.reason {
			t.Errorf("%s: fallback reason %q, want %q", name, got, c.reason)
		}
		e.accFallback = ""    // the copy carries no reason; every other field must match
		e.accUnpricedWhy = "" // the cause of an unpriced reason is part of the reason, not of the figures
		// A fallback row is marked, so the read views do not value it with the legacy rule.
		for i := range e.Components {
			if e.Components[i].PricedBy != PricedByNone {
				t.Errorf("%s: component %q PricedBy %q, want %q", name, e.Components[i].Component, e.Components[i].PricedBy, PricedByNone)
			}
			e.Components[i].PricedBy = ""
		}
		if !reflect.DeepEqual(e, want) {
			t.Errorf("%s: event differs from the cost-only copy:\n got %+v\nwant %+v", name, e, want)
		}
	}
}

// conservation is informational: it fails when the usage does not bill the forwarded body, which
// the plausibility bound covers where it matters. It must not gate the override.
func TestPriceMode_ConservationCheckDoesNotGate(t *testing.T) {
	e := switchEvent()
	e.accRes.Checks = []accountant.CheckFailure{{Check: "conservation", Detail: "bill difference=10 ops=5 tokens"}}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" || !accNear(e.Components[0].SavedUSD, 1000*0.3e-6) {
		t.Fatalf("conservation gated the override: reason %q components %+v", fb, e.Components)
	}
}

// An envelope operator (an injected tool declaration) has a figure and no pipeline row: the override
// adds an envelope row for it, so the rows still sum to the baseline delta.
func TestPriceMode_EnvelopeOperatorGetsARow(t *testing.T) {
	e := switchEvent()
	e.accRes.Ops = append(e.accRes.Ops, accountant.PricedOp{Operator: "context-guru.expand", ChangeID: "cg:expand:x",
		Caused: accountant.TierTokens{CacheRead: 100}})
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	var row *CompRow
	var sum float64
	for i := range e.Components {
		sum += e.Components[i].SavedUSD
		if e.Components[i].Component == "expand" {
			row = &e.Components[i]
		}
	}
	if row == nil || row.Kind != KindEnvelope || !accNear(row.SavedUSD, -100*0.3e-6) {
		t.Fatalf("expand row %+v", row)
	}
	if !accNear(sum, e.BaselineCostUSD-e.CostUSD) {
		t.Fatalf("rows sum %v, baseline delta %v", sum, e.BaselineCostUSD-e.CostUSD)
	}
}

// A figure for a name that is neither a row nor an envelope operator still falls back.
func TestPriceMode_UnknownOperatorStillFallsBack(t *testing.T) {
	e := switchEvent()
	e.accRes.Ops = append(e.accRes.Ops, accountant.PricedOp{Operator: "context-guru.nosuch", ChangeID: "n",
		Saved: accountant.TierTokens{CacheRead: 1}})
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != FallbackFigureWithoutRow {
		t.Fatalf("fallback %q, want %q", fb, FallbackFigureWithoutRow)
	}
}

// The break an ended change left behind is counted on a row of its own.
func TestPriceMode_EndedChangeBreakHasARow(t *testing.T) {
	e := switchEvent()
	e.accRes.CacheEffects = []accountant.CacheEffect{{Kind: accountant.EffectBreak,
		Saved: accountant.TierTokens{CacheRead: 100}, Caused: accountant.TierTokens{CacheWrite5m: 100}}}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	brk := 100*0.3e-6 - 100*3.75e-6
	var got float64
	found := false
	for _, c := range e.Components {
		if c.Component == EndedChangeComponent {
			got, found = c.SavedUSD, true
		}
	}
	if !found || !accNear(got, brk) || !accNear(e.BaselineCostUSD-e.CostUSD, 1000*0.3e-6+brk) {
		t.Fatalf("ended_change %v (found %v), delta %v", got, found, e.BaselineCostUSD-e.CostUSD)
	}
}

// penalty_unknown (a thread's first request) does not gate: the figure applies and is an estimate
// (accountant design §6.4). Any other unpriced reason still gates (TestPriceMode_FallsBackToNoSaving).
func TestPriceMode_PenaltyUnknownIsAnEstimate(t *testing.T) {
	e := switchEvent()
	e.accRes.Unpriced = []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" || !e.AccountantEstimated() {
		t.Fatalf("fallback %q estimated %v, want the override applied as an estimate", fb, e.AccountantEstimated())
	}
	// Issue #915: the estimate is held apart. The books get no saving from it.
	if e.BaselineCostUSD != e.CostUSD {
		t.Errorf("an estimated request booked a baseline delta of %v, want 0", e.BaselineCostUSD-e.CostUSD)
	}
	for _, c := range e.Components {
		if c.SavedUSD != 0 || c.SavedFirstUSD != 0 || c.SavedReplayUSD != 0 {
			t.Errorf("component %s booked %v from an estimated request, want 0", c.Component, c.SavedUSD)
		}
	}
	if want := 1000 * 0.3e-6; !accNear(e.EstimatedSavedUSD, want) {
		t.Errorf("EstimatedSavedUSD = %v, want the held figure %v", e.EstimatedSavedUSD, want)
	}
	if !e.EstimateHeld {
		t.Error("EstimateHeld = false on a held request")
	}
	e.Price(accTestPrice, true)
	if e.AccountantEstimated() || e.EstimatedSavedUSD != 0 || e.EstimateHeld {
		t.Fatal("Price left the estimate state set")
	}
}

// A request the library could not price books no saving, and the read views must agree: the
// component tab must not value its row with the legacy rule (it looks like a row from before the
// switch otherwise: saved_gross > 0, saved_usd = 0).
func TestAFallbackRowIsNotEstimatedAtReadTime(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(DayMs+1, "s", "m1", 5000, 4000)
	e.TenantID = "t1"
	e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens = 100, 1000, 0, 10
	e.Components = []CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 9999, SavedUnique: 9999}}
	e.SetAccountant(accountant.Result{Unpriced: []accountant.Unpriced{{OpID: "x", Reason: "r"}}})
	e.PriceMode(accTestPrice, true)
	if e.AccountantFallback() != FallbackUnpriced {
		t.Fatalf("fallback %q, want unpriced", e.AccountantFallback())
	}
	if got := e.Components[0].PricedBy; got != PricedByNone {
		t.Fatalf("PricedBy %q, want %q", got, PricedByNone)
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	// The two read-time passes that value a row with the legacy rule when it has no stored figure.
	if err := db.EstimateComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Component != "mask" {
			continue
		}
		if r.SavedUSD != 0 || r.SavedUSDEstimated != 0 || r.SavedUSDEstimatedRows != 0 || r.NetUSDWithEstimate != r.NetUSD ||
			r.SavedUSDFirstRemoval != 0 || r.SavedUSDReplay != 0 {
			t.Fatalf("a fallback row was valued at read time: saved %v estimated %v (%d rows) net %v with-estimate %v",
				r.SavedUSD, r.SavedUSDEstimated, r.SavedUSDEstimatedRows, r.NetUSD, r.NetUSDWithEstimate)
		}
		return
	}
	t.Fatal("no mask row")
}

// An event with no usage or no price books no saving, and says why: an empty reason would read as
// "the override applied".
func TestPriceMode_AnUnpricedEventSaysWhyItBooksNoSaving(t *testing.T) {
	for name, tc := range map[string]struct {
		price    modelinfo.Price
		complete bool
		why      string
	}{
		"no usage": {accTestPrice, false, UnpricedNoUsage},
		"no price": {modelinfo.Price{}, true, UnpricedNoPrice},
	} {
		e := switchEvent()
		e.PriceMode(tc.price, tc.complete)
		if fb := e.AccountantFallback(); fb != FallbackUnpriced {
			t.Errorf("%s: fallback = %q, want %q", name, fb, FallbackUnpriced)
		}
		if w := e.UnpricedWhy(); w != tc.why {
			t.Errorf("%s: UnpricedWhy = %q, want %q (#939)", name, w, tc.why)
		}
	}
}

// #939. An operation the library could not price is the third cause of "unpriced", and it names itself.
// penalty_unknown is not one of them: it prices the event as an estimate and leaves no reason.
func TestPriceMode_UnpricedWhyNamesTheOperationTheLibraryCouldNotPrice(t *testing.T) {
	e := switchEvent()
	e.accRes.Unpriced = []accountant.Unpriced{
		{OpID: "op1", Reason: accountant.ReasonPenaltyUnknown},
		{OpID: "op2", Reason: "no_rate"},
	}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != FallbackUnpriced {
		t.Fatalf("fallback = %q, want %q", fb, FallbackUnpriced)
	}
	if w := e.UnpricedWhy(); w != "op2:no_rate" {
		t.Fatalf("UnpricedWhy = %q, want the operation that has no price", w)
	}

	est := switchEvent()
	est.accRes.Unpriced = []accountant.Unpriced{{OpID: "op1", Reason: accountant.ReasonPenaltyUnknown}}
	est.PriceMode(accTestPrice, true)
	if w := est.UnpricedWhy(); w != "" {
		t.Fatalf("an estimated event has UnpricedWhy = %q, want empty", w)
	}
}

// UnpricedWhy is for the unpriced reason only, and a second PriceMode call on the same event clears it.
func TestUnpricedWhyIsEmptyForEveryOtherReason(t *testing.T) {
	e := switchEvent()
	e.PriceMode(modelinfo.Price{}, true)
	if e.UnpricedWhy() == "" {
		t.Fatal("setup: the no-price event has no cause")
	}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb == FallbackUnpriced {
		t.Fatalf("setup: the repriced event still falls back as unpriced (%q)", fb)
	}
	if w := e.UnpricedWhy(); w != "" {
		t.Fatalf("after a repricing UnpricedWhy = %q, want empty", w)
	}

	// A different reason leaves it empty too: the cause belongs to unpriced alone.
	o := switchEvent()
	o.SetAccountantContext(1, false) // a dropped operation: ops_dropped
	o.PriceMode(accTestPrice, true)
	if fb := o.AccountantFallback(); fb != FallbackOpsDropped {
		t.Fatalf("setup: fallback = %q, want %q", fb, FallbackOpsDropped)
	}
	if w := o.UnpricedWhy(); w != "" {
		t.Fatalf("ops_dropped event has UnpricedWhy = %q, want empty", w)
	}
}

// The estimate flag outlives the process: it is written with the request and read back, so the
// dashboard marks the figure after a restart, and an exact figure stays unmarked.
func TestPriceEstimatedIsStoredAndReadBack(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	est, exact := switchEvent(), switchEvent()
	est.TS, est.SessionID = 1, "est"
	exact.TS, exact.SessionID = 2, "exact"
	est.accRes.Unpriced = []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}
	est.PriceMode(accTestPrice, true)
	exact.PriceMode(accTestPrice, true)
	if !est.PriceEstimated || exact.PriceEstimated {
		t.Fatalf("PriceMode set estimated=%v on the penalty_unknown event and %v on the exact one (fallbacks %q, %q)",
			est.PriceEstimated, exact.PriceEstimated, est.AccountantFallback(), exact.AccountantFallback())
	}
	if err := db.insertBatch([]*Event{est, exact}); err != nil {
		t.Fatal(err)
	}
	page, err := db.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	held := map[string]float64{}
	for _, e := range page.Requests {
		got[e.SessionID] = e.PriceEstimated
		held[e.SessionID] = e.EstimatedSavedUSD
	}
	if !accNear(held["est"], est.EstimatedSavedUSD) || est.EstimatedSavedUSD == 0 || held["exact"] != 0 {
		t.Fatalf("stored estimated_saved_usd = %v, want %v on est and 0 on exact", held, est.EstimatedSavedUSD)
	}
	if len(got) != 2 || !got["est"] || got["exact"] {
		t.Fatalf("stored price_estimated = %v, want est true and exact false", got)
	}
}

// The marker the keep-alive gate trusts (context-guru#912): set when the override applied, clear on a
// fallback, and stored with the request.
func TestAccountantPricedIsSetOnOverrideAndStored(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ok, fell := switchEvent(), switchEvent()
	ok.TS, ok.SessionID, fell.TS, fell.SessionID = 1, "ok", 2, "fell"
	fell.accRes = nil // no_result: the override cannot apply
	ok.PriceMode(accTestPrice, true)
	fell.PriceMode(accTestPrice, true)
	if !ok.AccountantPriced || fell.AccountantPriced {
		t.Fatalf("AccountantPriced = %v on the overridden event and %v on the fallback (%q)",
			ok.AccountantPriced, fell.AccountantPriced, fell.AccountantFallback())
	}
	if err := db.insertBatch([]*Event{ok, fell}); err != nil {
		t.Fatal(err)
	}
	page, err := db.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range page.Requests {
		got[e.SessionID] = e.AccountantPriced
	}
	if len(got) != 2 || !got["ok"] || got["fell"] {
		t.Fatalf("stored acc_priced = %v, want ok true and fell false", got)
	}
}

// Removed tool declarations are not message text. TokensBefore counts messages only, so a small
// conversation that drops a large tool list must not trip the plausibility bound (#907).
func TestPriceMode_ToolSectionTokensAreNotBoundedByMessageTokens(t *testing.T) {
	e := switchEvent()
	e.TokensBefore = 121
	e.accRes.Ops = []accountant.PricedOp{{Operator: "context-guru.toolfilter", ChangeID: "cg:toolfilter:x",
		Saved: accountant.TierTokens{CacheRead: 2188}}}
	e.Components = []CompRow{{Component: "idle"}}
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	if got := e.BaselineCostUSD - e.CostUSD; !accNear(got, 2188*0.3e-6) {
		t.Fatalf("delta = %v, want the declarations' saving", got)
	}
	// The bound still holds for messages: the same size on a message operation falls back.
	m := switchEvent()
	m.TokensBefore = 121
	m.accRes.Ops[0].Saved = accountant.TierTokens{CacheRead: 2188}
	m.PriceMode(accTestPrice, true)
	if fb := m.AccountantFallback(); fb != FallbackImplausibleSize {
		t.Fatalf("message operation: fallback %q, want %q", fb, FallbackImplausibleSize)
	}
}

// A markPremium effect is the write premium of a breakpoint the host added (accountant#49). It is a cost.
// Before the dashboard knew the kind, every request with the default tail breakpoint fell back with
// "unpriced" (effect:markPremium:unknown_kind) and booked no saving at all.
func TestPriceMode_MarkPremiumIsACostOnTheComponentThatAddedTheMark(t *testing.T) {
	premium := accountant.CacheEffect{Kind: accountant.EffectMarkPremium,
		Saved: accountant.TierTokens{Input: 2000}, Caused: accountant.TierTokens{CacheWrite5m: 2000}}
	// 2,000 tokens move from input ($3/M) to a 5m write ($3.75/M): a cost of 2000 x 0.75e-6.
	cost := 2000 * (3.75e-6 - 3e-6)
	run := func(changeID string) *Event {
		e := switchEvent()
		e.Components = append(e.Components, CompRow{Component: "tailbreakpoint", Acted: true, Mutated: true})
		ops := e.accRes.Ops
		ops = append(ops, accountant.PricedOp{Operator: "context-guru.tailbreakpoint", ChangeID: "cg:tailbreakpoint:x"})
		res := *e.accRes
		res.Ops = ops
		p := premium
		p.ChangeID = changeID
		res.CacheEffects = append(res.CacheEffects, p)
		e.SetAccountant(res)
		e.PriceMode(accTestPrice, true)
		return e
	}
	for name, changeID := range map[string]string{"owned": "cg:tailbreakpoint:x", "no owner": ""} {
		t.Run(name, func(t *testing.T) {
			e := run(changeID)
			if fb := e.AccountantFallback(); fb != "" {
				t.Fatalf("fell back: %q (a markPremium effect must be priced)", fb)
			}
			var tail, sum float64
			for _, c := range e.Components {
				sum += c.SavedUSD
				if c.Component == "tailbreakpoint" {
					tail = c.SavedUSD
				}
			}
			if !accNear(tail, -cost) {
				t.Fatalf("tailbreakpoint saved_usd = %v, want the premium as a cost %v", tail, -cost)
			}
			// The mask saving (1000 cache-read tokens) minus the premium is the whole delta, and the rows add up to it.
			want := 1000*0.3e-6 - cost
			if !accNear(e.BaselineCostUSD-e.CostUSD, want) || !accNear(sum, want) {
				t.Fatalf("baseline delta %v, component sum %v, want %v", e.BaselineCostUSD-e.CostUSD, sum, want)
			}
		})
	}
}

// #908, accountant R56, reviewer proposal on #1003: the library prices tool deferral from the bodies, and its
// figure is the deferral's saving. It is booked on the tooldefer row, so the components still sum to the baseline
// delta, but the lever Overview adds beside the baseline (DeferNetUSD) is where the deferral card reads it. So the
// library's tooldefer figure leaves the baseline (the preset card) and moves to the lever: the lever is that
// figure minus the search cost, and the total is the same as if the figure had stayed in the baseline.
func deferralEvent(search float64, held bool) *Event {
	e := switchEvent()
	e.DeferGrossUSD, e.DeferNetUSD, e.DeferSearchUSD = 0.0009, 0.0009-search, search
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{
		{Operator: "context-guru.mask", ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 1000}},
		{Operator: "context-guru.tooldefer", ChangeID: "cg:tooldefer:x", Saved: accountant.TierTokens{CacheRead: 5000}},
	}})
	if held {
		e.accRes.Unpriced = []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}
	}
	e.PriceMode(accTestPrice, true)
	return e
}

// total is what Overview reports for the event: the baseline delta plus the lever.
func eventTotal(e *Event) float64 { return e.BaselineCostUSD - e.CostUSD + e.DeferNetUSD }

func TestPriceMode_TheDeferralFigureMovesFromThePresetToTheLever(t *testing.T) {
	e := deferralEvent(0, false)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	var sum, def float64
	for _, c := range e.Components {
		sum += c.SavedUSD
		if c.Component == "tooldefer" {
			def = c.SavedUSD
		}
	}
	wantDef, wantMask := 5000*0.3e-6, 1000*0.3e-6
	if !accNear(def, wantDef) {
		t.Errorf("tooldefer row books %v, want the library's %v", def, wantDef)
	}
	// The preset card is baseline - cost: only the preset's own saving is left in it.
	if !accNear(e.BaselineCostUSD-e.CostUSD, wantMask) {
		t.Errorf("baseline delta %v, want the preset's %v alone", e.BaselineCostUSD-e.CostUSD, wantMask)
	}
	// The rows still sum to what is in the baseline and the lever together: nothing is counted twice.
	if !accNear(sum, e.BaselineCostUSD-e.CostUSD+e.DeferNetUSD) {
		t.Errorf("rows sum %v, baseline delta %v + lever %v", sum, e.BaselineCostUSD-e.CostUSD, e.DeferNetUSD)
	}
	if !accNear(e.DeferNetUSD, wantDef) {
		t.Errorf("lever %v, want the library's deferral figure %v", e.DeferNetUSD, wantDef)
	}
}

// The total does not change with the split: it is the same figure as when the deferral stayed in the baseline.
func TestPriceMode_MovingTheDeferralFigureLeavesTheTotalAlone(t *testing.T) {
	e := deferralEvent(0.0002, false)
	if want := (1000 + 5000) * 0.3e-6; !accNear(eventTotal(e), want-0.0002) {
		t.Errorf("total %v, want the library's %v less the search cost 0.0002", eventTotal(e), want)
	}
	// Against the same request with no search cost: the total falls by exactly the search cost.
	none := deferralEvent(0, false)
	if d := eventTotal(none) - eventTotal(e); !accNear(d, 0.0002) {
		t.Errorf("a search cost of 0.0002 moved the total by %v", d)
	}
}

// A request deferral did not change has no ledger figure, and the override leaves the lever at 0.
func TestPriceMode_NoDeferralLeavesTheLeverAtZero(t *testing.T) {
	e := switchEvent()
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{
		{Operator: "context-guru.mask", ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 1000}},
	}})
	e.PriceMode(accTestPrice, true)
	if e.DeferNetUSD != 0 {
		t.Errorf("lever %v, want 0", e.DeferNetUSD)
	}
}

// Loading a deferred tool later is a cost the bodies cannot show (the definition returns inline, in the response
// path). The ledger prices it as SearchCost and the lever carries it as a cost against the library's figure.
func TestPriceMode_ALoadedDeferredToolIsACostOnTheLever(t *testing.T) {
	none, loaded := deferralEvent(0, false), deferralEvent(0.0004, false)
	for _, e := range []*Event{none, loaded} {
		if fb := e.AccountantFallback(); fb != "" {
			t.Fatalf("fell back: %q", fb)
		}
	}
	if !accNear(none.BaselineCostUSD-none.CostUSD, loaded.BaselineCostUSD-loaded.CostUSD) {
		t.Errorf("the baseline moved with the search cost: %v vs %v",
			none.BaselineCostUSD-none.CostUSD, loaded.BaselineCostUSD-loaded.CostUSD)
	}
	if d := none.DeferNetUSD - loaded.DeferNetUSD; !accNear(d, 0.0004) {
		t.Errorf("levers %v and %v, want them to differ by the search cost 0.0004", none.DeferNetUSD, loaded.DeferNetUSD)
	}
}

// A held request books no break-affected saving: the library's figure goes to EstimatedSavedUSD, outside every
// total. The ledger's net must stay in the total as it did before, and the deferral must not appear in two places:
// not in the held estimate as well.
func TestPriceMode_AHeldRequestKeepsTheLedgersNetAndHoldsNoDeferralTwice(t *testing.T) {
	e := deferralEvent(0.0002, true)
	if !e.EstimateHeld {
		t.Fatal("the request was not held")
	}
	if !accNear(e.DeferNetUSD, 0.0009-0.0002) {
		t.Errorf("held lever %v, want the ledger's net %v", e.DeferNetUSD, 0.0009-0.0002)
	}
	if e.BaselineCostUSD != e.CostUSD {
		t.Errorf("a held request books a baseline delta of %v", e.BaselineCostUSD-e.CostUSD)
	}
	// The held estimate is the other components' figure only; the deferral part is the ledger's, in the total.
	if want := 1000 * 0.3e-6; !accNear(e.EstimatedSavedUSD, want) {
		t.Errorf("held estimate %v, want the preset's %v without the deferral", e.EstimatedSavedUSD, want)
	}
}

// A request the accountant could not price keeps the ledger's net exactly as before.
func TestPriceMode_AnUnpricedRequestKeepsTheLedgersNet(t *testing.T) {
	e := switchEvent()
	e.DeferGrossUSD, e.DeferNetUSD, e.DeferSearchUSD = 0.0009, 0.0007, 0.0002
	e.Model = "" // no price: the library's figure is not applied
	e.PriceMode(accTestPrice, true)
	if e.AccountantFallback() == "" {
		t.Skip("the event was priced; this test needs an unpriced one")
	}
	if e.DeferNetUSD != 0.0007 {
		t.Errorf("lever %v, want the ledger's net 0.0007", e.DeferNetUSD)
	}
}

// Issue #915 with the issue's numbers. The first request on an unseen warm cache: the client's body
// would have billed 8010 tokens read, 7966 written and 5 as input. The proxy removed 15976 tokens and
// the forwarded request billed only 5 as input. The library prices every removed token as a write (the
// ceiling). The warm size is not in the usage, so the true saving cannot be priced. It can be bounded:
// a client token never costs less than a read, so the floor is every removed token at the read price.
func TestPriceMode_HeldEstimateIsBoundedByReadAndWrite(t *testing.T) {
	p := modelinfo.Price{Input: 10e-6, CacheRead: 0.2e-6, CacheWrite: 2.5e-6, Output: 2e-6}
	e := &Event{Model: "m", FreshInput: 5, OutputTokens: 1, TokensBefore: 20000,
		Components: []CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 15976, SavedUnique: 15976}}}
	e.SetAccountant(accountant.Result{
		Unpriced: []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}},
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c",
			Saved: accountant.TierTokens{CacheWrite5m: 15976}}}})
	e.PriceMode(p, true)
	if !e.EstimateHeld {
		t.Fatal("the request was not held")
	}
	if e.BaselineCostUSD != e.CostUSD {
		t.Errorf("the held request booked a delta of %v, want 0 (booking is the owner's decision)", e.BaselineCostUSD-e.CostUSD)
	}
	high, low := 15976*2.5e-6, 15976*0.2e-6
	if !accNear(e.EstimatedSavedUSD, high) {
		t.Errorf("ceiling %v, want %v", e.EstimatedSavedUSD, high)
	}
	if !accNear(e.EstimatedSavedLowUSD, low) {
		t.Errorf("floor %v, want %v", e.EstimatedSavedLowUSD, low)
	}
	hand := 8010*0.2e-6 + 7966*2.5e-6 + 5*10e-6 + 1*2e-6 // the issue's hand baseline: 0.021569
	if !accNear(hand, 0.021569) {
		t.Fatalf("hand figure %v is not the issue's 0.021569", hand)
	}
	truth := hand - e.CostUSD
	if truth < e.EstimatedSavedLowUSD || truth > e.EstimatedSavedUSD {
		t.Errorf("true saving %v is outside the bounds [%v, %v]", truth, e.EstimatedSavedLowUSD, e.EstimatedSavedUSD)
	}
	e.Price(p, true)
	if e.EstimatedSavedLowUSD != 0 {
		t.Error("Price left the floor set")
	}
}

// The deferral's figure is out of the held ceiling, so its tokens are out of the floor: a held request that
// also deferred tools keeps a floor of the other removed tokens at the read rate, never a negative one.
func TestPriceMode_HeldFloorLeavesTheDeferralOut(t *testing.T) {
	p := modelinfo.Price{Input: 10e-6, CacheRead: 0.2e-6, CacheWrite: 2.5e-6, Output: 2e-6}
	e := &Event{Model: "m", FreshInput: 5, OutputTokens: 1, TokensBefore: 20000,
		Components: []CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 4000, SavedUnique: 4000}}}
	e.SetAccountant(accountant.Result{
		Unpriced: []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}},
		Ops: []accountant.PricedOp{
			{Operator: "context-guru.mask", ChangeID: "c", Saved: accountant.TierTokens{CacheWrite5m: 4000}},
			{Operator: "context-guru.tooldefer", ChangeID: "cg:tooldefer:x", Saved: accountant.TierTokens{CacheWrite5m: 12000}}}})
	e.PriceMode(p, true)
	if !e.EstimateHeld {
		t.Fatal("the request was not held")
	}
	if want := 4000 * 2.5e-6; !accNear(e.EstimatedSavedUSD, want) {
		t.Errorf("ceiling %v, want %v without the deferral", e.EstimatedSavedUSD, want)
	}
	if want := 4000 * 0.2e-6; !accNear(e.EstimatedSavedLowUSD, want) {
		t.Errorf("floor %v, want %v without the deferral", e.EstimatedSavedLowUSD, want)
	}
}

// The floor and the ceiling are stored with the request and summed on both Overview paths.
func TestOverviewSumsTheHeldFloor(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 2; i++ {
		e := &Event{TS: now - int64(i)*1000, SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 5000, TokensAfter: 4000, FreshInput: 5, OutputTokens: 1,
			TokenAccounting: AccountingComplete, PriceEstimated: true, EstimateHeld: true,
			EstimatedSavedUSD: 0.04, EstimatedSavedLowUSD: 0.003}
		insertReq(t, db, e)
	}
	f := Filter{TenantAll: true, Since: now - 60_000}
	full, err := db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	lean, err := db.OverviewLean(f, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]*Overview{"full": full, "lean": lean} {
		if !accNear(o.EstimatedSavedLowUSD, 0.006) || !accNear(o.EstimatedSavedUSD, 0.08) {
			t.Errorf("%s: floor %v ceiling %v, want 0.006 and 0.08", name, o.EstimatedSavedLowUSD, o.EstimatedSavedUSD)
		}
	}
}

// Review of #1001, point 1: the floor re-prices only the Saved tokens of the counted operations and leaves
// every other part of the ceiling as the library priced it. That is right for the parts that come from the
// request's own usage (a break an operation caused: a read that did not happen against a write that did;
// the tokens an operation ADDED). This pins the arithmetic with such a break and an added declaration, and
// that the floor then goes below zero when the added tokens cost more than the removed ones save at the
// read rate: the floor is a number the readers must be able to show, not clamp away.
func TestPriceMode_HeldFloorKeepsBreaksAndAddedTokensAndCanBeACost(t *testing.T) {
	p := modelinfo.Price{Input: 10e-6, CacheRead: 0.2e-6, CacheWrite: 2.5e-6, Output: 2e-6}
	e := &Event{Model: "m", FreshInput: 5, OutputTokens: 1, TokensBefore: 20000,
		Components: []CompRow{{Component: "mask", Acted: true, Mutated: true, SavedGross: 4000, SavedUnique: 4000}}}
	e.SetAccountant(accountant.Result{
		Unpriced: []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}},
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c",
			Saved:  accountant.TierTokens{CacheWrite5m: 4000},
			Caused: accountant.TierTokens{CacheWrite5m: 1000}}}, // a declaration the operation added
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "c",
			Saved:  accountant.TierTokens{CacheRead: 500},
			Caused: accountant.TierTokens{CacheWrite5m: 500}}}})
	e.PriceMode(p, true)
	if !e.EstimateHeld {
		t.Fatal("the request was not held")
	}
	brk := 500*0.2e-6 - 500*2.5e-6 // the break: a cost, whatever the warm prefix was
	high := 4000*2.5e-6 - 1000*2.5e-6 + brk
	low := 4000*0.2e-6 - 1000*2.5e-6 + brk
	if !accNear(e.EstimatedSavedUSD, high) {
		t.Errorf("ceiling %v, want %v", e.EstimatedSavedUSD, high)
	}
	if !accNear(e.EstimatedSavedLowUSD, low) {
		t.Errorf("floor %v, want %v", e.EstimatedSavedLowUSD, low)
	}
	if e.EstimatedSavedLowUSD >= 0 {
		t.Errorf("floor %v, want a cost (< 0): the added tokens cost more than the removed ones save at the read rate", e.EstimatedSavedLowUSD)
	}
}
