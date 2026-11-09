package modelinfo

// The provider's minimum cacheable prefix, and the factor that turns our o200k token count into the
// provider's. ONE rule, used by cheapmodel (where to put a system breakpoint) and by apply (whether
// the prefix split is worth a breakpoint slot). It lived in cheapmodel; apply kept a flat 1024 copy
// that ignored the family and the unit (#798).

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
	MinCacheableHaiku = 4096
	MinCacheableSmall = 1024
)

// RealTokensPerO200k converts our own token count into the count the provider will bill.
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
const RealTokensPerO200k = 1.20

// MinCacheableO200k is MinCacheablePrefix expressed in the units tokens.Count actually
// returns, so like is compared with like.
func MinCacheableO200k(model string) int {
	return int(float64(MinCacheablePrefix(model)) / RealTokensPerO200k)
}

// MinCacheablePrefix returns the smallest system prefix worth marking for this model, in the
// PROVIDER's tokens. Callers holding an o200k count must go through MinCacheableO200k.
//
// An UNNAMEABLE model gets the haiku figure, not the smaller default: the failure we are
// avoiding is paying for a cache entry that is never read, so when we cannot tell which
// family we are talking to, the conservative answer is to demand the larger prefix and
// place no mark below it.
func MinCacheablePrefix(model string) int {
	switch Family(model) {
	case FamilySonnet, FamilyOpus:
		return MinCacheableSmall
	default:
		// Fable has no published minimum in this repo, so it keeps the haiku figure it always got.
		// haiku-class, and every id we cannot place — including a gateway alias that hides
		// the real model, which is the common case on a LiteLLM-style deployment.
		return MinCacheableHaiku
	}
}
