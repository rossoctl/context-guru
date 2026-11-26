package tooldefer

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// The ledger's gross and list cost are the accountant's removal figure for the same facts: the dashboard
// prices a declaration filter with the same function, so one request gives one number in both.
func TestPriceGrossAndListCostAreTheAccountantRemovalFigure(t *testing.T) {
	p := gpt56()
	f, _ := tokens.BilledDeltaFactor("opus")
	for _, tier := range []Tier{TierFresh, TierRead, TierWrite, TierWrite1h} {
		r := Request{Model: "opus", DeferredTokens: 7000, ListTokens: 300, Tier: tier, PromptTokens: 199_000}
		m, ok := Price(r, p)
		if !ok {
			t.Fatal("unpriced")
		}
		fig := accountant.RemovalSaving(accountant.RemovalInput{Model: "opus", Tier: tier.accountant(), Prompt: 199_000,
			Removed: 7000, Added: 300, Factor: f}, modelinfo.OneModel("opus", p))
		if !near(m.Gross, fig.SavedUSD) || !near(m.Cost, fig.AddedUSD) {
			t.Errorf("tier %d: ledger gross %v cost %v, accountant %v %v", tier, m.Gross, m.Cost, fig.SavedUSD, fig.AddedUSD)
		}
	}
}

// TierOf follows accountant.HeadTier. A usage with 1h tokens and no cache-write total is bad data
// (the 1h part is inside the write total): its head is fresh input, as it is on the dashboard.
func TestTierOfFollowsTheAccountantHeadTier(t *testing.T) {
	for _, c := range []struct {
		read, write, write1h int64
		want                 Tier
	}{
		{1, 5, 5, TierRead}, {0, 5, 5, TierWrite1h}, {0, 5, 0, TierWrite}, {0, 0, 0, TierFresh}, {0, 0, 5, TierFresh},
	} {
		if got := TierOf(c.read, c.write, c.write1h); got != c.want {
			t.Errorf("TierOf(%d,%d,%d) = %d, want %d", c.read, c.write, c.write1h, got, c.want)
		}
	}
}
