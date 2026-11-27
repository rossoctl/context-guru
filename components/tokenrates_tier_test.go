package components_test

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/lifecycle"
)

// TokenRates.At and LineBelow must pick the same tier as modelinfo.Price.At at every prompt size.
func TestTokenRatesTierMatchesPriceAt(t *testing.T) {
	p := modelinfo.Price{Input: 4e-6, Output: 20e-6, CacheRead: 0.4e-6, CacheWrite: 5e-6}.WithTiers("gpt-5.6")
	if !p.HasTiers() {
		t.Fatal("no built-in tier")
	}
	r := lifecycle.RatesOf(p)
	for _, n := range []int64{0, 1, 271_999, 272_000, 272_001, 1_000_000} {
		q := p.At(n)
		got := r.At(n)
		want := components.TokenRates{Input: q.Input, Output: q.Output, CacheRead: q.CacheRead, CacheWrite: q.CacheWrite}
		if got != want {
			t.Errorf("At(%d) = %+v, want %+v", n, got, want)
		}
		line, ok := r.LineBelow(n)
		if ok != (n > 272_000) || (ok && line != 272_000) {
			t.Errorf("LineBelow(%d) = %d, %v", n, line, ok)
		}
	}
}
