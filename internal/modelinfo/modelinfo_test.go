package modelinfo

import "testing"

func TestPriceCostAndZero(t *testing.T) {
	if !(Price{}).Zero() {
		t.Error("empty Price should be Zero")
	}
	p := Price{Input: 1e-06, Output: 2e-06, CacheRead: 1e-07, CacheWrite: 1.25e-06}
	if p.Zero() {
		t.Error("populated Price should not be Zero")
	}
	// 1000 fresh, 10000 read, 2000 write, 500 out.
	got := p.Cost(1000, 10000, 2000, 500)
	want := 1000*1e-06 + 10000*1e-07 + 2000*1.25e-06 + 500*2e-06
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("Cost = %v; want %v", got, want)
	}
}
