package dash

import (
	"path/filepath"
	"reflect"
	"testing"

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
		e.accFallback = "" // the copy carries no reason; every other field must match
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
	e.Price(accTestPrice, true)
	if e.AccountantEstimated() {
		t.Fatal("Price left the estimate flag set")
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
	}{
		"no usage": {accTestPrice, false},
		"no price": {modelinfo.Price{}, true},
	} {
		e := switchEvent()
		e.PriceMode(tc.price, tc.complete)
		if fb := e.AccountantFallback(); fb != FallbackUnpriced {
			t.Errorf("%s: fallback = %q, want %q", name, fb, FallbackUnpriced)
		}
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
	for _, e := range page.Requests {
		got[e.SessionID] = e.PriceEstimated
	}
	if len(got) != 2 || !got["est"] || got["exact"] {
		t.Fatalf("stored price_estimated = %v, want est true and exact false", got)
	}
}
