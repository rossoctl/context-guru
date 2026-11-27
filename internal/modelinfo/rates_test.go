package modelinfo

import (
	"context"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

type onePricer struct{ model string }

func (o onePricer) Price(_ context.Context, model string) (Price, bool) {
	if model != o.model {
		return Price{}, false
	}
	p := Price{Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6, Output: 4e-6}
	p.Tiers[0] = PriceTier{AbovePromptTokens: 200_000, InputMult: 2, ReadMult: 2, WriteMult: 2, OutputMult: 2}
	return p, true
}

// The other-model rule: Rates prices a model its lookup knows, and never prices another model with
// the rates of this one. An unknown model is 0 and an estimate.
func TestRatesOtherModelRule(t *testing.T) {
	p := Price{Input: 1e-6}
	one := OneModel("a", p)
	if v, est := one.PriceAt("a", time.Time{}, accountant.TierInput, 10); v != 1e-6 || est {
		t.Errorf("own model = %v %v, want 1e-6 false", v, est)
	}
	if v, est := one.PriceAt("b", time.Time{}, accountant.TierInput, 10); v != 0 || !est {
		t.Errorf("another model = %v %v, want 0 true", v, est)
	}
	chain := ChainRates(onePricer{model: "x"})
	if v, est := chain.PriceAt("x", time.Time{}, accountant.TierInput, 10); v != 1e-6 || est {
		t.Errorf("chain, known = %v %v", v, est)
	}
	if v, est := chain.PriceAt("y", time.Time{}, accountant.TierInput, 10); v != 0 || !est {
		t.Errorf("chain, unknown = %v %v, want 0 true", v, est)
	}
	if v, est := ChainRates(nil).PriceAt("x", time.Time{}, accountant.TierInput, 10); v != 0 || !est {
		t.Errorf("nil chain = %v %v, want 0 true", v, est)
	}
	if v, est := (Rates{}).PriceAt("x", time.Time{}, accountant.TierInput, 10); v != 0 || !est {
		t.Errorf("empty Rates = %v %v, want 0 true", v, est)
	}
}

// Price is PriceAt at size 0, and a size above a line gives the tier rate.
func TestRatesSizeAndBase(t *testing.T) {
	r := ChainRates(onePricer{model: "x"})
	if v, _ := r.Price("x", time.Time{}, accountant.TierCacheRead); v != 1e-7 {
		t.Errorf("base read = %v", v)
	}
	if v, _ := r.PriceAt("x", time.Time{}, accountant.TierCacheRead, 200_001); v != 2e-7 {
		t.Errorf("read above the line = %v, want 2e-7", v)
	}
	if v, _ := r.PriceAt("x", time.Time{}, accountant.TierCacheRead, 200_000); v != 1e-7 {
		t.Errorf("read at the line = %v, want base", v)
	}
}

func TestFlatRatesGiveEachTierItsRate(t *testing.T) {
	f := FlatRates{Input: 1, Output: 2, CacheRead: 3, Write5m: 4, Write1h: 5}
	for tier, want := range map[accountant.Tier]float64{accountant.TierInput: 1, accountant.TierOutput: 2,
		accountant.TierCacheRead: 3, accountant.TierCacheWrite5m: 4, accountant.TierCacheWrite1h: 5} {
		if v, est := f.PriceAt("any", time.Time{}, tier, 999_999); v != want || est {
			t.Errorf("%s = %v %v, want %v false", tier, v, est, want)
		}
	}
	if v, est := (FlatRates{}).PriceAt("m", time.Time{}, accountant.TierInput, 0); v != 0 || !est {
		t.Errorf("rate 0 = %v %v, want 0 true", v, est)
	}
	if _, est := f.PriceAt("m", time.Time{}, accountant.Tier("nope"), 0); !est {
		t.Error("unknown tier must be an estimate")
	}
}

func TestOnePriceMatchesOneModel(t *testing.T) {
	p := Price{Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}
	o := OnePrice{Model: "a", Price: p}
	want, westd := p.RateAt("a", accountant.TierCacheRead, 10)
	if v, est := o.PriceAt("a", time.Time{}, accountant.TierCacheRead, 10); v != want || est != westd {
		t.Errorf("own model = %v %v, want %v %v", v, est, want, westd)
	}
	if v, est := o.PriceAt("b", time.Time{}, accountant.TierCacheRead, 10); v != 0 || !est {
		t.Errorf("another model = %v %v, want 0 true", v, est)
	}
}
