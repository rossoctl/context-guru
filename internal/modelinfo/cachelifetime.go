package modelinfo

import (
	"strconv"
	"time"
)

// The Anthropic cache tiers. These are the ONE place the two lifetimes live.
const (
	AnthropicCacheTTL5m = 5 * time.Minute
	AnthropicCacheTTL1h = time.Hour
)

// FixedCacheLifetime reports a (provider, model) whose cache lifetime does not depend on a
// requested or billed tier. Only GPT-5.6+ on OpenAI qualifies: its documented minimum is 30
// minutes, and a one-hour tier is never requested or billed there. An older OpenAI model has a
// variable, undocumented retention, so it is NOT fixed and reads like any other provider.
func FixedCacheLifetime(provider, model string) (time.Duration, bool) {
	if OpenAI30MinuteCache(provider, model) {
		return OpenAIMinimumCacheTTL, true
	}
	return 0, false
}

// CacheLifetime is THE rule for how long a provider keeps a cache entry after a read.
//
// oneHour is the tier, and the CALLER chooses which fact it stands for: the tier the request
// ASKED for (from the body or the row's cache_ttl), or the tier the provider BILLED
// (cache_write_1h > 0). They differ when a one-hour request is billed at five minutes. A caller
// that acts on the entry (the keeper, a "cold after" display) should pass the billed tier; a
// caller that has only the request should pass the requested one. oneHour is ignored for a
// provider with a fixed lifetime. The model is a parameter so that no caller can apply the
// GPT-5.6+ minimum to an older OpenAI model by forgetting the gate.
func CacheLifetime(provider, model string, oneHour bool) time.Duration {
	if d, ok := FixedCacheLifetime(provider, model); ok {
		return d
	}
	if oneHour {
		return AnthropicCacheTTL1h
	}
	return AnthropicCacheTTL5m
}

// CacheLifetimeSQL is CacheLifetime as a SQL CASE expression, in units of unit (time.Second or
// time.Millisecond). providerCol is a column or expression holding the provider name;
// oneHourCond is a SQL boolean for the one-hour tier. Both are inserted verbatim, so pass only
// code, never user input. SQL cannot parse a model name, so this gates on the PROVIDER only:
// use it only on rows a Go write path already gated on the model (kaSavedGate), or fetch the
// tier alone and finish in Go with CacheLifetime. The values come from the same constants as CacheLifetime, so the Go
// and SQL copies cannot disagree.
func CacheLifetimeSQL(providerCol, oneHourCond string, unit time.Duration) string {
	n := func(d time.Duration) string { return strconv.FormatInt(int64(d/unit), 10) }
	return `(CASE WHEN ` + providerCol + ` = 'openai' THEN ` + n(OpenAIMinimumCacheTTL) +
		` WHEN ` + oneHourCond + ` THEN ` + n(AnthropicCacheTTL1h) +
		` ELSE ` + n(AnthropicCacheTTL5m) + ` END)`
}
