package dash

import (
	"math"
	"reflect"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

var accTestPrice = modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}

func accNear(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func figOf(r accountant.Result) AccFigure {
	e := &Event{}
	e.SetAccountant(r)
	return e.AccountantFigure(accTestPrice)
}

func TestTierUSD(t *testing.T) {
	got := tierUSD(accTestPrice, accountant.TierTokens{Input: 1000, CacheRead: 1000, CacheWrite5m: 1000, CacheWrite1h: 1000})
	want := 1000*3e-6 + 1000*0.3e-6 + 1000*3.75e-6 + 1000*6e-6
	if !accNear(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestAccountantResultReturnsACopy(t *testing.T) {
	e := &Event{}
	if _, ok := e.AccountantResult(); ok {
		t.Fatal("no result attached, ok must be false")
	}
	e.SetAccountant(accountant.Result{Version: 7})
	r, ok := e.AccountantResult()
	if !ok || r.Version != 7 {
		t.Fatalf("%+v %v", r, ok)
	}
	r.Version = 9
	if r2, _ := e.AccountantResult(); r2.Version != 7 {
		t.Fatal("accessor must return a copy")
	}
}

func TestFigure_NetOfBreakAttributedByChangeID(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
			Saved: accountant.TierTokens{CacheWrite5m: 1000}}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "c1",
			Caused: accountant.TierTokens{CacheWrite5m: 200}, Saved: accountant.TierTokens{CacheRead: 200}}},
	})
	want := 1000*3.75e-6 - (200*3.75e-6 - 200*0.3e-6)
	if !accNear(f.ByComponent["mask"], want) || !accNear(f.DeltaUSD, want) {
		t.Fatalf("component = %v delta = %v, want %v", f.ByComponent["mask"], f.DeltaUSD, want)
	}
}

func TestPriceAcc_DryRunNeverCounted(t *testing.T) {
	f := figOf(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
		DryRun: true, Saved: accountant.TierTokens{Input: 5000}}}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("a dry run must not reach a total: %+v", f)
	}
}

func TestPriceAcc_DryRunEstimatedDoesNotMarkFigure(t *testing.T) {
	f := figOf(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", DryRun: true, Estimated: true}}})
	if f.Estimated {
		t.Fatal("a skipped dry run must not mark the figure estimated")
	}
}

func TestFigure_SpendOpIsNotASaving(t *testing.T) {
	f := figOf(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.summarize", ChangeID: "s1",
		Call: &accountant.ModelCall{}, Saved: accountant.TierTokens{Input: 9000}}}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("a spend op must be skipped: %+v", f)
	}
}

func TestFigure_UnattributedBreakStaysOutOfComponents(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1", DryRun: true}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "c1",
			Caused: accountant.TierTokens{CacheWrite5m: 100}}}})
	if f.UnattributedBreakUSD <= 0 || len(f.ByComponent) != 0 || f.DeltaUSD != 0 {
		t.Fatalf("%+v", f)
	}
	if !accNear(f.UnattributedBreakUSD, 100*3.75e-6) {
		t.Fatalf("got %v", f.UnattributedBreakUSD)
	}
}

func TestFigure_UnpricedOperationIsListedNotZeroed(t *testing.T) {
	f := figOf(accountant.Result{Unpriced: []accountant.Unpriced{{OpID: "x", Reason: "no_usage"}}})
	if !reflect.DeepEqual(f.Unpriced, []string{"x:no_usage"}) {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_RescueIsKeepAliveSaved(t *testing.T) {
	f := figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectRescue, ChangeID: "w1",
		Saved: accountant.TierTokens{CacheWrite5m: 1000}, Caused: accountant.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !accNear(f.KeepAliveSavedUSD, want) || f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_SplitIsReportedApart(t *testing.T) {
	f := figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectSplit, ChangeID: "cg:cachesplit:1",
		Saved: accountant.TierTokens{CacheWrite5m: 1000}, Caused: accountant.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !accNear(f.SplitUSD, want) || f.DeltaUSD != 0 || f.KeepAliveSavedUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_TTLPremiumIsANegativeSaving(t *testing.T) {
	f := figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectTTLPremium, ChangeID: "headttl:1",
		Saved: accountant.TierTokens{CacheWrite5m: 1000}, Caused: accountant.TierTokens{CacheWrite1h: 1000}}}})
	want := 1000*3.75e-6 - 1000*6e-6
	if !accNear(f.TTLUSD, want) || f.TTLUSD >= 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_TTLRescueIsAPositiveSaving(t *testing.T) {
	f := figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectTTLRescue, ChangeID: "headttl:1",
		Saved: accountant.TierTokens{CacheWrite5m: 1000}, Caused: accountant.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !accNear(f.TTLUSD, want) || f.TTLUSD <= 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_UnknownEffectKindIsUnpriced(t *testing.T) {
	f := figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: "future", Saved: accountant.TierTokens{Input: 1}}}})
	if !reflect.DeepEqual(f.Unpriced, []string{"effect:future:unknown_kind"}) || f.DeltaUSD != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_EstimatedFlag(t *testing.T) {
	if figOf(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "a"}}}).Estimated {
		t.Fatal("exact figure marked estimated")
	}
	if !figOf(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "a", Estimated: true}}}).Estimated {
		t.Fatal("estimated op not flagged")
	}
	if !figOf(accountant.Result{CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectRescue, Estimated: true}}}).Estimated {
		t.Fatal("estimated effect not flagged")
	}
}

