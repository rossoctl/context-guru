package modelinfo

import "testing"

// TestCacheWritePremiumIsPerFamily pins BOTH directions. A one-sided test is how the
// previous reader concluded the fabricated multiple was "always conservative": it
// under-reports on Anthropic and OVER-reports everywhere else, and only a test that
// asserts both signs stops the next change from re-introducing one of them.
func TestCacheWritePremiumIsPerFamily(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"claude-opus-5", 1.25},
		{"claude-haiku-4-5", 1.25},
		{"aws/claude-sonnet-5", 1.25},         // Bedrock prefix
		{"anthropic.claude-3-5-sonnet", 1.25}, // Bedrock native id
		{"azure/gpt-5.5", 1.0},                // no creation premium
		{"gpt-4o", 1.0},
		{"gcp/gemini-2.5-pro", 1.0},
		{"gemini-3-flash-preview", 1.0},
		{"aws/gpt-oss-120b", 1.0},
		{"some-model-nobody-has-heard-of", 1.0}, // unknown: do not invent a premium
	} {
		if got := CacheWriteFracFor(tc.model); got != tc.want {
			t.Errorf("CacheWriteFracFor(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestFabricatedPremiumDoesNotReachNonAnthropicSavings is the dollar consequence: the
// same 100k saved tokens must not be priced 25% higher just because the price feed
// omitted a rate the provider never charges.
func TestFabricatedPremiumDoesNotReachNonAnthropicSavings(t *testing.T) {
	const input = 2.50 / 1e6 // azure/gpt-5.5's operator rate
	claude := input * CacheWriteFracFor("claude-opus-5")
	gpt := input * CacheWriteFracFor("azure/gpt-5.5")
	if claude <= gpt {
		t.Fatalf("Anthropic write rate %v should exceed a family with no premium %v", claude, gpt)
	}
	if got, want := claude/gpt, 1.25; got != want {
		t.Fatalf("premium ratio = %v, want %v", got, want)
	}
	// 100k tokens of savings, the shape baselineDeltaUSD prices.
	if over := 100_000 * (input*1.25 - gpt); over <= 0 {
		t.Fatalf("expected the old flat multiple to over-report, got %v", over)
	}
}
