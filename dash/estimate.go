package dash

import "github.com/rossoctl/context-guru/internal/modelinfo"

// The what-if estimates.
//
// Adapt-compaction prices a setting nobody ran: "what would this account save if it compacted
// differently?". No billed request exists for that, so the accountant library, which explains a
// bill, cannot answer it. These functions are the one place where such a forward estimate is priced.
// An estimate is not a bill: the screens that use them label their figures as estimates.
//
// They read a request's token counts only. They do not depend on the legacy per-component rule
// (Event.ComponentSavedUSD), which is for rows written before the accountant switch and goes when
// the last such row has aged out (issue #1080).

// estimateRepeatRate is what the RE-SENT part of removed content would be billed at on a request that
// carried fresh, read and write tokens.
//
// The old answer was always the cache-read rate, on the reasoning that a re-sent
// transcript is served from the provider's cache. That is true only of a request
// whose cache actually HIT. When it missed, the provider re-billed the entire prompt
// — so the content we had removed would have been re-billed too, at the
// cache-creation rate (12.5x a read on the Anthropic family), or at the fresh rate
// where nothing was written.
//
// This is not a rounding correction. On production traffic, turns whose cache had
// expired were 4% of requests and 31% of spend, all of it cache_creation; pricing
// their re-sent remainder as reads understated the value of removing it by ~12x, on
// exactly the turns where removing it is worth the most.
//
// Three guards keep this from inflating, which is the only direction that matters for a
// savings figure:
//
//   - A PARTIAL hit (some read, some written) keeps the read rate: which side of the
//     boundary the removed content sat on is not knowable from the usage block.
//   - A cache WRITE only earns the write rate when the write actually covers the prompt.
//     `cache_write > 0` does not mean the prompt was cached: a client with one breakpoint
//     after `tools` bills `cache_creation=2k, input=100k`, and the removed transcript there
//     would have been billed FRESH, not at 1.25x. So the write rate needs the written part
//     to be at least as large as the fresh part.
//   - Everything else is the fresh rate, never zero.
//
// One tempting "correction" here is wrong, and the old wording of this paragraph invited it.
// In cache-aware mode compaction only touches the UNCACHED tail, so it is true that the
// content removed on THIS turn would have been billed fresh rather than as a read. But that
// is the UNIQUE term, which is already priced at the write rate — this function only ever
// prices the REPLAY term, and replayed content is content removed on an EARLIER turn that by
// now sits deep inside the cached prefix, where CacheRead is exactly right. Replay is ~93% of
// realized value, so re-pricing it fresh on every warm turn would inflate warm-turn savings
// roughly 6x with nothing behind it.
func estimateRepeatRate(p modelinfo.Price, fresh, read, write int64) float64 {
	if read > 0 {
		return p.CacheRead
	}
	if write > 0 && write >= fresh {
		return p.CacheWrite
	}
	return p.Input
}
