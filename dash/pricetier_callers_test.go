package dash

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// gpt56 is gpt-5.6 at plausible list rates with its built-in tier: above 272,000 prompt tokens,
// 2x input, cache read and cache write, and 1.5x output.
func gpt56() modelinfo.Price {
	return modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6")
}

// haiku55 is Claude Haiku 5.5 with its built-in tier: above 100,000 prompt tokens every rate is 5x.
func haiku55() modelinfo.Price {
	return modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}.WithTiers("claude-haiku-5-5")
}

func TestEventCostUsesThePriceOfItsPromptSize(t *testing.T) {
	cases := []struct {
		name         string
		p            modelinfo.Price
		fresh, read  int64
		out          int64
		want, wantAt float64
	}{
		{"gpt-5.6 at the line is base", gpt56(), 100_000, 172_000, 1000,
			100_000*5e-6 + 172_000*0.5e-6 + 1000*30e-6, 0},
		{"gpt-5.6 above the line is 2x in, 1.5x out", gpt56(), 100_001, 172_000, 1000,
			100_001*10e-6 + 172_000*1e-6 + 1000*45e-6, 0},
		{"haiku 5.5 at the line is base", haiku55(), 40_000, 60_000, 100,
			40_000*1e-6 + 60_000*0.1e-6 + 100*5e-6, 0},
		{"haiku 5.5 above the line is 5x", haiku55(), 40_000, 60_001, 100,
			5 * (40_000*1e-6 + 60_001*0.1e-6 + 100*5e-6), 0},
	}
	for _, c := range cases {
		e := &Event{Model: "m", FreshInput: c.fresh, CacheRead: c.read, OutputTokens: c.out}
		e.Price(c.p, true)
		if !accNear(e.CostUSD, c.want) {
			t.Errorf("%s: cost %v want %v", c.name, e.CostUSD, c.want)
		}
	}
}

func TestEventCostFlatPriceIsUnchanged(t *testing.T) {
	flat := gpt56()
	flat.Tiers = [modelinfo.MaxTiers]modelinfo.PriceTier{} // flat: true
	e := &Event{Model: "m", FreshInput: 400_000, OutputTokens: 10}
	e.Price(flat, true)
	if want := 400_000*5e-6 + 10*30e-6; !accNear(e.CostUSD, want) {
		t.Fatalf("flat cost %v want %v", e.CostUSD, want)
	}
}

// A trim takes the request from 300,000 prompt tokens (above the line, billed at 2x) to 250,000
// (below it, 1x). The saved tokens are the client's: they bill at the dear tier.
func TestFigureSavedSideIsPricedAtTheClientSize(t *testing.T) {
	e := &Event{FreshInput: 250_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 50_000}}}})
	f := e.AccountantFigure(gpt56())
	if want := 50_000 * 10e-6; !accNear(f.ByComponent["mask"], want) { // 2x input
		t.Fatalf("saving %v want %v (the without side bills at 2x)", f.ByComponent["mask"], want)
	}
	// Both sides below the line: base rate.
	e = &Event{FreshInput: 200_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 50_000}}}})
	if want := 50_000 * 5e-6; !accNear(e.AccountantFigure(gpt56()).ByComponent["mask"], want) {
		t.Fatalf("below the line: %v want %v", e.AccountantFigure(gpt56()).ByComponent["mask"], want)
	}
	// Both sides above the line: dear tier.
	e = &Event{FreshInput: 400_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 50_000}}}})
	if want := 50_000 * 10e-6; !accNear(e.AccountantFigure(gpt56()).ByComponent["mask"], want) {
		t.Fatalf("above the line: %v want %v", e.AccountantFigure(gpt56()).ByComponent["mask"], want)
	}
}

// A component adds 40,000 tokens (a cost) to a request of 300,000 tokens. The request sent is above
// the line, so its added tokens bill at 2x. The request without it (260,000) is below.
func TestFigureCausedSideIsPricedAtOurSize(t *testing.T) {
	e := &Event{FreshInput: 300_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Caused: accountant.TierTokens{Input: 40_000}}}})
	f := e.AccountantFigure(gpt56())
	if want := -40_000 * 10e-6; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("cost %v want %v", f.ByComponent["mask"], want)
	}
}

func TestFigureHaikuSidesOnOppositeSides(t *testing.T) {
	e := &Event{FreshInput: 90_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{CacheRead: 20_000}}}})
	f := e.AccountantFigure(haiku55()) // without: 110,000 > 100,000, so 5x
	if want := 20_000 * 0.5e-6; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("haiku saving %v want %v", f.ByComponent["mask"], want)
	}
}

func TestFigureWithoutUsageKeepsBaseRates(t *testing.T) {
	e := &Event{} // no billed usage: size unknown
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 500_000}}}})
	if want := 500_000 * 5e-6; !accNear(e.AccountantFigure(gpt56()).ByComponent["mask"], want) {
		t.Fatalf("unknown size must price at base, got %v", e.AccountantFigure(gpt56()).ByComponent["mask"])
	}
}

func TestNextRequestUSDUsesTheTierOfThePrefix(t *testing.T) {
	warm, cold := NextRequestUSD(gpt56(), 300_000, 300)
	if !accNear(warm, 300_000*1e-6) || !accNear(cold, 300_000*10e-6) {
		t.Fatalf("above the line: warm %v cold %v", warm, cold)
	}
	warm, cold = NextRequestUSD(gpt56(), 272_000, 300)
	if !accNear(warm, 272_000*0.5e-6) || !accNear(cold, 272_000*5e-6) {
		t.Fatalf("at the line: warm %v cold %v", warm, cold)
	}
}
