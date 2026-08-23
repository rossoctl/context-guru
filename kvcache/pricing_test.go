package kvcache

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// stubPricer is an operator price list, in per-TOKEN units (the unit this package works in;
// the real Table converts from per-million on load).
type stubPricer map[string]modelinfo.Price

func (s stubPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	p, ok := s[model]
	return p, ok
}

// testRates are the IBM gateway's own claude-sonnet-5 rates, which are roughly half
// anthropic.com's list price — the exact reason internal/modelinfo.Table exists.
var testRates = modelinfo.Price{
	Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6,
}

// testPrices is the price list every simulator test replays against: one priced model ("m")
// and one the list has never heard of ("unpriced").
func testPrices() *PriceList {
	return NewPriceList(context.Background(), []string{"m", "unpriced"},
		stubPricer{"m": testRates}, Multipliers{}, nil)
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}

// The 1-hour creation rate is DERIVED, because no gateway publishes one, and it is derived
// from the multiplier against base INPUT rather than from the 5-minute rate.
func TestOneHourWriteIsDerivedFromTheMultiplierAgainstInput(t *testing.T) {
	p := FromPrice("m", testRates, Multipliers{}, SourcePriceList)
	if !p.Known {
		t.Fatal("a fully specified price list must be Known")
	}
	near(t, "input", p.Input, 3e-6)
	near(t, "write_5m", p.Write5m, 3.75e-6)    // the list's own cache_creation rate
	near(t, "write_1h", p.Write1h, 6e-6)       // 2.0 x input
	near(t, "cache_read", p.CacheRead, 0.3e-6) // the list's own read rate
	if p.PingOutputTokens != DefaultPingOutputTokens || p.PingInputTokens != DefaultPingInputTokens {
		t.Errorf("ping overhead = %d in / %d out; the assumption must be explicit, not zero",
			p.PingInputTokens, p.PingOutputTokens)
	}
	// A deployment whose provider charges differently says so, and nothing is hardcoded.
	q := FromPrice("m", testRates, Multipliers{Write1h: 3.0}, SourcePriceList)
	near(t, "write_1h at a 3.0x multiplier", q.Write1h, 9e-6)
}

// A price list that omits a cache tier must not leave a cached request priced as free.
func TestAMissingCacheTierIsFilledFromTheMultiplierNotLeftAtZero(t *testing.T) {
	p := FromPrice("m", modelinfo.Price{Input: 4e-6, Output: 20e-6}, Multipliers{}, SourcePriceList)
	near(t, "cache_read", p.CacheRead, 0.4e-6)
	near(t, "write_5m", p.Write5m, 5e-6)
	near(t, "write_1h", p.Write1h, 8e-6)
}

// A 1-hour entry can never be cheaper to create than a 5-minute one. A price list that says
// otherwise is a typo, and honouring it would make every 1h arm look free.
func TestOneHourWriteIsNeverCheaperThanFiveMinute(t *testing.T) {
	p := FromPrice("m", modelinfo.Price{Input: 1e-6, Output: 1e-6, CacheRead: 1e-7,
		CacheWrite: 9e-6}, Multipliers{}, SourcePriceList)
	if p.Write1h < p.Write5m {
		t.Errorf("write_1h %.9f < write_5m %.9f", p.Write1h, p.Write5m)
	}
	near(t, "write_1h clamped to write_5m", p.Write1h, 9e-6)
}

// An unpriced model gets a ROW that says so — never a zero rate, and never omission.
func TestAnUnpricedModelIsKnownFalseAndNotZeroRated(t *testing.T) {
	l := testPrices()
	p := l.For("unpriced")
	if p.Known {
		t.Fatal("a model the price list has never heard of must not read as priced")
	}
	if p.Source != "" {
		t.Errorf("source = %q; an unpriced row names no source", p.Source)
	}
	if l.For("never-seen").Model != "never-seen" {
		t.Error("a model outside the list must still come back naming itself, so the page can " +
			"report WHICH model it could not price")
	}
	// And it appears in the list, so the reader can tell "unpriced" from "no traffic".
	var found bool
	for _, m := range l.Models {
		if m.Model == "unpriced" {
			found = true
		}
	}
	if !found {
		t.Error("the unpriced model was dropped from the list instead of reported")
	}
}

