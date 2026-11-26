package modelinfo

import (
	"context"
	"time"

	"github.com/rossoctl/accountant"
)

// RateAt is the rate per token of one billing tier, for a request of promptTokens prompt tokens, and
// whether the rate is an estimate. It is the host pricer of accountant.Figure (SizedPricer.PriceAt):
// the price is resolved for the model first, then the tier rule At picks the rates of the size. A
// size of 0 or less gives the base rate. A cache-read rate that Resolved filled from a fallback is an
// estimate (#1248). An unknown tier is 0 and an estimate.
func (p Price) RateAt(model string, t accountant.Tier, promptTokens int64) (float64, bool) {
	pr := p.Resolved(model).At(promptTokens)
	switch t {
	case accountant.TierInput:
		return pr.Input, false
	case accountant.TierCacheRead:
		return pr.CacheRead, pr.CacheReadFallback
	case accountant.TierCacheWrite5m:
		return pr.CacheWrite, false // a rule per family, not a guess (see CacheReadFallback)
	case accountant.TierCacheWrite1h:
		return pr.CacheWrite1h(), false
	case accountant.TierOutput:
		return pr.Output, false
	}
	return 0, true
}

// Rates is the one host pricer of the accountant: accountant.SizedPricer for accountant.Figure and
// accountant.RemovalSaving, and the pricer of the metrics sink. It reads the price of a model from
// Lookup and takes the rate of one tier at one prompt size from Price.RateAt.
//
// The other-model rule: Rates prices any model its Lookup knows. For a model Lookup does not know, the
// rate is 0 and an estimate. Rates never prices a call with the rates of another model. A host that has
// one price (the dashboard, for one request) gives a Lookup that knows one model (OneModel). A host that
// has the whole price chain (the proxy) gives the chain (ChainRates).
type Rates struct {
	Lookup func(model string) (Price, bool)
}

// PriceAt is accountant.SizedPricer. A promptTokens of 0 or less gives the base rate.
func (r Rates) PriceAt(model string, _ time.Time, t accountant.Tier, promptTokens int64) (float64, bool) {
	if r.Lookup == nil {
		return 0, true
	}
	p, ok := r.Lookup(model)
	if !ok {
		return 0, true // unknown model: no money, marked estimated
	}
	return p.RateAt(model, t, promptTokens)
}

// Price is the base rate (the metrics sink's Pricer): the rate of a request of unknown size.
func (r Rates) Price(model string, at time.Time, t accountant.Tier) (float64, bool) {
	return r.PriceAt(model, at, t, 0)
}

// OneModel is Rates for a host that knows the price of one model.
func OneModel(model string, p Price) Rates {
	return Rates{Lookup: func(m string) (Price, bool) { return p, m == model }}
}

// ChainRates is Rates over a Pricer chain. A nil chain knows no model. It never blocks: the chain
// answers from memory.
func ChainRates(p Pricer) Rates {
	if p == nil {
		return Rates{}
	}
	return Rates{Lookup: func(m string) (Price, bool) { return p.Price(context.Background(), m) }}
}
