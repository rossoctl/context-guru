package modelinfo

import (
	"context"
	"math"
	"testing"
)

// TestCacheMultiplesKeepTheirShippedValues pins the three Anthropic multiples. Every cost
// figure in the repo reads them. A change here moves dollars on every page.
func TestCacheMultiplesKeepTheirShippedValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"read", AnthropicCacheReadFrac, 0.1},
		{"write 5m", AnthropicCacheWrite5mFrac, 1.25},
		{"write 1h", AnthropicCacheWrite1hFrac, 2.0},
	} {
		if tc.got != tc.want {
			t.Errorf("%s multiple = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestPriceCacheWrite1h(t *testing.T) {
	p := Price{Input: 3e-6, CacheWrite: 3.75e-6}
	if got, want := p.CacheWrite1h(), 6e-6; got != want {
		t.Errorf("CacheWrite1h() = %v, want %v", got, want)
	}
	// CostWithCacheWrite1h adds (1h rate - 5m rate) on the 1h tokens, as before.
	got := p.CostWithCacheWrite1h(0, 0, 1000, 0, 1000)
	if want := 1000 * 6e-6; got != want {
		t.Errorf("CostWithCacheWrite1h = %v, want %v", got, want)
	}
}

// ParseTable fills a blank tier from the shared multiples and keeps 1.0 for a family
// that charges no write premium.
func TestParseTableDefaultsReadTheSharedMultiples(t *testing.T) {
	tb, err := ParseTable([]byte("models:\n  - {match: claude-x, in: 10, out: 50}\n  - {match: gemini-x, in: 10, out: 50}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := tb.Price(context.Background(), "claude-x")
	if c.CacheRead != c.Input*AnthropicCacheReadFrac || c.CacheWrite != c.Input*AnthropicCacheWrite5mFrac {
		t.Errorf("claude row = %+v", c)
	}
	g, _ := tb.Price(context.Background(), "gemini-x")
	if g.CacheWrite != g.Input {
		t.Errorf("gemini write = %v, want input %v", g.CacheWrite, g.Input)
	}
}

// The 1-hour rate is Input x AnthropicCacheWrite1hFrac for every caller. This pins the values
// the dash callers used as bare `2 * p.Input`, and the premium form `rate1h - rate5m`.
func TestCacheWrite1hMatchesTheOldBareLiteral(t *testing.T) {
	for _, in := range []float64{0, 1e-6, 3e-6, 15e-6} {
		p := Price{Input: in, CacheWrite: in * 1.25}
		if p.CacheWrite1h() != 2*in {
			t.Errorf("input %v: CacheWrite1h = %v, want %v", in, p.CacheWrite1h(), 2*in)
		}
		if got, want := p.CacheWrite1h()-p.CacheWrite, 2*in-p.CacheWrite; got != want {
			t.Errorf("input %v: premium = %v, want %v", in, got, want)
		}
	}
}

// The premium is the one amount every figure adds for a 1-hour write. It is never negative, even
// when an operator states a 5m write rate above the derived 1h one.
func TestCacheWrite1hPremium(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Price
		want float64
	}{
		{"derived", Price{Input: 3e-6, CacheWrite: 3.75e-6}, 2.25e-6},
		{"flat 1h", Price{Input: 3e-6, CacheWrite: 3.75e-6, Write1hFrac: 1.0}, 0},
		{"5m above 1h", Price{Input: 3e-6, CacheWrite: 9e-6}, 0},
	} {
		if got := tc.p.CacheWrite1hPremium(); math.Abs(got-tc.want) > 1e-15 {
			t.Errorf("%s: premium = %v, want %v", tc.name, got, tc.want)
		}
	}
}
