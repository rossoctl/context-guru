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
	return components.RatesOf(pr)
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
