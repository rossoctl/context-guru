package apply

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// CacheFacts are the metadata facts that say how long the provider keeps one thread's cache entry NOW
// (#1350). Nothing here is content: a provider, a model, two tiers and a floor.
//
// EntryLifetime and IsCold are the ONE place that turns them into a lifetime and a verdict. Every reader
// asks them: the cold check (apply), the keeper (cacheTTL, the substitute windows, the 1-hour probe, the
// 1-hour touch), the dashboard's cache clock and its miss attribution. No other code writes "5 minutes",
// "30 minutes" or "1 hour" for a cache lifetime. The table underneath is modelinfo.CacheLifetime.
//
// Why in apply: apply, keepalive, dash and the proxy already import it, and it imports none of them, so
// all can call it without a cycle. The tier tables stay in internal/modelinfo.
type CacheFacts struct {
	Provider bschemas.ModelProvider
	Model    string
	// LastOneHour is the tier the LAST touch of the entry left it at, whoever made that touch: a real
	// request that asked 1 hour, or a keep-alive request that did (a free 1-hour ping). The last touch
	// counts, not the longest: a 5-minute touch after a 1-hour ping moves the entry back (#1258).
	LastOneHour bool
	// BilledOneHour is the tier the provider billed the entry's last write at, when the caller has only
	// that (the dashboard rows). It widens like LastOneHour: either one says 1 hour.
	BilledOneHour bool
	// Floor is the longest lifetime a safety guard has seen for this prefix (the monotone sessionTTL, the
	// parent's record). The lifetime is never shorter than it: the guards only ever err toward WARM.
	Floor time.Duration
}

// EntryLifetime is how long the provider keeps the entry after its last touch. A provider with a fixed
// lifetime (OpenAI GPT-5.6+, 30 minutes) ignores the tiers.
func EntryLifetime(f CacheFacts) time.Duration {
	return max(f.Floor, modelinfo.CacheLifetime(string(f.Provider), f.Model, f.LastOneHour || f.BilledOneHour))
}

// IsCold reports whether the entry is certainly gone: the last touch was prevAtMs, the question comes at
// nowMs, and the lifetime plus the margin has passed. An unknown last touch (0) is never cold.
func IsCold(f CacheFacts, prevAtMs, nowMs int64) bool {
	return cacheIsCold(prevAtMs, nowMs, EntryLifetime(f))
}
