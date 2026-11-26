package modelinfo

import "github.com/rossoctl/accountant"

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
