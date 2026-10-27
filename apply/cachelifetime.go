package apply

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// CacheLifetimeKind distinguishes a known expiry from a minimum guarantee. A
// minimum may prove an entry is still eligible, but cannot prove it has expired.
type CacheLifetimeKind uint8

const (
	CacheLifetimeUnknown CacheLifetimeKind = iota
	CacheLifetimeExact
	CacheLifetimeMinimum
)

const (
	AnthropicDefaultCacheTTL = 5 * time.Minute
	OpenAIMinimumCacheTTL    = modelinfo.OpenAIMinimumCacheTTL
	anthropicDefaultTTL      = AnthropicDefaultCacheTTL
	extendedTTL              = time.Hour
	coldMargin               = time.Minute
)

// CacheLifetime returns the only lifetime facts the cache gate may act on.
// GPT-5.6+ defaults to a 30-minute MINIMUM after the latest write or reuse;
// OpenAI may retain the prefix longer, with no documented maximum. Therefore
// even a very old OpenAI entry cannot safely set ColdCache or lift TailOnlyCold.
// Earlier OpenAI models have variable retention, so their lifetime is unknown
// without a reliable per-request fact.
func CacheLifetime(provider bschemas.ModelProvider, model string, body []byte) (time.Duration, CacheLifetimeKind) {
	if explicitBreakpointProvider(provider) {
		if bodyAsksExtendedTTL(body) {
			return extendedTTL, CacheLifetimeExact
		}
		return AnthropicDefaultCacheTTL, CacheLifetimeExact
	}
	if modelinfo.OpenAI30MinuteCache(string(provider), model) {
		return OpenAIMinimumCacheTTL, CacheLifetimeMinimum
	}
	return 0, CacheLifetimeUnknown
}

// restartGrace is the restart grace for a thread whose first request after the restart carries
// this lifetime (#496): the lifetime itself — the longest an entry a restart forgot can still
// be alive — with two substitutions.
//
// Every OpenAI request gets the 30-minute minimum, whatever CacheLifetime says about the model.
// A pre-GPT-5.6 model's Unknown is a statement about what the COLD gate may conclude (it must
// not declare cold on an undocumented retention), not a claim that the prefix is gone in 5
// minutes; for the grace the safe read is the documented minimum. CacheLifetime itself is
// unchanged — the cold gate needs it as it is.
//
// A non-OpenAI model whose retention is unknown gets the SHORTEST known lifetime, 5 minutes,
// because the grace errs in both directions: too long freezes a brand-new thread's first
// request, too short lets a component rewrite a live prefix. The keep-alive has its own copy
// of the lifetime rule (keepalive.cacheLifetime); this one stays separate so neither imports
// the other.
func restartGrace(provider bschemas.ModelProvider, ttl time.Duration, kind CacheLifetimeKind) time.Duration {
	if provider == bschemas.OpenAI {
		return OpenAIMinimumCacheTTL
	}
	if kind == CacheLifetimeUnknown {
		return AnthropicDefaultCacheTTL
	}
	return ttl
}
