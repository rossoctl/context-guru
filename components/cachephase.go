package components

import "time"

// CachePhase names where a request sits in the life of the prompt-cache entry its prefix
// populated. It exists because two components now need the same fact and deriving it twice is
// how the cold decision and the dashboard came to disagree once already (see ttlTier in
// apply): one fact, one reader.
//
// THE PHASE THAT MATTERS IS PreExpiry, AND IT IS THE RESOLUTION OF A CONTRADICTION. A component
// that both ASKS the provider something over the cached transcript and REWRITES deep history
// wants opposite cache states:
//
//	the ASK needs a WARM cache — a prefix ask reads an entry that must still exist, or the call
//	pays fresh for the whole transcript, which is the cost the design exists to avoid;
//	the REWRITE wants a COLD cache — rewriting deep history invalidates a live prefix and forces
//	a cache-write of the whole suffix at 1.25x fresh.
//
// Both are cheap in the window where the entry still exists but has little life left: the ask
// still reads it, and what the rewrite invalidates is nearly worthless.
//
// THE TTL IS DERIVED, NOT ASSUMED. Ctx.CacheTTLMs is the same figure apply's cold decision uses,
// read out of the request itself: a bare `ephemeral` mark is 5 minutes, an explicit `ttl: "1h"`
// is an hour, widened to the longest lifetime this prefix has ever asked for. Zero means the
// cache-aware path did not run, which is Unknown — NOT Warm and not Cold.
//
// WHAT CALLERS DO WITH Unknown IS THEIR POLICY, NOT THIS FUNCTION'S, and the two shipped
// callers answer it oppositely on purpose. extract_llm_sweep must not fire on Unknown: it would
// invalidate live prefixes on exactly the deployments whose TTL could not be read. A size-gated
// compactor must fire on Unknown, because Unknown is not rare or exotic: CacheTTLMs is zero
// whenever the cache-aware path is off entirely (a non-Anthropic-family provider with no
// cache_control breakpoint, `cache_mode: off`, a bypassed turn), and declining there would
// disable the component on those deployments.
//
// UNKNOWN DOES NOT IMPLY THERE IS NO LIVE PREFIX, and this comment used to claim it did — that
// "on those deployments MaxCachedIdx stays -1 so every offloader is already permitted to rewrite
// deep history". The implication is false, and a live run reached the gap: a request can carry
// MaxCachedIdx >= 0 with no readable TTL, in which case there IS a cached prefix and Unknown means
// only that we cannot say how much life is left in it. Two reachable shapes —
//
//   - apply's legacy no-Tracker path (every library consumer of BodyFull/BodyOpts) sets
//     MaxCachedIdx from the store and never sets CacheTTLMs at all;
//   - `cache_mode: on` against a provider whose TTL this repo does not derive.
//
// A third, the same-millisecond concurrent turn, was the one observed live; that one is now fixed
// at its root by making IdleMs distinguish zero from unknown. The remaining two are answered by
// Trigger.CacheAllows, which refuses Unknown when MaxCachedIdx says a prefix exists.
type CachePhase int

const (
	// CachePhaseUnknown means the cache-aware path did not run, so nothing is known about a
	// cached prefix — not that there isn't one.
	CachePhaseUnknown CachePhase = iota
	// CachePhaseWarm means the entry is believed live with meaningful lifetime left.
	CachePhaseWarm
	// CachePhasePreExpiry means the entry is believed live but within the caller's window of
	// expiring, so invalidating it costs little.
	CachePhasePreExpiry
	// CachePhaseCold means the entry is believed gone, so there is no prefix to disturb.
	CachePhaseCold
)

// String names the phase, so a test failure and a log line say which one rather than an int.
func (p CachePhase) String() string {
	switch p {
	case CachePhaseWarm:
		return "warm"
	case CachePhasePreExpiry:
		return "pre_expiry"
	case CachePhaseCold:
		return "cold"
	default:
		return "unknown"
	}
}

// CacheRemaining is the cache entry's believed remaining lifetime: the TTL this request asked
// for minus this session's idle time. ok=false means unknown, and a caller must not treat that
// as zero — zero is a positive claim that the entry has expired.
//
// Exported separately from CachePhase because the arithmetic is one subtraction over two Ctx
// fields, and anything else that wants it (a dashboard row, a keep-alive deadline) must read it
// here rather than re-derive a TTL of its own.
func (c *Ctx) CacheRemaining() (time.Duration, bool) {
	// IdleMs < 0 is unknown; IdleMs == 0 is a positive claim of zero idle, and the entry then has
	// its whole TTL left. Treating zero as unknown reported "cannot tell" for the WARMEST possible
	// request, and the compaction gate permits Unknown — see Ctx.IdleMs.
	if c == nil || c.CacheTTLMs <= 0 || c.IdleMs < 0 {
		return 0, false
	}
	return time.Duration(c.CacheTTLMs-c.IdleMs) * time.Millisecond, true
}

