package bobplugin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The plugin's prices.yaml is what the proxy loads as MODEL_PRICES under bob-ultrahigh. A file the
// proxy cannot parse stops it at startup, so the shipped file itself is loaded here, the proxy's way.
// Every Bob tier must price a cache read and a cache write at its input rate: Bob's flat rate.
func TestBobPricesFileIsAFlatRateForEveryTier(t *testing.T) {
	table, err := modelinfo.LoadTable(filepath.Join("context-guru", "prices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{"premium", "premium-ide", "standard", "standard-ide", "fast", "fast-ide"} {
		p, ok := table.Price(context.Background(), tier)
		if !ok || p.Input <= 0 {
			t.Errorf("%s: no price (ok=%v, input=%v)", tier, ok, p.Input)
			continue
		}
		if p.CacheRead != p.Input || p.CacheWrite != p.Input {
			t.Errorf("%s: input %v, cache read %v, cache write %v; Bob bills all three the same",
				tier, p.Input, p.CacheRead, p.CacheWrite)
		}
		if p.Write1hFrac != 1.0 {
			t.Errorf("%s: 1-hour write = %vx input, Bob bills it at 1x", tier, p.Write1hFrac)
		}
	}
}
