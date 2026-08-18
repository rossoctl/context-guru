package cheapmodel

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// Prompt-cache minimums, MEASURED against the gateway rather than assumed.
//
// A provider silently ignores a cache_control breakpoint that sits below its minimum
// cacheable prefix: no error, no cache entry, cache_creation_input_tokens: 0. The
// numbers differ per model family, so a single constant would either waste writes on
// one family or forgo caching on another.
//
//	~1.5k prefix, claude-haiku-4-5  => write=0 read=0   (inert)
//	~4.5k prefix, claude-haiku-4-5  => write=5401 then read=5401
//	~1.5k prefix, claude-sonnet-5   => write=2653 then read=2653
const (
	minCacheableHaiku = 4096
	minCacheableSmall = 1024
)

// minCacheablePrefix returns the smallest system prefix worth marking for this model.
//
// An UNNAMEABLE model gets the haiku figure, not the smaller default: the failure we are
// avoiding is paying for a cache entry that is never read, so when we cannot tell which
// family we are talking to, the conservative answer is to demand the larger prefix and
// place no mark below it.
func minCacheablePrefix(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "sonnet"), strings.Contains(m, "opus"):
		return minCacheableSmall
	default:
		// haiku-class, and every id we cannot place — including a gateway alias that hides
		// the real model, which is the common case on a LiteLLM-style deployment.
		return minCacheableHaiku
	}
}

// systemBlocks renders ordered system text into Anthropic content blocks, marking the
// LAST one with a cache_control breakpoint only when the whole prefix clears the model's
// minimum cacheable size. Empty and blank blocks are dropped: a blank block is rejected
// by the API, and it would also change the cached prefix bytes for everyone else.
//
// Returns nil when there is nothing to send, so the caller omits the field entirely and
// the request stays byte-identical to one that never had a system prompt.
func systemBlocks(system []string, model string) []any {
	kept := make([]string, 0, len(system))
	total := 0
	for _, b := range system {
		if strings.TrimSpace(b) == "" {
			continue
		}
		kept = append(kept, b)
		total += tokens.Count(b)
	}
	if len(kept) == 0 {
		return nil
	}
	out := make([]any, 0, len(kept))
	for i, b := range kept {
		blk := map[string]any{"type": "text", "text": b}
		if i == len(kept)-1 && total >= minCacheablePrefix(model) {
			blk["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		out = append(out, blk)
	}
	return out
}

// CacheablePrefix reports whether a system prefix of promptTokens would actually be
// cached by this model. Exported so a caller pricing a call does not have to duplicate
// the table (the economic gate must not assume a cache it will not get).
func CacheablePrefix(model string, promptTokens int) bool {
	return promptTokens >= minCacheablePrefix(model)
}
