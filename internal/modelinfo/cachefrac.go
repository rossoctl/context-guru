package modelinfo

import "strings"

// CacheWriteFracFor is the cache-CREATION rate as a multiple of the fresh input rate,
// for a model whose price feed does not state one.
//
// It exists because a single fabricated multiple was applied to every model on earth.
// `1.25x input` is the Anthropic family's published 5-minute cache-write premium, and it
// is right for Claude — including Claude on Bedrock, which is what this deployment runs.
// It is WRONG for OpenAI and Gemini: neither charges a premium to CREATE a cache entry
// (they discount cached reads and, for Gemini's explicit caching, bill storage per hour —
// neither is a per-token write surcharge). So every OpenAI/Gemini row was getting a
// write rate 25% above anything that could be billed.
//
// Why that mattered even though those providers never report cache_write: Event.Price uses
// CacheWrite for cost_usd, where a zero cache_write count makes the rate irrelevant, AND
// baselineDeltaUSD uses it to price SAVED tokens, where the count is our own and the rate
// is applied regardless of what the request billed. So the fabricated premium reached the
// savings figure on traffic that could never have paid it.
//
// Unknown families get 1.0 rather than 1.25: for a SAVINGS number the direction that does
// not invent value is the safe one, and no provider outside the Anthropic family is known
// to charge a creation premium. A feed that states a real rate never reaches this function.
func CacheWriteFracFor(model string) float64 {
	if chargesCacheWritePremium(model) {
		return anthropicCacheWriteFrac
	}
	return 1.0
}

// anthropicCacheWriteFrac / anthropicCacheReadFrac are the Anthropic family's published
// multiples, named rather than repeated as bare literals in three files.
const (
	anthropicCacheWriteFrac = 1.25
	anthropicCacheReadFrac  = 0.1
)

// chargesCacheWritePremium reports whether this model's provider bills a per-token
// surcharge to create a prompt-cache entry. True for the Anthropic family only.
func chargesCacheWritePremium(model string) bool {
	m := strings.ToLower(model)
	// Bedrock and Vertex ids carry a vendor prefix ("aws/claude-opus-5",
	// "anthropic.claude-3-5-sonnet"), so match on the substring, not a prefix.
	return strings.Contains(m, "claude") || strings.Contains(m, "anthropic")
}
