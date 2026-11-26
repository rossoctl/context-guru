package dash

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// tierUSD prices tokens by tier, for the expected values of the tests. It is written out here on purpose,
// apart from the accountant's own rule, so a test compares the library's figure with a plain sum. A 1h
// write bills at the 5m write rate plus modelinfo.Price.CacheWrite1hPremium.
func tierUSD(p modelinfo.Price, t accountant.TierTokens) float64 {
	return float64(t.Input)*p.Input + float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite5m)*p.CacheWrite + float64(t.CacheWrite1h)*(p.CacheWrite+p.CacheWrite1hPremium())
}
