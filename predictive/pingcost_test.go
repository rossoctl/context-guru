package predictive

import (
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

// The predictive ping cost and the keeper read one constant for a ping's own input (#1437).
func TestPingCostUsesTheKeeperConstant(t *testing.T) {
	if pingFresh != kvcache.DefaultPingInputTokens {
		t.Fatalf("pingFresh %d != kvcache.DefaultPingInputTokens %d", pingFresh, kvcache.DefaultPingInputTokens)
	}
	r := &Response{Prefix: 1000, PRead: 1, PIn: 10, POut: 100}
	if want := 1000.0 + float64(kvcache.DefaultPingInputTokens)*10 + 100; r.PingCost() != want {
		t.Fatalf("PingCost %v want %v", r.PingCost(), want)
	}
}
