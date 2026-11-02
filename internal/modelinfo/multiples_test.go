package modelinfo

import (
	"context"
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
