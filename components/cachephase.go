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
// compactor must fire on Unknown: CacheTTLMs and IdleMs are also zero whenever the cache-aware
// path is off entirely (a non-Anthropic-family provider with no cache_control breakpoint,
// `cache_mode: off`, a bypassed turn, a session's first turn), and on those deployments
// MaxCachedIdx stays -1 so every offloader is already permitted to rewrite deep history —
// declining there would protect nothing and disable the component. See Trigger.CacheAllows.
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
	if c == nil || c.CacheTTLMs <= 0 || c.IdleMs <= 0 {
		return 0, false
	}
	return time.Duration(c.CacheTTLMs-c.IdleMs) * time.Millisecond, true
}

// CachePhase classifies this request against preExpiry, the width of the window before the
// entry's believed expiry that the caller considers cheap to invalidate.
//
// ColdCache is checked FIRST and is not merely redundant against `remaining <= 0`: it is apply's
// own verdict, computed with its clock-skew margin over the same timestamps, and one cheap
// agreement check costs nothing next to a wrongly invalidated prefix. A turn apply has called
// cold is Cold here even if the arithmetic below would have said otherwise.
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