// CachePhase classifies this request against preExpiry, the width of the window before the
// entry's believed expiry that the caller considers cheap to invalidate.
//
// ColdCache is checked FIRST: it is apply's own verdict over the same timestamps, and a turn apply
// has called cold is Cold here even if the arithmetic below would have said otherwise.
//
// It is NOT a safety check, and an earlier version of this comment implied it was ("one cheap
// agreement check costs nothing next to a wrongly invalidated prefix"). Checking ColdCache first can
// only make this classifier MORE willing to say cold, never less, so it cannot protect a live
// prefix. The reachable disagreement was the opposite one — the arithmetic below calling an entry cold
// while apply called it warm — and that was answered by a stricter cold test carrying a clock-skew
// margin, not by this line. That test went away with the cold-gated cache states (see the note above
// the phase constants); apply's ColdCache, checked here, is now the only margin-bearing verdict.
func (c *Ctx) CachePhase(preExpiry time.Duration) CachePhase {
	if c == nil {
		return CachePhaseUnknown
	}
	if c.ColdCache {
		return CachePhaseCold
	}
	remaining, ok := c.CacheRemaining()
	if !ok {
		return CachePhaseUnknown
	}
	if remaining <= 0 {
		return CachePhaseCold
	}
	if remaining <= preExpiry {
		return CachePhasePreExpiry
	}
	return CachePhaseWarm
}

// COLD HERE MEANS "MIGHT BE GONE", NOT "CERTAINLY GONE", and the distinction outlived the code that
// used to encode it.
//
// This package once carried a second, STRICTER cold test — CertainlyColdByClock, which required the
// entry to be past nominal expiry by a clock-skew allowance before claiming it was gone. Two callers
// wanted opposite safe errors:
//
//   - extract_llm_sweep needs an entry that STILL EXISTS, because its prefix ask reads one. Wrongly
//     believing the entry is alive makes the ask pay fresh for the whole transcript, so the safe
//     error is to assume it is already gone — which the threshold above delivers by calling nominal
//     expiry Cold.
//   - a compaction gate needed an entry that was CERTAINLY GONE, because rewriting deep history on a
//     live prefix pays a cache-write of the whole suffix at 1.25x. The safe error ran the other way.
//
// The strict test went away with the `cold` and `pre_expiry_or_cold` cache states it existed to
// serve (see components.CacheStates). What remains true, and is why this note stays: the Cold verdict
// below is the SWEEP's threshold, and nothing should read it as proof that a prefix is safe to
// destroy. apply computes Ctx.ColdCache with its own skew margin (apply.coldMargin) and that flag is
// checked first, so a caller wanting the conservative answer should reach for the flag rather than
// re-deriving one here.

// FillDenominator is what a fill FRACTION is a fraction OF: C, the point at which the conversation's
// own compaction mechanism acts, falling back to the model's context window when C is unknown.
//
// It exists as one function so that "90% full" cannot come to mean two different things in two
// callers — the failure this repo has had with cache TTLs and with token rulers, twice each.
//
// THE FALLBACK IS THE WINDOW AND THAT IS CORRECT FOR TWO OF THE FOUR CASES: a deployment where
// nothing compacts (a raw API client) genuinely has C == W, because the conversation grows until the
// provider rejects it. It is a guess for the case where a client compacts and we have not observed it
// — and those two are not currently distinguishable, which is why CompactionPointSource is carried
// rather than the number alone.
//
// A GUESSED C IS STILL ACTED ON, deliberately, where a guessed WINDOW is not (see FracResolvable).
// The asymmetry is about the direction of harm. A wrong window fires the gate at the wrong absolute
// size against a live cache: five times too early on the Opus family, invalidating prefixes that had
// most of their life left. A wrong C only moves the moment within the window — and the cache-state
// conjunct still has to agree, so the turn is one where the write was due anyway. Too-high a C means
// the component never fires (no harm, no benefit); too-low means it compacts a smaller prefix at a
// moment that was already cheap. Refusing to act on an assumed C would disable the component on every
// model whose client behaviour has not been measured, which today is all of them but haiku.
func (c *Ctx) FillDenominator() int {
	if c == nil {
		return 0
	}
	if c.CompactionPoint > 0 {
		return c.CompactionPoint
	}
	return c.CtxWindow
}
