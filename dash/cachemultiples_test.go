package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// TestNextRequestUSDReadsTheSharedMultiples pins the cold figure of each tier to the shared
// multiples. The 1h tier is the input rate x 2.0, the 5m tier is the price's own write rate.
func TestNextRequestUSDReadsTheSharedMultiples(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	warm, cold := NextRequestUSD(p, 1000, 3600)
	if warm != 1000*p.CacheRead || cold != 1000*p.Input*modelinfo.AnthropicCacheWrite1hFrac {
		t.Errorf("1h: warm=%v cold=%v", warm, cold)
	}
	_, cold = NextRequestUSD(p, 1000, 300)
	if cold != 1000*p.CacheWrite {
		t.Errorf("5m cold=%v", cold)
	}
}
