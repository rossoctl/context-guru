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
	AnthropicDefaultCacheTTL = modelinfo.AnthropicCacheTTL5m
	OpenAIMinimumCacheTTL    = modelinfo.OpenAIMinimumCacheTTL
	anthropicDefaultTTL      = AnthropicDefaultCacheTTL
	extendedTTL              = modelinfo.AnthropicCacheTTL1h
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
		// The tier is the one the body REQUESTED; the shared rule maps it to a lifetime.
		return EntryLifetime(CacheFacts{Provider: provider, Model: model, LastOneHour: bodyAsksExtendedTTL(body)}), CacheLifetimeExact
	}
	if modelinfo.OpenAI30MinuteCache(string(provider), model) {
		return OpenAIMinimumCacheTTL, CacheLifetimeMinimum
	}
	return 0, CacheLifetimeUnknown
}

// EntryAgeMs is how old the provider's cache entry was when a request arrived: the gap since the last real
// request, cut short by a ping that refreshed the entry later than that request.
func EntryAgeMs(sinceMs, tsMs int64, refreshedAt time.Time) int64 {
	if refreshedAt.IsZero() || sinceMs <= 0 {
		return sinceMs
	}
	if age := tsMs - refreshedAt.UnixMilli(); age >= 0 && age < sinceMs {
		return age
	}
	return sinceMs
}

// BilledLifetimeMs is the lifetime of the entry a request tried to read, in ms; 0 when the lifetime is not
// exact. billedOneHour is the tier the provider BILLED that entry at, not the tier a request asked for: a
// one-hour ask billed at five minutes stays five minutes. billedOneHour false (also "unknown") keeps the base
// lifetime. refreshedOneHour says the last touch was a live free 1-hour ping; it is a second, separate reason
// for the 1-hour lifetime. The result is the 1-hour lifetime when either boolean is true.
func BilledLifetimeMs(provider bschemas.ModelProvider, model string, billedOneHour, refreshedOneHour bool) int64 {
	if _, kind := CacheLifetime(provider, model, nil); kind != CacheLifetimeExact {
		return 0
	}
	// The entry is at 1 hour when its last write was billed there, or when its last touch was a live free
	// 1-hour ping (#1350). The host passes the two facts; the rule is here.
	return EntryLifetime(CacheFacts{Provider: provider, Model: model, BilledOneHour: billedOneHour, LastOneHour: refreshedOneHour}).Milliseconds()
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
// request, too short lets a component rewrite a live prefix. Both this and the keep-alive
// take the lifetime from modelinfo.CacheLifetime.
func restartGrace(provider bschemas.ModelProvider, ttl time.Duration, kind CacheLifetimeKind) time.Duration {
	if provider == bschemas.OpenAI {
		return OpenAIMinimumCacheTTL
	}
	if kind == CacheLifetimeUnknown {
		return AnthropicDefaultCacheTTL
	}
	return ttl
}
