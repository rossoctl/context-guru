package modelinfo

import "testing"

// #1248: a cache-read rate that no source states, for a model that is not Claude, is priced from
// Anthropic's multiple. That is a fallback rate, and Resolved must say so. The write tiers follow a
// rule per family, so they are never a fallback.
func TestResolvedMarksAFallbackReadRate(t *testing.T) {
	const in = 10e-6
	for _, tc := range []struct {
		name  string
		p     Price
		model string
		want  bool
	}{
		{"claude uses the published multiple", Price{Input: in}, "claude-opus-5-5", false},
		{"bedrock claude id", Price{Input: in}, "us.anthropic.claude-sonnet-5-5", false},
		{"non-Anthropic with no read rate", Price{Input: in}, "gemini-3-flash-preview", true},
		{"stated read rate", Price{Input: in, CacheRead: 2e-6}, "gpt-4o", false},
		{"operator multiple", Price{Input: in, ReadFrac: 0.5}, "gpt-4o", false},
		{"empty model is not known to be Claude", Price{Input: in}, "", true},
	} {
		got := tc.p.Resolved(tc.model)
		if got.CacheReadFallback != tc.want {
			t.Errorf("%s: CacheReadFallback = %v, want %v", tc.name, got.CacheReadFallback, tc.want)
		}
		// A second Resolved sees the filled rate as stated. It must keep the mark.
		if again := got.Resolved(tc.model); again != got {
			t.Errorf("%s: Resolved twice = %+v, want %+v", tc.name, again, got)
		}
	}
}

// FallbackCost is the cache reads priced at a fallback rate, and only those.
func TestFallbackCost(t *testing.T) {
	const in = 10e-6
	gem := Price{Input: in, Output: 4e-5}.Resolved("gemini-3-flash-preview")
	if got, want := gem.FallbackCost(1000), float64(1000*in*AnthropicCacheReadFrac); got-want > 1e-15 || want-got > 1e-15 {
		t.Errorf("gemini fallback = %v, want %v", got, want)
	}
	if got := gem.FallbackCost(0); got != 0 {
		t.Errorf("no cache reads: fallback = %v, want 0", got)
	}
	if got := (Price{Input: in, CacheRead: 2e-6}).Resolved("gpt-4o").FallbackCost(1000); got != 0 {
		t.Errorf("stated read rate: fallback = %v, want 0", got)
	}
	if got := (Price{Input: in}).Resolved("claude-opus-5-5").FallbackCost(1000); got != 0 {
		t.Errorf("claude: fallback = %v, want 0", got)
	}
	// litellm keeps a negative stated rate after Resolved. The fallback part is never below 0.
	neg := Price{Input: in}.Resolved("gpt-4o")
	neg.CacheRead = -1e-6
	if got := neg.FallbackCost(1000); got != 0 {
		t.Errorf("negative rate: fallback = %v, want 0", got)
	}
}
