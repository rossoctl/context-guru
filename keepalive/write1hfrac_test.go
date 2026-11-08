package keepalive

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// A keep-alive ping or substitute prices its 1-hour writes at the rate the price list states.
// An entry at the input rate pays no 1-hour extra cost. A Claude row still pays the 2.0x rate.
func TestKeepAliveCostUsesThePriceListOneHourRate(t *testing.T) {
	tb, err := modelinfo.ParseTable([]byte("models:\n" +
		"  - {match: acme/flat-1, in: 3, out: 15, cache_write_1h_frac: 1.0}\n" +
		"  - {match: claude-sonnet*, in: 3, out: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	k, fd, _ := testKeeper(t)
	fd.setPrices(proxyPricer{tb})
	u := Usage{FreshInput: 4, CacheWrite: 20315, CacheWrite1h: 20157, Output: 12}
	flat := 20319*3e-6 + 12*15e-6

	got, priced := k.pingCost("", "acme/flat-1", 20315, u)
	if !priced || math.Abs(got-flat) > 1e-12 {
		t.Errorf("acme/flat-1 ping cost = %v (priced %v), want %v", got, priced, flat)
	}
	if got := k.sweepAnswerUSD(pingJob{e: &kaEntry{model: "acme/flat-1"}}, "", u); math.Abs(got-flat) > 1e-12 {
		t.Errorf("acme/flat-1 substitute answer cost = %v, want %v", got, flat)
	}
	claude := 4*3e-6 + 20315*3.75e-6 + 12*15e-6 + 20157*0.75*3e-6
	if got, _ := k.pingCost("", "claude-sonnet-4-6", 20315, u); math.Abs(got-claude) > 1e-12 {
		t.Errorf("Claude ping cost = %v, want %v", got, claude)
	}
}
