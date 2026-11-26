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

// The record path prices the ping and the miss on one lookup. It gives the same two figures as the
// separate functions, for a 5-minute and a 1-hour entry.
func TestProjectedPingAndMissUSDMatchesTheSeparateFunctions(t *testing.T) {
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{tieredGPT56()}})
	for _, oneHour := range []bool{false, true} {
		ping, miss := k.projectedPingAndMissUSD("gpt-5.6", 300_000, "/v1/messages", "", oneHour)
		if want := k.projectedPingUSD("gpt-5.6", 300_000, "/v1/messages", ""); !near(ping, want) {
			t.Errorf("ping %v want %v", ping, want)
		}
		if want := k.projectedMissUSD("gpt-5.6", 300_000, "", oneHour); !near(miss, want) {
			t.Errorf("miss %v want %v", miss, want)
		}
	}
	if p, m := k.projectedPingAndMissUSD("", 300_000, "/v1/messages", "", false); p != 0 || m != 0 {
		t.Errorf("no model: %v %v", p, m)
	}
}

// A ping's bill comes from the accountant now, with the exact bits of the formula that priced it before
// (modelinfo.Price.CostWithCacheWrite1h): the keeper's cost, the substitute's cost and the sweep answer's cost.
func TestKeeperBillIsTheSameBitsAsTheOldFormula(t *testing.T) {
	prices := []struct {
		model string
		p     modelinfo.Price
	}{
		{"claude-sonnet-5", modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}},
		{"gpt-5.6", tieredGPT56()},
		{"claude-haiku-5-5", modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}.WithTiers("claude-haiku-5-5")},
	}
	usages := []Usage{
		{FreshInput: 7, CacheRead: 48_000, Output: 1},
		{FreshInput: 100, CacheRead: 300_000, Output: 500},
		{FreshInput: 10, CacheRead: 120_000, CacheWrite: 5000, CacheWrite1h: 5000, Output: 200},
		{FreshInput: 10, CacheRead: 20_000, CacheWrite: 5000, CacheWrite1h: 3000, Output: 200},
		{FreshInput: 123_457, CacheRead: 98_765, CacheWrite: 54_321, CacheWrite1h: 20_000, Output: 4_567},
	}
	for _, c := range prices {
		k, fd, _ := testKeeper(t)
		fd.setPrices(proxyPricer{fixedPrice{c.p}})
		for _, u := range usages {
			size := modelinfo.PromptTokens(int64(u.FreshInput), int64(u.CacheRead), int64(u.CacheWrite))
			want := c.p.At(size).CostWithCacheWrite1h(u.FreshInput, u.CacheRead, u.CacheWrite, u.Output, u.CacheWrite1h)
			got, ok := k.pingCost("", c.model, 50_000, u)
			if !ok || got != want {
				t.Errorf("%s %+v: pingCost %v ok %v, old formula %v", c.model, u, got, ok, want)
			}
			// The sweep answer's bill leaves the read out and keeps the tier of the whole call.
			wantAns := c.p.At(size).CostWithCacheWrite1h(u.FreshInput, 0, u.CacheWrite, u.Output, u.CacheWrite1h)
			ans := u
			ans.CacheRead = 0
			if got := billedUSD(c.p, c.model, billedPrompt(u, 50_000), ans); got != wantAns {
				t.Errorf("%s %+v: answer bill %v, old formula %v", c.model, u, got, wantAns)
			}
		}
	}
}
