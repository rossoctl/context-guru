package dash

import (
	"reflect"
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

// switchEvent is a priced-looking event with a mask component that acted and a library Result
// that saved 1000 cache-read tokens for it.
func switchEvent() *Event {
	e := &Event{Model: "m", FreshInput: 100, CacheRead: 1000, OutputTokens: 10, TokensBefore: 5000,
		Components: []CompRow{
			{Component: "mask", Acted: true, Mutated: true, SavedGross: 9999, SavedUnique: 9999},
			{Component: "idle"},
		}}
	e.SetCounterfactual(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c",
		Saved: cf.TierTokens{CacheRead: 1000}}}})
	return e
}

func legacyCopy(e *Event) *Event {
	c := *e
	c.Components = append([]CompRow(nil), e.Components...)
	c.Price(cfTestPrice, true)
	return &c
}

func TestPriceMode_CounterfactualOverridesComponentsAndBaseline(t *testing.T) {
	e := switchEvent()
	e.PriceMode(cfTestPrice, true, SavingsCounterfactual)
	want := 1000 * 0.3e-6
	if !cfNear(e.Components[0].SavedUSD, want) || e.Components[1].SavedUSD != 0 {
		t.Fatalf("components = %+v, want mask %v", e.Components, want)
	}
	if !cfNear(e.BaselineCostUSD-e.CostUSD, want) {
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
	cases := map[string]func(*Event){
		"no result":   func(e *Event) { e.cfRes = nil },
		"ops dropped": func(e *Event) { e.SetCounterfactualContext(1, false) },
		"observe":     func(e *Event) { e.SetCounterfactualContext(0, true) },
		"unpriced": func(e *Event) {
			e.cfRes.Unpriced = []cf.Unpriced{{OpID: "x", Reason: "r"}}
		},
		"acted without figure": func(e *Event) { e.Components[1].Acted = true },
		"figure without row":   func(e *Event) { e.Components = e.Components[1:] },
	}
	for name, mut := range cases {
		e := switchEvent()
		mut(e)
		want := legacyCopy(e)
		e.PriceMode(cfTestPrice, true, SavingsCounterfactual)
		if !reflect.DeepEqual(e, want) {
			t.Errorf("%s: event differs from the legacy-priced copy:\n got %+v\nwant %+v", name, e, want)
		}
	}
}

func TestPriceMode_LegacyShadowAndPriceAgree(t *testing.T) {
	want := legacyCopy(switchEvent())
	for name, m := range map[string]SavingsMode{"legacy": SavingsLegacy, "shadow": SavingsShadow} {
		e := switchEvent()
		e.PriceMode(cfTestPrice, true, m)
		if !reflect.DeepEqual(e, want) {
			t.Errorf("%s differs from Price", name)
		}
	}
}

func TestPriceMode_UnaccountedEventIsNotOverridden(t *testing.T) {
	e := switchEvent()
	e.PriceMode(cfTestPrice, false, SavingsCounterfactual)
	if e.BaselineCostUSD != 0 || e.Components[0].SavedUSD != 0 {
		t.Fatalf("overrode an unpriced event: %+v", e)
	}
}
