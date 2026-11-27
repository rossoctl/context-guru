package kvcache

import (
	"math"
	"testing"
)

// same allows one unit in the last place: the compiler may fuse a multiply and an add on some CPUs, and the
// two expressions add the same terms in the same order.
func same(a, b float64) bool { return math.Abs(a-b) <= 4e-16*math.Max(math.Abs(a), math.Abs(b)) }

// RequestCost and RecreateCost come from the accountant now. They must give the same figures as the
// formulas that the simulator used before (to the last bit, give or take a fused multiply-add).
func TestRequestAndRecreateCostKeepTheirFigures(t *testing.T) {
	p := Pricing{Model: "m", Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, Write5m: 3.75e-6, Write1h: 6e-6,
		PingInputTokens: 7, PingOutputTokens: 1}
	sem := Semantics{}
	for _, cached := range []int64{0, 1, 999, 48_123, 271_993, 1_000_000} {
		for _, tier := range []TTL{TTLNone, TTL5m, TTL1h} {
			oldRe := float64(cached)*p.writeRate(tier) + float64(p.PingInputTokens)*p.Input + float64(sem.pingOutput(p))*p.Output
			if got := p.RecreateCost(cached, tier, sem); !same(got, oldRe) {
				t.Errorf("RecreateCost(%d, %v) = %v, want %v", cached, tier, got, oldRe)
			}
			for _, io := range [][2]int64{{0, 0}, {10, 500}, {123, 4567}} {
				oldReq := float64(io[0])*p.Input + float64(cached)*p.CacheRead + float64(cached/3)*p.writeRate(tier) + float64(io[1])*p.Output
				if got := p.RequestCost(io[0], cached, cached/3, io[1], tier); !same(got, oldReq) {
					t.Errorf("RequestCost(%v, %d, %v) = %v, want %v", io, cached, tier, got, oldReq)
				}
			}
		}
	}
	// The zero-generation semantics give a ping with no output.
	z := Semantics{ZeroGeneration: true}
	if got, want := p.RecreateCost(1000, TTL5m, z), 1000*p.Write5m+7*p.Input; !same(got, want) {
		t.Errorf("zero generation: %v want %v", got, want)
	}
}

// The simulator prices every replayed request with RequestCost.
func BenchmarkRequestCost(b *testing.B) {
	p := Pricing{Model: "m", Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, Write5m: 3.75e-6, Write1h: 6e-6}
	b.ReportAllocs()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += p.RequestCost(100, 50_000, 2000, 400, TTL5m)
	}
	_ = sink
}
