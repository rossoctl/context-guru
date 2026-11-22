package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// #1248: the cost of a tier priced from a fallback rate is kept apart, so the metrics can say how
// much of the spend is a guess. A Claude model's published multiples are not a fallback.
func TestPriceKeepsTheFallbackCost(t *testing.T) {
	near := func(a, b float64) bool { d := a - b; return d < 1e-12 && d > -1e-12 }
	feed := modelinfo.Price{Input: 10e-6, Output: 4e-5} // a feed that states no cache rate

	gem := &Event{Model: "gemini-3-flash-preview", FreshInput: 100, CacheRead: 1000, OutputTokens: 10}
	gem.Price(feed.Resolved(gem.Model), true)
	want := 1000 * 10e-6 * modelinfo.AnthropicCacheReadFrac
	if !near(gem.FallbackRateCostUSD, want) {
		t.Errorf("gemini fallback cost = %v, want %v (the cache reads only)", gem.FallbackRateCostUSD, want)
	}
	if gem.FallbackRateCostUSD >= gem.CostUSD {
		t.Errorf("fallback %v must be a part of cost %v", gem.FallbackRateCostUSD, gem.CostUSD)
	}

	cl := &Event{Model: "claude-sonnet-5-5", FreshInput: 100, CacheRead: 1000, OutputTokens: 10}
	cl.Price(feed.Resolved(cl.Model), true)
	if cl.CostUSD <= 0 || cl.FallbackRateCostUSD != 0 {
		t.Errorf("claude cost/fallback = %v/%v, want >0/0", cl.CostUSD, cl.FallbackRateCostUSD)
	}

	// A row that cannot be priced keeps no fallback cost from an earlier pricing.
	gem.Price(feed.Resolved(gem.Model), false)
	if gem.FallbackRateCostUSD != 0 {
		t.Errorf("unpriced row fallback cost = %v, want 0", gem.FallbackRateCostUSD)
	}
}
