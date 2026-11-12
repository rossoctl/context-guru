package bobplugin

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// bobcoinsPerMillion is Bob's measured rate, in Bobcoins per million tokens.
const bobcoinsPerMillion = 2.0

// The plugin's prices.yaml is what the proxy loads as MODEL_PRICES under bob-ultrahigh. A file the
// proxy cannot parse stops it at startup, so the shipped file itself is loaded here, the proxy's way.
// Every Bob tier must price a cache read and a cache write at its input rate: Bob's flat rate.
// Output too: IBM's own per-call credits are 2.0 x (prompt + completion) / 1e6 (20 calls, 0.0000%
// error, 2026-11-04), so the file is in Bobcoins, 2.0 per million for every token type.
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
		if perMillion := p.Input * 1e6; math.Abs(perMillion-bobcoinsPerMillion) > 1e-9 || p.Output != p.Input {
			t.Errorf("%s: input %v, output %v per million; Bob bills both at %v Bobcoins per million",
				tier, p.Input*1e6, p.Output*1e6, bobcoinsPerMillion)
		}
		if p.Unit() != "Bobcoins" {
			t.Errorf("%s: unit %q; Bob's file prices in Bobcoins, so its costs must say so", tier, p.Unit())
		}
		if p.Write1hFrac != 1.0 {
			t.Errorf("%s: 1-hour write = %vx input, Bob bills it at 1x", tier, p.Write1hFrac)
		}
	}
}
