package cheapmodel

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// DefaultMaxTokens is the reply budget for one cheap-model call.
//
// Raised from 2048 after a measured failure: on a real session a sonnet-class extraction
// model's reply stopped exactly at 2048, so the Starlark program was incomplete and
// unparseable and the call bought nothing for 26.8 s and ~$0.08. A truncated reply is the
// worst outcome available — full price, zero result — and output tokens are billed on what is
// actually produced, so a larger cap costs nothing when it is not used. Callers that reserve
// this out of an input budget must use this constant, not a literal, or the two drift.
const DefaultMaxTokens = 4096

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

// realTokensPerO200k converts our own token count into the count the provider will bill.
//
// THE FLOORS ABOVE ARE IN THE PROVIDER'S TOKENS; internal/tokens counts o200k_base. Comparing
// the two directly is a unit error, and it was silently costing every haiku call its cache.
// MEASURED 2026-08-19 against the gateway, same bytes both sides:
//
//	preamble+context, claude-haiku-4-5:  3,673 o200k  ->  4,217 billed  (1.148x)
//	same shape, re-measured 2026-08-20:   6,143 o200k  ->  7,077 billed  (1.152x)
//	preamble only,    aws/claude-sonnet-5: 1,893 o200k ->  2,956 billed  (1.56x)
//
// The 3,682-o200k prefix CACHED (write=4,412 then read=4,412) while the unconverted
// comparison (3,682 < 4,096) withheld the breakpoint — a cache the provider was willing to
// grant, declined on arithmetic. 1.20x is kept as the DIVISOR deliberately: it is looser than
// the 1.148-1.152x actually measured, so the derived floor (4096/1.20 = 3413 o200k) sits below
// the true one (4096/1.152 = 3555) and the breakpoint is offered slightly early rather than
// withheld. A too-tight divisor would reintroduce the bug this comment exists to explain.
// haiku-class is the family
// carrying the 4,096 floor, so it is the right conversion here; it is also the smaller of
// the two, so it never claims a cache we have evidence would be refused.
//
// Being wrong in the LOOSE direction is nearly free: a breakpoint below the provider's real
// minimum is ignored, not charged (measured: write=0, read=0, input_tokens identical with and
// without the mark), and claimCacheWrite's release() records that and retries later. Being
// wrong in the TIGHT direction forgoes the cache permanently, which is what happened here.
const realTokensPerO200k = 1.20

// minCacheableO200k is minCacheablePrefix expressed in the units tokens.Count actually
// returns, so like is compared with like.
func minCacheableO200k(model string) int {
	return int(float64(minCacheablePrefix(model)) / realTokensPerO200k)
}

