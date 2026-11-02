package modelinfo

import (
	"strings"
	"time"
)

// The GPT-5.6+ minimum is refreshed by a cache read. Keep-alive leaves two
// minutes for scheduling and upstream latency before that guarantee ends.
const (
	OpenAIMinimumCacheTTL      = 30 * time.Minute
	OpenAIDefaultKeepAliveIdle = OpenAIMinimumCacheTTL - 2*time.Minute
)

// The provider's cache lifetimes. They run from the START of the previous request. Every cap on a
// ping time reads these two.
const (
	AnthropicCacheLifetime5m = 5 * time.Minute
	AnthropicCacheLifetime1h = time.Hour
)

// The Anthropic one-hour tier. This is the ONE place the one-hour ping time lives: 59 minutes
// after the previous request STARTED, one minute inside the lifetime, and one ping. Tune it here.
// The five-minute pair (240 s, 2 pings) stays in config.
const (
	AnthropicDefaultKeepAliveIdle1h     = 59 * time.Minute
	AnthropicDefaultKeepAliveMaxPings1h = 1
	// AnthropicMaxKeepAliveMaxPings1h caps K for a 1-hour thread. The credential hold is
	// (K+1) x 59 min, so 3 bounds it at 236 min; it matches the manager-strategy band.
	AnthropicMaxKeepAliveMaxPings1h = 3
)

// OpenAI30MinuteCache is the shared provider/model gate for the GPT-5.6+
// automatic-cache guarantee. Route-specific callers may impose further limits.
func OpenAI30MinuteCache(provider, model string) bool {
	return provider == "openai" && GPT56OrLater(model)
}

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
		return AnthropicCacheWrite5mFrac
	}
	return 1.0
}

// The Anthropic family's published cache multiples of the fresh input rate. This is the ONE place
// they are written. kvcache, dash, components/offload, cheapmodel and proxy all read them.
const (
	// AnthropicCacheReadFrac is the standard cache-READ multiple. Four models have a lower one:
	// see CacheReadFracFor. Use that function when the model is known.
	AnthropicCacheReadFrac = 0.1
	// AnthropicCacheReadFracOpusSonnet55 is the cache-READ multiple of Claude Opus 5.5 and
	// Claude Sonnet 5.5.
	AnthropicCacheReadFracOpusSonnet55 = 0.05
	// AnthropicCacheReadFracFableMythos51 is the cache-READ multiple of Claude Fable 5.1 and
	// Claude Mythos 5.1.
	AnthropicCacheReadFracFableMythos51 = 0.025
	// AnthropicCacheWrite5mFrac is the 5-minute cache-WRITE multiple.
	AnthropicCacheWrite5mFrac = 1.25
	// AnthropicCacheWrite1hFrac is the 1-hour cache-WRITE multiple. It is taken from the fresh
	// input rate, not from the 5-minute write rate, because that is how the provider documents it.
	AnthropicCacheWrite1hFrac = 2.0
)

// CacheReadFracFor is the cache-READ rate as a multiple of the fresh input rate, for a model
// whose price feed does not state one.
//
// Source: the official pricing page, https://platform.claude.com/docs/en/about-claude/pricing
// ("Prompt caching" table and footnotes), read 2026-11-02. A cache read costs 0.1x base input.
// Exceptions: 0.025x on Claude Fable 5.1 and Claude Mythos 5.1; 0.05x on Claude Opus 5.5 and
// Claude Sonnet 5.5. "All other models use the standard 0.1x multiplier." Fable 5, Mythos 5,
// Opus 5, Sonnet 5 and Haiku 5.5 stay at 0.1x. Cache WRITE multiples do not change by model.
//
// The match is on tier AND version. "claude-opus-5" is a prefix of "claude-opus-5-5", so a
// prefix test would give Opus 5 the Opus 5.5 rate. The version must be the whole "5-5" or "5.5",
// not the start of a longer number, so a date suffix such as "claude-opus-5-20260101" stays 0.1x.
// Vendor prefixes ("aws/", "us.anthropic.") and the "[1m]" suffix do not change the answer.
// Every other model, including every non-Anthropic one, gets AnthropicCacheReadFrac.
func CacheReadFracFor(model string) float64 {
	full, _ := Normalize(model)
	for _, t := range [...]struct {
		tier         string
		major, minor int
		frac         float64
	}{
		{"fable", 5, 1, AnthropicCacheReadFracFableMythos51},
		{"mythos", 5, 1, AnthropicCacheReadFracFableMythos51},
		{"opus", 5, 5, AnthropicCacheReadFracOpusSonnet55},
		{"sonnet", 5, 5, AnthropicCacheReadFracOpusSonnet55},
	} {
		if maj, min, ok := tierVersion(full, t.tier); ok && maj == t.major && min == t.minor {
			return t.frac
		}
	}
	return AnthropicCacheReadFrac
}

// tierVersion reads the "<major><sep><minor>" that follows a tier word in a normalized id.
// The separator is '-', '.' or '_'. It reports ok=false when the word is missing or no version
// follows. A missing minor reads as -1, so "claude-opus-5" is (5, -1), never (5, 5).
func tierVersion(full, tier string) (major, minor int, ok bool) {
	i := strings.LastIndex(full, tier)
	if i < 0 {
		return 0, 0, false
	}
	rest := full[i+len(tier):]
	if rest == "" || !isVersionSep(rest[0]) {
		return 0, 0, false
	}
	rest = rest[1:]
	major, rest, ok = leadingInt(rest)
	if !ok {
		return 0, 0, false
	}
	if rest == "" || !isVersionSep(rest[0]) {
		return major, -1, true
	}
	minor, _, ok = leadingInt(rest[1:])
	if !ok {
		return major, -1, true
	}
	return major, minor, true
}

func isVersionSep(c byte) bool { return c == '-' || c == '.' || c == '_' }

// leadingInt parses the run of digits at the start of s.
func leadingInt(s string) (n int, rest string, ok bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' && i < 9 {
		n = n*10 + int(s[i]-'0')
		i++
	}
	return n, s[i:], i > 0
}

// chargesCacheWritePremium reports whether this model bills a per-token surcharge
// to create a prompt-cache entry.
func chargesCacheWritePremium(model string) bool {
	// Bedrock and Vertex ids carry a vendor prefix ("aws/claude-opus-5",
	// "anthropic.claude-3-5-sonnet"), so IsClaude matches on the substring, not a prefix.
	return IsClaude(model) || GPT56OrLater(model)
}

// GPT56OrLater reports whether an OpenAI GPT model uses the GPT-5.6+ cache rules.
// Routed model IDs (for example, azure/gpt-5.6-luna) are supported.
func GPT56OrLater(model string) bool {
	model = strings.ToLower(model)
	// A routing prefix may itself contain an older GPT name. The final model
	// token is the one the request actually selects.
	i := strings.LastIndex(model, "gpt-")
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
