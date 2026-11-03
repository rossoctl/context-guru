package dash

import (
	"reflect"
	"testing"

	"github.com/rossoctl/accountant"
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

func legacyCopy(e *Event) *Event {
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
	l := legacyCopy(switchEvent())
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

// With no rescue from the library, the keep-alive column is 0 even where the legacy rule credits.
func TestPriceMode_NoRescueMeansNoKeepAliveCredit(t *testing.T) {
	e := switchEvent()
	e.KeepAlivePings, e.SinceLastMs, e.KeepAliveRefreshed = 1, 1<<40, 1000
	if l := legacyCopy(e); l.KeepAliveSavedUSD <= 0 {
		t.Fatalf("fixture: legacy credits nothing (%v)", l.KeepAliveSavedUSD)
	}
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

func TestPriceMode_FallsBackToLegacy(t *testing.T) {
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
		want := legacyCopy(e)
		e.PriceMode(accTestPrice, true)
		if got := e.AccountantFallback(); got != c.reason {
			t.Errorf("%s: fallback reason %q, want %q", name, got, c.reason)
		}
		e.accFallback = "" // the legacy copy carries no reason; every other field must match
		if !reflect.DeepEqual(e, want) {
			t.Errorf("%s: event differs from the legacy-priced copy:\n got %+v\nwant %+v", name, e, want)
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
// (accountant design §6.4). Any other unpriced reason still gates (TestPriceMode_FallsBackToLegacy).
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
