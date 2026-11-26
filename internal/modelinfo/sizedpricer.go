package modelinfo

import (
	"time"

	"github.com/rossoctl/accountant"
)

// Pricer returns the price as an accountant.SizedPricer: the rate of one token of any tier, at the
// prompt size of its request. It holds ONE price, so it answers for any model name it is asked
// about, and the caller passes the model that the price belongs to. The rule is RateAt, the same
// rule that accountant.Figure uses for savings.
func (p Price) Pricer() accountant.SizedPricer { return priced{p} }

type priced struct{ p Price }

func (s priced) PriceAt(model string, _ time.Time, t accountant.Tier, promptTokens int64) (float64, bool) {
	return s.p.RateAt(model, t, promptTokens)
}

// FlatRates is a fixed rate table, as a SizedPricer: every model and every prompt size gets the
// same rate. A caller that holds rates already read at a prompt size uses it. A rate of 0 is
// unknown, and an estimate, as the accountant reads it.
type FlatRates struct {
	Input, Output, CacheRead, Write5m, Write1h float64
}

func (f FlatRates) PriceAt(_ string, _ time.Time, t accountant.Tier, _ int64) (float64, bool) {
	var r float64
	switch t {
	case accountant.TierInput:
		r = f.Input
	case accountant.TierOutput:
		r = f.Output
	case accountant.TierCacheRead:
		r = f.CacheRead
	case accountant.TierCacheWrite5m:
		r = f.Write5m
	case accountant.TierCacheWrite1h:
		r = f.Write1h
	}
	return r, r <= 0
}
