package dash

import (
	"math"
	"reflect"
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

var cfTestPrice = modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}

func cfNear(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func figOf(r cf.Result) CFFigure {
	e := &Event{}
	e.SetCounterfactual(r)
	return e.CounterfactualFigure(cfTestPrice)
}

func TestTierUSD(t *testing.T) {
	got := tierUSD(cfTestPrice, cf.TierTokens{Input: 1000, CacheRead: 1000, CacheWrite5m: 1000, CacheWrite1h: 1000})
	want := 1000*3e-6 + 1000*0.3e-6 + 1000*3.75e-6 + 1000*6e-6
	if !cfNear(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCounterfactualAccessorReturnsACopy(t *testing.T) {
	e := &Event{}
	if _, ok := e.Counterfactual(); ok {
		t.Fatal("no result attached, ok must be false")
	}
	e.SetCounterfactual(cf.Result{Version: 7})
	r, ok := e.Counterfactual()
	if !ok || r.Version != 7 {
		t.Fatalf("%+v %v", r, ok)
	}
	r.Version = 9
	if r2, _ := e.Counterfactual(); r2.Version != 7 {
		t.Fatal("accessor must return a copy")
	}
}

func TestFigure_NetOfBreakAttributedByChangeID(t *testing.T) {
	f := figOf(cf.Result{
		Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
			Saved: cf.TierTokens{CacheWrite5m: 1000}}},
		CacheEffects: []cf.CacheEffect{{Kind: cf.EffectBreak, ChangeID: "c1",
			Caused: cf.TierTokens{CacheWrite5m: 200}, Saved: cf.TierTokens{CacheRead: 200}}},
	})
	want := 1000*3.75e-6 - (200*3.75e-6 - 200*0.3e-6)
	if !cfNear(f.ByComponent["mask"], want) || !cfNear(f.DeltaUSD, want) {
		t.Fatalf("component = %v delta = %v, want %v", f.ByComponent["mask"], f.DeltaUSD, want)
	}
}

func TestPriceCF_DryRunNeverCounted(t *testing.T) {
	f := figOf(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1",
		DryRun: true, Saved: cf.TierTokens{Input: 5000}}}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("a dry run must not reach a total: %+v", f)
	}
}

func TestPriceCF_DryRunEstimatedDoesNotMarkFigure(t *testing.T) {
	f := figOf(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", DryRun: true, Estimated: true}}})
	if f.Estimated {
		t.Fatal("a skipped dry run must not mark the figure estimated")
	}
}

func TestFigure_SpendOpIsNotASaving(t *testing.T) {
	f := figOf(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.summarize", ChangeID: "s1",
		Call: &cf.ModelCall{}, Saved: cf.TierTokens{Input: 9000}}}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("a spend op must be skipped: %+v", f)
	}
}

func TestFigure_UnattributedBreakStaysOutOfComponents(t *testing.T) {
	f := figOf(cf.Result{
		Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "c1", DryRun: true}},
		CacheEffects: []cf.CacheEffect{{Kind: cf.EffectBreak, ChangeID: "c1",
			Caused: cf.TierTokens{CacheWrite5m: 100}}}})
	if f.UnattributedBreakUSD <= 0 || len(f.ByComponent) != 0 || f.DeltaUSD != 0 {
		t.Fatalf("%+v", f)
	}
	if !cfNear(f.UnattributedBreakUSD, 100*3.75e-6) {
		t.Fatalf("got %v", f.UnattributedBreakUSD)
	}
}

func TestFigure_UnpricedOperationIsListedNotZeroed(t *testing.T) {
	f := figOf(cf.Result{Unpriced: []cf.Unpriced{{OpID: "x", Reason: "no_usage"}}})
	if !reflect.DeepEqual(f.Unpriced, []string{"x:no_usage"}) {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_RescueIsKeepAliveSaved(t *testing.T) {
	f := figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectRescue, ChangeID: "w1",
		Saved: cf.TierTokens{CacheWrite5m: 1000}, Caused: cf.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !cfNear(f.KeepAliveSavedUSD, want) || f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_SplitIsReportedApart(t *testing.T) {
	f := figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectSplit, ChangeID: "cg:cachesplit:1",
		Saved: cf.TierTokens{CacheWrite5m: 1000}, Caused: cf.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !cfNear(f.SplitUSD, want) || f.DeltaUSD != 0 || f.KeepAliveSavedUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_TTLPremiumIsANegativeSaving(t *testing.T) {
	f := figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectTTLPremium, ChangeID: "headttl:1",
		Saved: cf.TierTokens{CacheWrite5m: 1000}, Caused: cf.TierTokens{CacheWrite1h: 1000}}}})
	want := 1000*3.75e-6 - 1000*6e-6
	if !cfNear(f.TTLUSD, want) || f.TTLUSD >= 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_TTLRescueIsAPositiveSaving(t *testing.T) {
	f := figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectTTLRescue, ChangeID: "headttl:1",
		Saved: cf.TierTokens{CacheWrite5m: 1000}, Caused: cf.TierTokens{CacheRead: 1000}}}})
	want := 1000*3.75e-6 - 1000*0.3e-6
	if !cfNear(f.TTLUSD, want) || f.TTLUSD <= 0 {
		t.Fatalf("%+v want %v", f, want)
	}
}

func TestFigure_UnknownEffectKindIsUnpriced(t *testing.T) {
	f := figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: "future", Saved: cf.TierTokens{Input: 1}}}})
	if !reflect.DeepEqual(f.Unpriced, []string{"effect:future:unknown_kind"}) || f.DeltaUSD != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_EstimatedFlag(t *testing.T) {
	if figOf(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "a"}}}).Estimated {
		t.Fatal("exact figure marked estimated")
	}
	if !figOf(cf.Result{Ops: []cf.PricedOp{{Operator: "context-guru.mask", ChangeID: "a", Estimated: true}}}).Estimated {
		t.Fatal("estimated op not flagged")
	}
	if !figOf(cf.Result{CacheEffects: []cf.CacheEffect{{Kind: cf.EffectRescue, Estimated: true}}}).Estimated {
		t.Fatal("estimated effect not flagged")
	}
}

func TestFigure_NoResultIsZeroAndNilSafe(t *testing.T) {
	f := (&Event{}).CounterfactualFigure(cfTestPrice)
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 || f.Estimated || len(f.Unpriced) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestFigure_ComponentNameTrimsOnlyTheOperatorPrefix(t *testing.T) {
	f := figOf(cf.Result{Ops: []cf.PricedOp{
		{Operator: "context-guru.mask", ChangeID: "a", Saved: cf.TierTokens{Input: 10}},
		{Operator: "headttl", ChangeID: "b", Saved: cf.TierTokens{Input: 10}},
	}})
	if _, ok := f.ByComponent["mask"]; !ok {
		t.Fatalf("%+v", f.ByComponent)
	}
	if _, ok := f.ByComponent["headttl"]; !ok {
		t.Fatalf("%+v", f.ByComponent)
	}
}

func TestFigure_InteractionIsAttributedToNobody(t *testing.T) {
	f := figOf(cf.Result{Interaction: cf.TierTokens{Input: 5000}})
	if f.DeltaUSD != 0 || len(f.ByComponent) != 0 {
		t.Fatalf("%+v", f)
	}
}
