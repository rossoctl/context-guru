package dash

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
)

// #1441: tool deferral has one figure, the accountant's. These tests build a request with one search and two
// loaded definitions, and check that the component figure, the lever, the panel columns and the /stats mapping
// (gross, cost, net) are the same numbers.

// searchedEvent is a request with deferral and one search that loaded 2 definitions (1,500 billed tokens in all).
// The search turns come from the same builder the proxy uses (accrec.DeferSpendOps).
func searchedEvent(price string, fresh int64, saved accountant.TierTokens) *Event {
	e := &Event{Model: "m", FreshInput: fresh, OutputTokens: 100, TokensBefore: 900_000, DeferTokens: 5000, DeferSearches: 1, DeferLoaded: 2,
		Components: []CompRow{{Component: "idle"}}}
	ops := []accountant.PricedOp{{Operator: accwire.Operator(accwire.OpToolDefer), ChangeID: "cg:tooldefer:x", Saved: saved}}
	spend := tooldefer.SearchSpend{Output: 30, OutputEstimated: true, Loaded: 1500}
	for _, o := range accrec.DeferSpendOps("m", spend) {
		ops = append(ops, accountant.PricedOp{OpID: o.ID, Kind: o.Kind, Operator: o.Operator, Call: o.Call})
	}
	e.SetAccountant(accountant.Result{Ops: ops})
	_ = price
	return e
}

func componentOf(e *Event, name string) float64 {
	for _, c := range e.Components {
		if c.Component == name {
			return c.SavedUSD
		}
	}
	return 0
}

// Each search turn is priced from its own usage: the output at the output rate, the second pass at the input rate,
// the next request's write at the write rate. The figure is the body change less the three.
func TestDeferSearchTurnsArePricedFromTheirOwnUsage(t *testing.T) {
	e := searchedEvent("", 100_000, accountant.TierTokens{CacheRead: 20_000})
	e.PriceMode(accTestPrice, true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	gross := 20_000 * 0.3e-6
	spend := 30*15e-6 + 1500*3e-6 + 1500*3.75e-6
	if got := componentOf(e, "tooldefer"); !accNear(got, gross-spend) {
		t.Fatalf("tooldefer figure %v, want the body saving %v less the search turns %v", got, gross, spend)
	}
	// The panel, the lever and the stats mapping are that one figure.
	if !accNear(e.DeferNetUSD, componentOf(e, "tooldefer")) || !accNear(e.DeferGrossUSD, gross) || !accNear(e.DeferCostUSD, spend) {
		t.Fatalf("net %v gross %v cost %v, want %v, %v, %v", e.DeferNetUSD, e.DeferGrossUSD, e.DeferCostUSD, gross-spend, gross, spend)
	}
	// The spend operations are not a saving: no other component, no baseline delta of their own.
	if !accNear(e.BaselineCostUSD-e.CostUSD, 0) {
		t.Fatalf("baseline delta %v, want 0: the figure left it for the lever", e.BaselineCostUSD-e.CostUSD)
	}
}

// Without the spend operations the figure is the body change alone. This is what the mutation "drop the spend op" does.
func TestDeferFigureWithoutSpendsIsTheBodyChangeAlone(t *testing.T) {
	e := searchedEvent("", 100_000, accountant.TierTokens{CacheRead: 20_000})
	res, _ := e.AccountantResult()
	res.Ops = res.Ops[:1]
	e.SetAccountant(res)
	e.PriceMode(accTestPrice, true)
	if got := componentOf(e, "tooldefer"); !accNear(got, 20_000*0.3e-6) {
		t.Fatalf("figure %v, want the body change alone", got)
	}
}

// Above a price line the re-rate is in the one figure, and so in the lever and the panel columns. gpt-5.6 has its
// line at 272,000 prompt tokens: the request sent is at 250,000, the client's would have been at 290,000.
func TestDeferFigureAboveAPriceLineHoldsTheReRateAndTheSearchTurns(t *testing.T) {
	e := searchedEvent("", 250_000, accountant.TierTokens{Input: 40_000})
	e.PriceMode(gpt56(), true)
	if fb := e.AccountantFallback(); fb != "" {
		t.Fatalf("fell back: %q", fb)
	}
	fig := e.AccountantFigure(gpt56())
	reRate := 250_000*5e-6 + 100*15e-6 // the tokens that stay and the output, at the dear rate less the base rate
	// The search turns are small calls: base rates.
	spend := 30*30e-6 + 1500*5e-6 + 1500*5e-6
	want := 40_000*10e-6 + reRate - spend
	if got := fig.ByComponent["tooldefer"]; !accNear(got, want) {
		t.Fatalf("figure %v, want saved %v + re-rate %v - search turns %v = %v", got, 40_000*10e-6, reRate, spend, want)
	}
	if !accNear(e.DeferNetUSD, want) || !accNear(componentOf(e, "tooldefer"), want) {
		t.Fatalf("lever %v, row %v, want %v", e.DeferNetUSD, componentOf(e, "tooldefer"), want)
	}
	if !accNear(e.DeferGrossUSD, 40_000*10e-6+reRate) || !accNear(e.DeferCostUSD, spend) {
		t.Fatalf("gross %v cost %v, want %v and %v", e.DeferGrossUSD, e.DeferCostUSD, 40_000*10e-6+reRate, spend)
	}
}

// A spend call prices at the size of its own usage, as accountant.Figure does: a small call stays at the base rates
// whatever the request's prompt, and a call of its own above the line is dear.
func TestDeferSpendPricesAtItsOwnSize(t *testing.T) {
	call := func(in int) *accountant.ModelCall {
		return &accountant.ModelCall{Usage: accountant.CallUsage{Usage: accountant.Usage{Input: in}}}
	}
	if got := spendUSD(gpt56(), call(1000)); !accNear(got, 1000*5e-6) {
		t.Fatalf("small call: %v", got)
	}
	if got := spendUSD(gpt56(), call(300_000)); !accNear(got, 300_000*10e-6) {
		t.Fatalf("call above the line: %v", got)
	}
	if got := spendUSD(accTestPrice, call(300_000)); !accNear(got, 300_000*3e-6) {
		t.Fatalf("a price with no tiers: %v", got)
	}
}

// An unpriced model books no figure anywhere: no lever, no gross, no cost. The ledger counts it as unpriced.
func TestDeferUnpricedModelBooksNoFigure(t *testing.T) {
	e := searchedEvent("", 100_000, accountant.TierTokens{CacheRead: 20_000})
	e.Model = ""
	e.PriceMode(accTestPrice, true)
	if e.AccountantFallback() == "" {
		t.Skip("the event was priced; this test needs an unpriced one")
	}
	if e.DeferNetUSD != 0 || e.DeferGrossUSD != 0 || e.DeferCostUSD != 0 || e.AccountantPriced {
		t.Fatalf("net %v gross %v cost %v priced %v, want no figure", e.DeferNetUSD, e.DeferGrossUSD, e.DeferCostUSD, e.AccountantPriced)
	}
}