func TestFigure_NoResultIsZeroAndNilSafe(t *testing.T) {
	f := (&Event{}).AccountantFigure(accTestPrice)
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 || f.Estimated || len(f.Unpriced) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_ComponentNameTrimsOnlyTheOperatorPrefix(t *testing.T) {
	f := figOf(accountant.Result{Ops: []accountant.PricedOp{
		{Operator: "context-guru.mask", ChangeID: "a", Saved: accountant.TierTokens{Input: 10}},
		{Operator: "headttl", ChangeID: "b", Saved: accountant.TierTokens{Input: 10}},
	}})
	if _, ok := f.ByComponent["mask"]; !ok {
		t.Fatalf("%+v", f.ByComponent)
	}
	if _, ok := f.ByComponent["headttl"]; !ok {
		t.Fatalf("%+v", f.ByComponent)
	}
}

func TestFigure_InteractionIsAttributedToNobody(t *testing.T) {
	f := figOf(accountant.Result{Interaction: accountant.TierTokens{Input: 5000}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_UnpricedOpInOpsIsNotAZeroSaving(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{
			{OpID: "bad", Operator: "context-guru.mask", ChangeID: "c1"},
			{OpID: "ok", Operator: "context-guru.dedupe", ChangeID: "c2", Saved: accountant.TierTokens{Input: 100}},
		},
		Unpriced: []accountant.Unpriced{{OpID: "bad", Reason: "no_usage"}},
	})
	if _, in := f.ByComponent["mask"]; in {
		t.Fatalf("unpriced op must be absent: %+v", f.ByComponent)
	}
	if !accNear(f.ByComponent["dedupe"], 100*3e-6) || !accNear(f.DeltaUSD, 100*3e-6) {
		t.Fatalf("%+v", f)
	}
	if !reflect.DeepEqual(f.Unpriced, []string{"bad:no_usage"}) {
		t.Fatalf("%v", f.Unpriced)
	}
}

func TestFigure_EmptyOpIDUnpricedEntryNeverDropsRealTokens(t *testing.T) {
	f := figOf(accountant.Result{
		Ops:      []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1", Saved: accountant.TierTokens{Input: 100}}},
		Unpriced: []accountant.Unpriced{{Reason: "end_request_missing"}},
	})
	if !accNear(f.ByComponent["mask"], 100*3e-6) {
		t.Fatalf("%+v", f)
	}
}

// A break with no ChangeID that the client body does not have is one an ended change left behind
// (accountant design §5.2 row 1, Q12): it counts in DeltaUSD, and no component owns it.
func TestFigure_EndedChangeBreakCountsAndIsUnowned(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 100}}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak,
			Caused: accountant.TierTokens{CacheWrite5m: 100}, Saved: accountant.TierTokens{CacheRead: 100}}},
	})
	if !accNear(f.ByComponent["mask"], 100*3e-6) {
		t.Fatalf("component touched: %+v", f)
	}
	brk := 100*0.3e-6 - 100*3.75e-6
	if !accNear(f.EndedBreakUSD, brk) || !accNear(f.DeltaUSD, 100*3e-6+brk) || f.UnattributedBreakUSD != 0 {
		t.Fatalf("%+v", f)
	}
}

// A break the client body has too (InBaseline: a client edit, an expiry) is shown and never counted.
func TestFigure_InBaselineBreakIsShownNotCounted(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 100}}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, InBaseline: true,
			Caused: accountant.TierTokens{CacheWrite5m: 100}, Saved: accountant.TierTokens{CacheRead: 100}}},
	})
	if !accNear(f.DeltaUSD, 100*3e-6) || f.EndedBreakUSD != 0 {
		t.Fatalf("an in-baseline break was counted: %+v", f)
	}
	if !accNear(f.UnattributedBreakUSD, 100*3.75e-6-100*0.3e-6) {
		t.Fatalf("%+v", f)
	}
}

