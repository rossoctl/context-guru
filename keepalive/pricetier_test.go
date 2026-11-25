package keepalive

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

func tieredGPT56() modelinfo.Price {
	return modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6")
}

func near(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}

// The per-ping dollar guard reads the cache-read rate of the prefix's own tier.
func TestProjectedPingUsesTheTierOfThePrefix(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	out := float64(pingOutputBudget("/v1/messages"))
	at := k.projectedPingUSD("gpt-5.6", 271_999, "/v1/messages", "")
	if want := 271_999*0.5e-6 + out*30e-6; !near(at, want) {
		t.Fatalf("at the line %v want base %v", at, want)
	}
	// A prefix of exactly 272,000 sends a ping of 272,001 tokens: the tier rate.
	edge := k.projectedPingUSD("gpt-5.6", 272_000, "/v1/messages", "")
	if want := 272_000*1e-6 + out*45e-6; !near(edge, want) {
		t.Fatalf("prefix 272,000 %v want tier %v", edge, want)
	}
	above := k.projectedPingUSD("gpt-5.6", 300_000, "/v1/messages", "")
	if want := 300_000*1e-6 + out*45e-6; !near(above, want) { // 2x read, 1.5x output
		t.Fatalf("above the line %v want tier %v", above, want)
	}
}

// A ping the guard let through at the base rate is refused above the line.
func TestPerPingGuardRefusesAboveTheLine(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	ceiling := 0.14 // 272k reads cost 0.136 + output; 280k at 2x costs 0.28
	if got := k.projectedPingUSD("gpt-5.6", 270_000, "/v1/messages", ""); got > ceiling {
		t.Fatalf("below the line %v should fit %v", got, ceiling)
	}
	if got := k.projectedPingUSD("gpt-5.6", 280_000, "/v1/messages", ""); got <= ceiling {
		t.Fatalf("above the line %v should exceed %v", got, ceiling)
	}
}

// A ping that has run is priced at the prompt size its usage billed.
func TestPingCostUsesBilledPromptSize(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	cost, ok := k.pingCost("", "gpt-5.6", 100_000, Usage{CacheRead: 300_000, Output: 1})
	if want := 300_000*1e-6 + 45e-6; !ok || !near(cost, want) {
		t.Fatalf("above the line: %v want %v", cost, want)
	}
	cost, _ = k.pingCost("", "gpt-5.6", 300_000, Usage{CacheRead: 100_000, Output: 1})
	if want := 100_000*0.5e-6 + 30e-6; !near(cost, want) {
		t.Fatalf("below the line (usage wins over the planned prefix): %v want %v", cost, want)
	}
}
