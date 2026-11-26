package keepalive

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// MissUSD never returns a negative figure, and an unknown price is 0 (the guard fails open).
func TestMissUSDEdges(t *testing.T) {
	inverted := modelinfo.Price{Input: 1e-6, CacheRead: 3e-6, CacheWrite: 1e-6, Output: 5e-6}
	if got := MissUSD(inverted, "m", 1000, false); got != 0 {
		t.Errorf("a write rate below the read rate: %v, want 0", got)
	}
	if got := MissUSD(modelinfo.Price{}, "claude-sonnet-5", 1000, true); got != 0 {
		t.Errorf("an unknown price: %v, want 0", got)
	}
	if got := MissUSD(gpt55(), "gpt-5.5", 0, false); got != 0 {
		t.Errorf("no prefix: %v, want 0", got)
	}
}

// PingUSD reads the prefix, the 7 fresh input tokens and the output at the tier of the ping's prompt
// size: a prefix 7 tokens below a line sends a ping above it.
func TestPingUSDPricesTheWholePingAtItsOwnSize(t *testing.T) {
	p := tieredGPT56()
	below := PingUSD(p, "gpt-5.6", 271_990, 1) // 271,997: base rates
	if want := 271_990*0.5e-6 + kvcache.DefaultPingInputTokens*5e-6 + 30e-6; !near(below, want) {
		t.Errorf("below the line %v want %v", below, want)
	}
	above := PingUSD(p, "gpt-5.6", 271_995, 1) // 272,002: tier rates
	if want := 271_995*1e-6 + kvcache.DefaultPingInputTokens*10e-6 + 45e-6; !near(above, want) {
		t.Errorf("above the line %v want %v", above, want)
	}
	if got := PingUSD(modelinfo.Price{}, "gpt-5.6", 1000, 1); got != 0 {
		t.Errorf("an unknown price: %v, want 0", got)
	}
}

// The keeper's summary projection stays the read of the prefix plus the output guess: a summary call
// carries no ping body.
func TestProjectedSummaryUSDHasNoPingBody(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Output: 15e-6}}})
	if got, want := k.projectedSummaryUSD("claude-sonnet-5", "", 100_000), 100_000*0.3e-6+summaryOutputBudgetGuess*15e-6; !near(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}
