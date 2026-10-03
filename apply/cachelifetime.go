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
	OpenAIMinimumCacheTTL    = 30 * time.Minute
	anthropicDefaultTTL      = AnthropicDefaultCacheTTL
	extendedTTL              = time.Hour
	coldMargin               = time.Minute
)

// CacheLifetime returns the only lifetime facts the cache gate may act on.
// GPT-5.6+ defaults to a 30-minute MINIMUM after the latest write or reuse;
// OpenAI may retain the prefix longer. Earlier OpenAI models have variable
// retention, so their lifetime is unknown without a reliable per-request fact.
func CacheLifetime(provider bschemas.ModelProvider, model string, body []byte) (time.Duration, CacheLifetimeKind) {
	switch provider {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		if bodyAsksExtendedTTL(body) {
			return extendedTTL, CacheLifetimeExact
		}
		return AnthropicDefaultCacheTTL, CacheLifetimeExact
	case bschemas.OpenAI:
		if modelinfo.GPT56OrLater(model) {
			return OpenAIMinimumCacheTTL, CacheLifetimeMinimum
		}
	}
	return 0, CacheLifetimeUnknown
}
