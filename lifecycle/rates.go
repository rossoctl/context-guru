package lifecycle

import (
	"context"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// SelfRates is the per-token rate card of one model, for a component that calls that model and
// prices its own spend. A nil pricer or an unnameable model gives the zero value, which a
// component must read as "unknown" and fall back on, never as free.
func SelfRates(ctx context.Context, p modelinfo.Pricer, model string) components.TokenRates {
	if p == nil || model == "" {
		return components.TokenRates{}
	}
	pr, ok := p.Price(ctx, model)
	if !ok || pr.Zero() {
		return components.TokenRates{}
	}
	return RatesOf(pr)
}

// RatesFor hands the pipeline the operator's rate card as a lookup by model name, so a component
// that compacts with a model other than the request's own prices its spend from the same card the
// bill uses. nil when there is no pricer.
func RatesFor(ctx context.Context, p modelinfo.Pricer) func(string) components.TokenRates {
	if p == nil {
		return nil
	}
	return func(model string) components.TokenRates { return SelfRates(ctx, p, model) }
}

// RatesOf is the rate card of a price: the four base rates and its tiers as absolute rates. The tiers come
// from modelinfo.Price.At, the one place that resolves a tier's rates.
func RatesOf(pr modelinfo.Price) components.TokenRates {
	out := components.TokenRates{Input: pr.Input, Output: pr.Output, CacheRead: pr.CacheRead, CacheWrite: pr.CacheWrite}
	n := 0
	for _, t := range pr.Tiers {
		if t.AbovePromptTokens <= 0 || n >= components.MaxRateTiers {
			continue
		}
		q := pr.At(t.AbovePromptTokens + 1)
		out.Tiers[n] = components.RateTier{AbovePromptTokens: t.AbovePromptTokens,
			Input: q.Input, Output: q.Output, CacheRead: q.CacheRead, CacheWrite: q.CacheWrite}
		n++
	}
	return out
}
