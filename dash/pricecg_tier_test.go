package dash

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	lc "github.com/rossoctl/context-guru/lifecycle"
)

// A compaction call on a prompt above a price-tier line (gpt-5.6, 300K) is priced at the tier on the dash row
// and in the component's booked CostUSD: the same dollars (#1329).
func TestPriceCGPricesAtTheTierOfTheCallsPrompt(t *testing.T) {
	p := modelinfo.Price{Input: 5e-6, Output: 30e-6, CacheRead: 0.5e-6, CacheWrite: 6.25e-6}.WithTiers("gpt-5.6")
	e := &Event{Model: "gpt-5.6", CGFresh: 1000, CGCacheRead: 299_000, CGCacheWrite: 0, CGOutput: 500}
	e.PriceCG(nil, p, true)
	card := lc.RatesOf(p)
	want := card.Cost(1000, 500, 0, 299_000) // the component's CostUSD
	if math.Abs(e.CGLLMCostUSD-want) > 1e-9 {
		t.Fatalf("dash row %v, component %v", e.CGLLMCostUSD, want)
	}
	flat := 1000*5e-6 + 299_000*0.5e-6 + 500*30e-6
	if e.CGLLMCostUSD < 1.9*flat {
		t.Fatalf("dash row %v is not at the tier (flat %v)", e.CGLLMCostUSD, flat)
	}
}
