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
	if e.KeepAliveSavedUSD != l.KeepAliveSavedUSD || e.CachesplitSavedUSD != l.CachesplitSavedUSD ||
		e.CacheSavedUSD != l.CacheSavedUSD || e.CostUSD != l.CostUSD {
		t.Fatal("keep-alive, cachesplit, cache-saved or cost changed by the override")
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
