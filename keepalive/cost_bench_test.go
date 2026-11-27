package keepalive

import "testing"

// The keeper prices one ping and one miss for each recorded response. This is that cost.
func BenchmarkProjectedPingAndMissUSD(b *testing.B) {
	k, fd, _ := testKeeperWith(b, nil)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	b.ReportAllocs()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink += k.projectedPingUSD("gpt-5.6", 300_000, "/v1/messages", "")
		sink += k.projectedMissUSD("gpt-5.6", 300_000, "", false)
	}
	_ = sink
}

// The record path prices the ping and the miss on one price lookup.
func BenchmarkProjectedPingAndMissOnePrice(b *testing.B) {
	k, fd, _ := testKeeperWith(b, nil)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	b.ReportAllocs()
	var sink float64
	for i := 0; i < b.N; i++ {
		p, m := k.projectedPingAndMissUSD("gpt-5.6", 300_000, "/v1/messages", "", false, 0)
		sink += p + m
	}
	_ = sink
}