// minCacheablePrefix returns the smallest system prefix worth marking for this model, in the
// PROVIDER's tokens. Callers holding an o200k count must go through minCacheableO200k.
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
func systemBlocks(system []string, model string) (blocks []any, release func(wrote, read bool)) {
	kept := make([]string, 0, len(system))
	total := 0
	for _, b := range system {
		if strings.TrimSpace(b) == "" {
			continue
		}
		kept = append(kept, b)
		total += tokens.Count(b)
	}
	noop := func(bool, bool) {}
	if len(kept) == 0 {
		return nil, noop
	}
	// Two independent conditions, both measured: the prefix must be big enough for the
	// provider to cache it at all, and a read must be able to follow the write.
	mark := false
	release = noop
	if total >= minCacheableO200k(model) {
		mark, release = claimCacheWrite(model, strings.Join(kept, "\x00"))
	}
	out := make([]any, 0, len(kept))
	for i, b := range kept {
		blk := map[string]any{"type": "text", "text": b}
		if i == len(kept)-1 && mark {
			blk["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		out = append(out, blk)
	}
	return out, release
}

// CacheablePrefix reports whether a system prefix of promptTokens would actually be
// cached by this model. Exported so a caller pricing a call does not have to duplicate
// the table (the economic gate must not assume a cache it will not get). promptTokens is an
// o200k count (internal/tokens), the same unit systemBlocks uses, so the conversion to the
// provider's tokens happens in exactly one place.
func CacheablePrefix(model string, promptTokens int) bool {
	return promptTokens >= minCacheableO200k(model)
}

// --- Only write a cache entry something can read ------------------------------------
//
// The size floor above stops us asking for a cache the provider would ignore. It does not
// stop the other waste, which a live session made visible: two extraction calls in one
// request ran CONCURRENTLY, so neither could read what neither had written yet, and both
// paid the 1.25x cache-creation premium — measured cache_write=5228, cache_read=0. A cache
// entry that is only ever written is strictly worse than no breakpoint at all.
//
// So a call marks the prefix only when a read can plausibly follow: either this exact prefix
// has already been written on this model (so the mark IS the read), or no write for it is
// currently in flight and this call takes the one write slot. Concurrent siblings send no
// mark and pay plain fresh input, which is what they would have paid anyway.
//
// ponytail: process-wide map keyed by prefix hash + model, never pruned. It holds one small
// entry per distinct preamble per model — a handful in practice, since the whole point of a
// preamble is that it is invariant. Add eviction if a deployment ever generates prefixes
// dynamically.
type prefixState struct {
	written  bool
	inflight bool
	// at is when the entry was last observed to exist. The provider's ephemeral entry expires
	// after a few minutes of not being used, and `written` without an expiry was sticky for
	// the process lifetime: after an idle gap every concurrent caller would mark, each pay a
	// full creation charge, and the double-write waste this protocol exists to prevent would
	// return on the first burst after every quiet period.
	at time.Time
}

// prefixEntryTTL is how long we keep believing a cache entry exists. Deliberately under the
// provider's 5-minute ephemeral lifetime: being wrong in this direction costs one extra
// single-writer round (correct behaviour, slightly conservative), while being wrong the other
// way costs a concurrent burst of full-price writes.
const prefixEntryTTL = 4 * time.Minute

var (
	prefixMu    sync.Mutex
	prefixCache = map[string]*prefixState{}
)

// claimCacheWrite reports whether this call should carry the breakpoint, and returns a
// release func to be called once the response is in. release records whether a cache entry
// now exists, so later calls know the mark will be a read rather than another write.
func claimCacheWrite(model, prefix string) (mark bool, release func(wrote, read bool)) {
	sum := sha256.Sum256([]byte(model + "\x00" + prefix))
	key := string(sum[:])

	prefixMu.Lock()
	st := prefixCache[key]
	if st == nil {
		st = &prefixState{}
		prefixCache[key] = st
	}
	if st.written && time.Since(st.at) > prefixEntryTTL {
		st.written = false // the provider's entry has almost certainly expired
	}
	switch {
	case st.written:
		mark = true // already cached: marking asks for a READ
	case st.inflight:
		mark = false // a sibling is writing it; do not pay for a second copy
	default:
		st.inflight, mark = true, true
	}
	claimed := mark && !st.written
	prefixMu.Unlock()

	// Idempotent, and it MUST be called on every exit path including transport errors and
	// non-200s: a claimed write slot that is never released leaves inflight set forever, and
	// no later call would ever mark the prefix again.
	done := false
	return mark, func(wrote, read bool) {
		prefixMu.Lock()
		defer prefixMu.Unlock()
		if done {
			return
		}
		done = true
		if claimed {
			st.inflight = false
		}
		if wrote || read {
			st.written, st.at = true, time.Now()
			return
		}
		// Neither written nor read while we asked for it: the provider ignored the mark (a
		// prefix below its real minimum, a gateway that strips cache_control). Leave written
		// false so the next call re-tries rather than assuming a cache that does not exist.
	}
}

// resetPrefixCache is for tests: the map is process-wide, so one test's write state would
// otherwise decide another test's assertions.
func resetPrefixCache() {
	prefixMu.Lock()
	defer prefixMu.Unlock()
	prefixCache = map[string]*prefixState{}
}
