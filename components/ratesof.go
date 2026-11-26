package components

import "github.com/rossoctl/context-guru/internal/modelinfo"

// RatesOf is the rate card of a price: the four base rates and its tiers as absolute rates. The tiers come
// from modelinfo.Price.At, the one place that resolves a tier's rates.
func RatesOf(pr modelinfo.Price) TokenRates {
	out := TokenRates{Input: pr.Input, Output: pr.Output, CacheRead: pr.CacheRead, CacheWrite: pr.CacheWrite}
	n := 0
	for _, t := range pr.Tiers {
		if t.AbovePromptTokens <= 0 || n >= MaxRateTiers {
			continue
		}
		q := pr.At(t.AbovePromptTokens + 1)
		out.Tiers[n] = RateTier{AbovePromptTokens: t.AbovePromptTokens,
			Input: q.Input, Output: q.Output, CacheRead: q.CacheRead, CacheWrite: q.CacheWrite}
		n++
	}
	return out
}
