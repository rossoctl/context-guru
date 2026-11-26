package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// A first request of a session with a realistic cold write books its cost when the price is known
// (issue #920). The reported $0 row had no price: the proxy's price map had not loaded yet, so Price
// was called with accountingComplete true and p.Zero() true. tokens_before=49 plays no part.
func TestFirstRequestBooksCostWhenThePriceIsKnown(t *testing.T) {
	p := modelinfo.Price{Input: 3e-06, Output: 1.5e-05, CacheRead: 3e-07, CacheWrite: 3.75e-06}
	for _, tokensBefore := range []int{49, 29650} {
		e := &Event{Model: "claude-sonnet-4-5", Status: 200, TokensBefore: tokensBefore, CacheWrite: 29650, OutputTokens: 50}
		e.PriceMode(p, true)
		if e.TokenAccounting != AccountingComplete {
			t.Errorf("tokens_before=%d: token_accounting = %q, want complete", tokensBefore, e.TokenAccounting)
		}
		want := 29650*3.75e-06 + 50*1.5e-05
		if e.CostUSD < want*0.999 || e.CostUSD > want*1.001 {
			t.Errorf("tokens_before=%d: cost = %v, want %v", tokensBefore, e.CostUSD, want)
		}
	}
}

// Without a price the row is partial at $0. That is the honest answer for an unknown price. The fix for
// #920 is to make the price known before the first request, not to guess one here.
func TestFirstRequestWithoutAPriceIsPartial(t *testing.T) {
	e := &Event{Model: "claude-sonnet-4-5", Status: 200, TokensBefore: 49, CacheWrite: 29650}
	e.PriceMode(modelinfo.Price{}, true)
	if e.TokenAccounting != AccountingPartial || e.CostUSD != 0 {
		t.Errorf("got %q cost %v, want partial and 0", e.TokenAccounting, e.CostUSD)
	}
}
