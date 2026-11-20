package modelinfo

import "testing"

// #1248: a cache tier that no source states, for a model outside the Anthropic family, is priced
// from a default multiple. That is a fallback rate, and Resolved must say so per tier.
func TestResolvedMarksFallbackTiers(t *testing.T) {
	const in = 10e-6
	for _, tc := range []struct {
		name                string
		p                   Price
		model               string
		read, write, write1 bool
	}{
		{"claude uses the published multiples", Price{Input: in}, "claude-opus-5-5", false, false, false},
		{"non-Anthropic with no cache rate", Price{Input: in}, "gemini-3-flash-preview", true, true, true},
		{"stated read rate is not a fallback", Price{Input: in, CacheRead: 2e-6}, "gpt-4o", false, true, true},
		{"operator multiples are not a fallback", Price{Input: in, ReadFrac: 0.5, WriteFrac: 1, Write1hFrac: 1}, "gpt-4o", false, false, false},
		{"empty model is not known to be Anthropic", Price{Input: in}, "", true, true, true},
	} {
		got := tc.p.Resolved(tc.model)
		if got.CacheReadFallback != tc.read || got.CacheWriteFallback != tc.write || got.CacheWrite1hFallback != tc.write1 {
			t.Errorf("%s: fallback read/write/1h = %v/%v/%v, want %v/%v/%v", tc.name,
				got.CacheReadFallback, got.CacheWriteFallback, got.CacheWrite1hFallback, tc.read, tc.write, tc.write1)
		}
		// A second Resolved sees the filled rates as stated. It must keep the marks.
		if again := got.Resolved(tc.model); again != got {
			t.Errorf("%s: Resolved twice = %+v, want %+v", tc.name, again, got)
		}
	}
}

// FallbackCost is the part of CostWithCacheWrite1h that came from fallback tiers, and only that part.
func TestFallbackCost(t *testing.T) {
	const in = 10e-6
	near := func(a, b float64) bool { d := a - b; return d < 1e-15 && d > -1e-15 }
	gem := Price{Input: in, Output: 4e-5}.Resolved("gemini-3-flash-preview")
	if got := gem.FallbackCost(1000, 0, 0); !near(got, 1000*in*AnthropicCacheReadFrac) {
		t.Errorf("gemini read fallback = %v, want %v", got, 1000*in*AnthropicCacheReadFrac)
	}
	if got := gem.FallbackCost(0, 100, 0); !near(got, 100*in) {
		t.Errorf("gemini write fallback = %v, want %v (1.0x input)", got, 100*in)
	}
	if got := gem.FallbackCost(0, 100, 40); !near(got, 100*in+40*gem.CacheWrite1hPremium()) {
		t.Errorf("gemini 1h write fallback = %v, want the 5m write plus the 1h premium", got)
	}
	if got, all := gem.FallbackCost(1000, 100, 40), gem.CostWithCacheWrite1h(0, 1000, 100, 0, 40); !near(got, all) {
		t.Errorf("all-fallback tiers = %v, want the whole cache cost %v", got, all)
	}
	stated := Price{Input: in, CacheRead: 2e-6}.Resolved("gpt-4o")
	if got := stated.FallbackCost(1000, 100, 0); !near(got, 100*in) {
		t.Errorf("stated read, filled write = %v, want only the write %v", got, 100*in)
	}
	if got := (Price{Input: in}).Resolved("claude-opus-5-5").FallbackCost(1000, 100, 40); got != 0 {
		t.Errorf("claude fallback = %v, want 0", got)
	}
}
