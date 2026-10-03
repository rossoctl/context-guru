package modelinfo

import "strings"

// CacheWriteFracFor is the cache-CREATION rate as a multiple of the fresh input rate,
// for a model whose price feed does not state one.
//
// It exists because a single fabricated multiple was applied to every model on earth.
// `1.25x input` applies to Anthropic and GPT-5.6 or later. Earlier OpenAI models and
// Gemini do not charge a per-token cache-write premium.
//
// Why that mattered even though those providers never report cache_write: Event.Price uses
// CacheWrite for cost_usd, where a zero cache_write count makes the rate irrelevant, AND
// baselineDeltaUSD uses it to price SAVED tokens, where the count is our own and the rate
// is applied regardless of what the request billed. So the fabricated premium reached the
// savings figure on traffic that could never have paid it.
//
// Unknown families get 1.0 rather than 1.25: for a SAVINGS number the direction that does
// not invent value is the safe one. A feed that states a real rate never reaches this function.
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

// chargesCacheWritePremium reports whether this model bills a per-token surcharge
// to create a prompt-cache entry.
func chargesCacheWritePremium(model string) bool {
	m := strings.ToLower(model)
	// Bedrock and Vertex ids carry a vendor prefix ("aws/claude-opus-5",
	// "anthropic.claude-3-5-sonnet"), so match on the substring, not a prefix.
	return strings.Contains(m, "claude") || strings.Contains(m, "anthropic") || GPT56OrLater(m)
}

// GPT56OrLater reports whether an OpenAI GPT model uses the GPT-5.6+ cache rules.
// Routed model IDs (for example, azure/gpt-5.6-luna) are supported.
func GPT56OrLater(model string) bool {
	model = strings.ToLower(model)
	i := strings.Index(model, "gpt-")
	if i < 0 {
		return false
	}
	v := model[i+4:]
	major, n := 0, 0
	for n < len(v) && v[n] >= '0' && v[n] <= '9' {
		major = major*10 + int(v[n]-'0')
		n++
	}
	if n == 0 || major != 5 {
		return major > 5
	}
	if n == len(v) || v[n] != '.' {
		return false
	}
	v = v[n+1:]
	minor, digits := 0, 0
	for digits < len(v) && v[digits] >= '0' && v[digits] <= '9' {
		minor = minor*10 + int(v[digits]-'0')
		digits++
	}
	return digits > 0 && minor >= 6
}