// An unpriced setting (no position: penalty_unknown) still owns the break the library priced for it.
func TestFigure_UnpricedSettingOwnsItsBreak(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{OpID: "op1", Operator: "inference-router", ChangeID: "rt:1"}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "rt:1",
			Caused: accountant.TierTokens{CacheWrite5m: 100}, Saved: accountant.TierTokens{CacheRead: 100}}},
		Unpriced: []accountant.Unpriced{{OpID: "op1", Reason: "penalty_unknown"}},
	})
	if brk := 100*0.3e-6 - 100*3.75e-6; !accNear(f.ByComponent["inference-router"], brk) || f.UnattributedBreakUSD != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_DuplicateChangeIDKeepsFirstOwner(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{
			{Operator: "context-guru.a", ChangeID: "c1", Saved: accountant.TierTokens{Input: 100}},
			{Operator: "context-guru.b", ChangeID: "c1", Saved: accountant.TierTokens{Input: 100}},
		},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "c1", Caused: accountant.TierTokens{Input: 10}}},
	})
	if !accNear(f.ByComponent["a"], 100*3e-6-10*3e-6) || !accNear(f.ByComponent["b"], 100*3e-6) {
		t.Fatalf("%+v", f.ByComponent)
	}
}

func TestFigure_BreakCausedAsOneHourWrite(t *testing.T) {
	f := figOf(accountant.Result{
		Ops: []accountant.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1", Saved: accountant.TierTokens{Input: 1000}}},
		CacheEffects: []accountant.CacheEffect{{Kind: accountant.EffectBreak, ChangeID: "c1",
			Caused: accountant.TierTokens{CacheWrite1h: 200}, Saved: accountant.TierTokens{CacheRead: 200}}},
	})
	want := 1000*3e-6 - (200*6e-6 - 200*0.3e-6)
	if !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("got %v want %v", f.ByComponent["mask"], want)
	}
}

// Issue #1076: Event.CostUSD (modelinfo) and the accountant figure (tierUSD) must price the same
// 1h-written tokens alike, for every price shape: derived, a flat 1h rate (cache_write_1h_frac:
// 1.0, as prices.example.yaml ships) and a stated 5m rate above the derived 1h one.
func TestTierUSDAgreesWithCostWithCacheWrite1h(t *testing.T) {
	for name, p := range map[string]modelinfo.Price{
		"derived":     accTestPrice,
		"flat 1h":     {Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Write1hFrac: 1.0},
		"5m above 1h": {Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 9e-6},
	} {
		const n = 1000
		got := tierUSD(p, accountant.TierTokens{CacheWrite1h: n})
		want := p.CostWithCacheWrite1h(0, 0, n, 0, n)
		if !accNear(got, want) {
			t.Errorf("%s: tierUSD = %v, CostWithCacheWrite1h = %v", name, got, want)
		}
	}
}

// NextRequestUSD names the 1h tier by the modelinfo TTL, not by a literal.
func TestNextRequestUSDColdAtTheOneHourRate(t *testing.T) {
	_, cold := NextRequestUSD(accTestPrice, 1000, 3600)
	if want := 1000 * accTestPrice.CacheWrite1h(); !accNear(cold, want) {
		t.Errorf("1h cold = %v, want %v", cold, want)
	}
	_, cold = NextRequestUSD(accTestPrice, 1000, 300)
	if want := 1000 * accTestPrice.CacheWrite; !accNear(cold, want) {
		t.Errorf("5m cold = %v, want %v", cold, want)
	}
}

// Issue #1077: dash.TierTokens carries the stored JSON names, and the library owns the arithmetic. A
// tier the library adds must reach dash.TierTokens, or add and less would drop it without an error:
// set every library tier to 1 and require the round trip to keep them all.
func TestTierTokensCarryEveryLibraryTier(t *testing.T) {
	var lib accountant.TierTokens
	v := reflect.ValueOf(&lib).Elem()
	for i := range v.NumField() {
		v.Field(i).SetInt(1)
	}
	if got := tiersOf(lib).lib(); got != lib {
		t.Fatalf("a library tier is lost between accountant.TierTokens and dash.TierTokens: %+v -> %+v", lib, got)
	}
	var sum TierTokens
	sum.add(tiersOf(lib))
	sum.add(tiersOf(lib))
	if got := sum.less(tiersOf(lib)); got != tiersOf(lib) {
		t.Fatalf("add then less = %+v, want %+v", got, tiersOf(lib))
	}
}

// A fatal library check this host has no name for still gates the figure (issue #1077). The library
// decides which checks are fatal; a non-fatal one does not gate.
func TestAnUnnamedFatalCheckStillGates(t *testing.T) {
	if got := fatalCheckFallback("some_future_check"); got != FallbackCheckOther {
		t.Errorf("fatalCheckFallback = %q, want %q", got, FallbackCheckOther)
	}
	e := switchEvent()
	e.accRes.Checks = []accountant.CheckFailure{{Check: accountant.CheckConservation}}
	e.PriceMode(accTestPrice, true)
	if got := e.AccountantFallback(); got != "" {
		t.Errorf("an informational check gated the figure: fallback %q", got)
	}
}
