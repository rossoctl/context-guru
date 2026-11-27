package proxy

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

var benchUsage = Usage{CacheRead: 300_000, FreshInput: 100, Output: 500}

// The pricing part of one live decision, as it was: a memo and a closure for each call, and a rate table over
// the price chain.
func BenchmarkLiveDecisionPricingMemoPerCall(b *testing.B) {
	h := &Handler{opts: Options{Prices: tieredPricer{gpt56Price()}}}
	u := benchUsage
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		price := h.autoPrice()
		dp, _ := campaign.PriceDecision(price, "gpt-5.6", u.CacheRead+u.CacheWrite, modelinfo.PromptTokens(u.FreshInput, u.CacheRead, u.CacheWrite), false)
		_, _, _, _, _ = price("gpt-5.6", 0)
		usd, _ := accountant.RequestCost(accountant.RequestCostInput{Model: "gpt-5.6",
			Billed: accountant.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead)}, Output: int(u.Output),
			PromptTokens: modelinfo.PromptTokens(u.FreshInput, u.CacheRead, u.CacheWrite)}, modelinfo.ChainRates(h.opts.Prices))
		_, _ = dp, usd
	}
}

// The same work now: one lookup, one value-type rate table.
func BenchmarkLiveDecisionPricing(b *testing.B) {
	h := &Handler{opts: Options{Prices: tieredPricer{gpt56Price()}}}
	u := benchUsage
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mp, _ := h.livePrice("gpt-5.6")
		at := func(_ string, n int64) (float64, float64, float64, float64, bool) {
			q := mp.At(n)
			return q.Input, q.Output, q.CacheRead, q.CacheWrite, true
		}
		dp, _ := campaign.PriceDecision(at, "gpt-5.6", u.CacheRead+u.CacheWrite, modelinfo.PromptTokens(u.FreshInput, u.CacheRead, u.CacheWrite), false)
		_, _, _, _, _ = at("gpt-5.6", 0)
		usd := billedCostOf(mp, "gpt-5.6", time.Time{}, u)
		_, _ = dp, usd
	}
}
