package modelinfo

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// Price.Pricer gives the rates of RateAt: the tier of the prompt size, the model's own multiples for a
// blank rate, and the 1-hour write derived from the input rate.
func TestPricerReadsTheRateAtThePromptSize(t *testing.T) {
	p := Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6")
	pr := p.Pricer()
	at := time.Time{}
	if v, _ := pr.PriceAt("gpt-5.6", at, accountant.TierCacheRead, 272_000); v != 0.5e-6 {
		t.Errorf("at the line: %v", v)
	}
	if v, _ := pr.PriceAt("gpt-5.6", at, accountant.TierCacheRead, 272_001); v != 1e-6 {
		t.Errorf("above the line: %v", v)
	}
	if v, _ := pr.PriceAt("gpt-5.6", at, accountant.TierCacheWrite1h, 0); v <= 0 {
		t.Errorf("1h write rate: %v", v)
	}
}

func TestFlatRatesIsTheSameRateAtAnySize(t *testing.T) {
	f := FlatRates{Input: 1, Output: 2, CacheRead: 3, Write5m: 4, Write1h: 5}
	for tier, want := range map[accountant.Tier]float64{
		accountant.TierInput: 1, accountant.TierOutput: 2, accountant.TierCacheRead: 3,
		accountant.TierCacheWrite5m: 4, accountant.TierCacheWrite1h: 5,
	} {
		if v, est := f.PriceAt("any", time.Time{}, tier, 1_000_000); v != want || est {
			t.Errorf("%s: %v %v want %v", tier, v, est, want)
		}
	}
	if v, est := (FlatRates{}).PriceAt("x", time.Time{}, accountant.TierInput, 0); v != 0 || !est {
		t.Errorf("a rate of 0 is unknown: %v %v", v, est)
	}
}
