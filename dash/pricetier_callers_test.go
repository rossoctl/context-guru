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
	// Bill without: 300,000 x 10e-6 = 3.00. Bill sent: 250,000 x 5e-6 = 1.25. The 250,000 that stay
	// also drop from 2x to 1x: that is the re-rate.
	if want := 300_000*10e-6 - 250_000*5e-6; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("saving %v want %v", f.ByComponent["mask"], want)
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

// A component adds 40,000 tokens to a request of 300,000 tokens. The request sent is above the line
// (2x). The request without it (260,000) is below. The whole bill difference is a cost:
// 260,000 x 5e-6 - 300,000 x 10e-6 = -1.70. No component has a net saving, so nobody owns the 260,000
// that stay going from 1x to 2x (-1.30): it books to the ended-change figure.
func TestFigureCausedSideIsPricedAtOurSize(t *testing.T) {
	e := &Event{FreshInput: 300_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Caused: accountant.TierTokens{Input: 40_000}}}})
	f := e.AccountantFigure(gpt56())
	if want := -40_000 * 10e-6; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("cost %v want %v", f.ByComponent["mask"], want)
	}
	if want := -260_000 * 5e-6; !accNear(f.EndedBreakUSD, want) {
		t.Fatalf("unowned re-rate %v want %v", f.EndedBreakUSD, want)
	}
	if want := 260_000*5e-6 - 300_000*10e-6; !accNear(f.DeltaUSD, want) {
		t.Fatalf("delta %v want %v", f.DeltaUSD, want)
	}
}

func TestFigureHaikuSidesOnOppositeSides(t *testing.T) {
	e := &Event{CacheRead: 90_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{CacheRead: 20_000}}}})
	f := e.AccountantFigure(haiku55()) // without: 110,000 > 100,000, so 5x
	if want := 110_000*0.5e-6 - 90_000*0.1e-6; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("haiku saving %v want %v", f.ByComponent["mask"], want)
	}
}

// The example of the owner: gpt-5.6, line 272,000. A trim takes 290,000 to 250,000, with 2,000 output.
// Saving = bill without - bill sent, each at its own size, output included (1.5x above the line).
func TestFigureFullBillDifferenceAcrossTheLine(t *testing.T) {
	e := &Event{FreshInput: 250_000, OutputTokens: 2_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 40_000}}}})
	f := e.AccountantFigure(gpt56())
	want := (290_000*10e-6 + 2_000*45e-6) - (250_000*5e-6 + 2_000*30e-6) // 2.99 - 1.31
	if !accNear(f.ByComponent["mask"], want) || !accNear(f.DeltaUSD, want) {
		t.Fatalf("component %v delta %v want %v", f.ByComponent["mask"], f.DeltaUSD, want)
	}
}

// Two components split the re-rate in proportion to their net saved tokens (30,000 and 10,000), and
// the components add up to the request total.
func TestFigureReRateSplitsByNetSavedTokens(t *testing.T) {
	e := &Event{FreshInput: 250_000, OutputTokens: 2_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{
		{Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 30_000}},
		{Operator: "context-guru.prune", Saved: accountant.TierTokens{Input: 10_000}}}})
	f := e.AccountantFigure(gpt56())
	reRate := 250_000*5e-6 + 2_000*15e-6 // 1.28: the tokens that stay and the output
	if want := 30_000*10e-6 + 0.75*reRate; !accNear(f.ByComponent["mask"], want) {
		t.Fatalf("mask %v want %v", f.ByComponent["mask"], want)
	}
	if want := 10_000*10e-6 + 0.25*reRate; !accNear(f.ByComponent["prune"], want) {
		t.Fatalf("prune %v want %v", f.ByComponent["prune"], want)
	}
	if sum := f.ByComponent["mask"] + f.ByComponent["prune"]; !accNear(sum, f.DeltaUSD) {
		t.Fatalf("components %v do not add up to delta %v", sum, f.DeltaUSD)
	}
}

// A negative re-rate follows the same split: the request sent is above the line and the one without
// is below it. The owner of the net saving can go below 0.
func TestFigureNegativeReRateCanTakeAComponentBelowZero(t *testing.T) {
	e := &Event{FreshInput: 300_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{
		{Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 20_000}},
		{Operator: "context-guru.prune", Caused: accountant.TierTokens{Input: 50_000}}}})
	f := e.AccountantFigure(gpt56()) // without: 270,000 (1x); sent: 300,000 (2x)
	if want := 20_000*5e-6 - 250_000*5e-6; !accNear(f.ByComponent["mask"], want) || f.ByComponent["mask"] >= 0 {
		t.Fatalf("mask %v want %v (below 0)", f.ByComponent["mask"], want)
	}
	if want := 270_000*5e-6 - 300_000*10e-6; !accNear(f.DeltaUSD, want) {
		t.Fatalf("delta %v want %v", f.DeltaUSD, want)
	}
}

// Both sides on one side of the line: no re-rate. A price with no tiers: unchanged, whatever the sizes.
func TestFigureNoLineCrossedNoReRate(t *testing.T) {
	op := accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask", Saved: accountant.TierTokens{Input: 50_000}}}}
	e := &Event{FreshInput: 400_000, OutputTokens: 2_000}
	e.SetAccountant(op)
	if want := 50_000 * 10e-6; !accNear(e.AccountantFigure(gpt56()).DeltaUSD, want) {
		t.Fatalf("both above: %v", e.AccountantFigure(gpt56()).DeltaUSD)
	}
	flat := gpt56()
	flat.Tiers = [modelinfo.MaxTiers]modelinfo.PriceTier{}
	e = &Event{FreshInput: 250_000, OutputTokens: 2_000}
	e.SetAccountant(op)
	if want := 50_000 * 5e-6; !accNear(e.AccountantFigure(flat).DeltaUSD, want) {
		t.Fatalf("no tiers: %v", e.AccountantFigure(flat).DeltaUSD)
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
	warm, cold = NextRequestUSD(gpt56(), 271_999, 300)
	if !accNear(warm, 271_999*0.5e-6) || !accNear(cold, 271_999*5e-6) {
		t.Fatalf("below the line: warm %v cold %v", warm, cold)
	}
	// A prefix of exactly 272,000 plus the next request's input is above the line.
	warm, _ = NextRequestUSD(gpt56(), 272_000, 300)
	if !accNear(warm, 272_000*1e-6) {
		t.Fatalf("prefix 272,000: warm %v", warm)
	}
}

// The floor re-prices the saved tokens at the read rate of the size WITHOUT context-guru. The trim
// takes the request from 300,000 (2x) to 250,000: saved input bills 10e-6, as a read 1e-6.
func TestHeldFloorUsesTheSavedSidePrice(t *testing.T) {
	e := &Event{FreshInput: 250_000}
	e.SetAccountant(accountant.Result{Ops: []accountant.PricedOp{{Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 50_000}}}})
	delta := 50_000 * 10e-6
	if got, want := e.heldFloorUSD(gpt56(), delta), 50_000*1e-6; !accNear(got, want) {
		t.Fatalf("floor %v want %v", got, want)
	}
}