// An override is how an operator prices a model the list has never heard of — a preview id,
// an internal route name, a server-resolved tier. It makes the row KNOWN.
func TestAnOverridePricesAModelTheListDoesNotKnow(t *testing.T) {
	in, w1h := 2e-6, 5e-6
	l := NewPriceList(context.Background(), []string{"unpriced"}, stubPricer{}, Multipliers{},
		map[string]Override{"unpriced": {Input: &in, Write1h: &w1h}})
	p := l.For("unpriced")
	if !p.Known {
		t.Fatal("an override must make the row priced")
	}
	if p.Source != "override" {
		t.Errorf("source = %q, want override — nobody may read a typed number as a configured one",
			p.Source)
	}
	near(t, "overridden input", p.Input, 2e-6)
	near(t, "overridden write_1h", p.Write1h, 5e-6)
	// A zero is a legitimate override value, and "not edited" must stay distinguishable from it.
	zero := 0.0
	q := Override{CacheRead: &zero}.Apply(FromPrice("m", testRates, Multipliers{}, SourcePriceList))
	near(t, "an override to zero", q.CacheRead, 0)
	if !q.Known {
		t.Error("zeroing one tier does not make a model unpriced")
	}
	if !(Override{}).Empty() {
		t.Error("an empty override must report itself empty")
	}
	if got := (Override{}).Apply(FromPrice("m", testRates, Multipliers{}, SourcePriceList)); got.Source != SourcePriceList {
		t.Errorf("an empty override changed the source to %q", got.Source)
	}
}

// The cost formulas, spelled out against hand arithmetic. Every dollar figure on the page
// comes through these four functions, so they are asserted rather than trusted.
func TestCostFormulas(t *testing.T) {
	p := FromPrice("m", testRates, Multipliers{}, SourcePriceList)
	sem := DefaultSemantics()

	// request_cost = input×3e-6 + read×0.3e-6 + write×write_rate + output×15e-6
	near(t, "a 5m write of 100k with 200 fresh input and 50 output",
		p.RequestCost(200, 0, 100_000, 50, TTL5m),
		200*3e-6+100_000*3.75e-6+50*15e-6)
	near(t, "the same request at the 1h tier",
		p.RequestCost(200, 0, 100_000, 50, TTL1h),
		200*3e-6+100_000*6e-6+50*15e-6)
	near(t, "a hit that reads 100k and writes nothing",
		p.RequestCost(200, 100_000, 0, 50, TTL5m),
		200*3e-6+100_000*0.3e-6+50*15e-6)
	// No cache_control: there is no write rate at all, and the caller must have billed the
	// prefix as fresh input instead.
	near(t, "no tier has no write rate", p.RequestCost(0, 0, 100_000, 0, TTLNone), 0)

	// keep_alive_cost = cached×0.3e-6 + 1×3e-6 + 1×15e-6
	near(t, "one keep-alive on a 100k prefix", p.KeepAliveCost(100_000, sem),
		100_000*0.3e-6+3e-6+15e-6)
	// A provider that accepts a zero-generation request drops the output token.
	near(t, "one keep-alive with zero generation",
		p.KeepAliveCost(100_000, Semantics{ZeroGeneration: true}),
		100_000*0.3e-6+3e-6)

	// recreate_cost pays the WRITE rate, which is where the 12.5x and 20x live.
	near(t, "a late keep-alive at the 5m tier", p.RecreateCost(100_000, TTL5m, sem),
		100_000*3.75e-6+3e-6+15e-6)
	near(t, "a late keep-alive at the 1h tier", p.RecreateCost(100_000, TTL1h, sem),
		100_000*6e-6+3e-6+15e-6)
	// The ratio the whole mechanism rests on: a refresh against a re-creation.
	if r := (100_000 * 3.75e-6) / (100_000 * 0.3e-6); math.Abs(r-12.5) > 1e-9 {
		t.Errorf("a 5m re-creation is %.3fx a read, want 12.5x", r)
	}

	// uncached_cost = (input+prefix)×input + output×output
	near(t, "the same prompt with no cache at all", p.UncachedCost(200, 100_000, 50),
		(200+100_000)*3e-6+50*15e-6)

	// hold_cost = creation + pings×keep_alive
	near(t, "holding 100k at 5m with two refreshes", p.HoldCost(100_000, TTL5m, 2, sem),
		100_000*3.75e-6+2*(100_000*0.3e-6+3e-6+15e-6))
	near(t, "holding 100k at 1h with no refresh", p.HoldCost(100_000, TTL1h, 0, sem),
		100_000*6e-6)
}

// The multipliers are the deployment's, and a zero means "use the default" so a partially
// filled struct is safe rather than free.
func TestMultiplierDefaults(t *testing.T) {
	m := Multipliers{Write1h: 2.5}.WithDefaults()
	if m.CacheRead != DefaultCacheReadMultiple || m.Write5m != DefaultWrite5mMultiple {
		t.Errorf("a partially filled Multipliers lost its defaults: %+v", m)
	}
	if m.Write1h != 2.5 {
		t.Errorf("the supplied multiplier was overwritten: %v", m.Write1h)
	}
	if d := DefaultMultipliers(); d.CacheRead != 0.1 || d.Write5m != 1.25 || d.Write1h != 2.0 {
		t.Errorf("the shipped multiples moved: %+v", d)
	}
}
