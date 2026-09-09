package components

import (
	"testing"
	"time"
)

// The phase classification, row by row, with the phase NAME pinned rather than just a boolean.
//
// The name matters because two of these rows are the ones a boolean cannot tell apart, and the two
// callers treat them oppositely: Cold means "there is provably no prefix to disturb", Unknown means
// "the cache-aware path did not run and nothing is known". A predicate that collapsed them would
// look correct to extract_llm_sweep (which fires on neither) and be wrong for summarize (which
// fires on Unknown and must not fire on Cold-as-Unknown).
func TestCachePhaseClassifiesEachCacheState(t *testing.T) {
	const ttl = 5 * 60 * 1000
	ms := func(d time.Duration) int64 { return d.Milliseconds() }

	cases := []struct {
		name string
		c    *Ctx
		want CachePhase
	}{
		{"no Ctx at all", nil, CachePhaseUnknown},
		{"the cache-aware path did not run, so both figures are zero", &Ctx{}, CachePhaseUnknown},
		{"a TTL with no previous turn on record is unknown, not warm",
			&Ctx{CacheTTLMs: ttl}, CachePhaseUnknown},
		{"idle time with no TTL is unknown, not cold",
			&Ctx{IdleMs: ms(9 * time.Minute)}, CachePhaseUnknown},
		{"plenty of lifetime left is warm",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(30 * time.Second)}, CachePhaseWarm},
		{"just outside the window is still warm",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(3*time.Minute + 59*time.Second)}, CachePhaseWarm},
		{"exactly one window's width left is pre-expiry",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(4 * time.Minute)}, CachePhasePreExpiry},
		{"a few seconds left is pre-expiry",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(4*time.Minute + 55*time.Second)}, CachePhasePreExpiry},
		{"exactly at expiry is cold, not warm",
			&Ctx{CacheTTLMs: ttl, IdleMs: ttl}, CachePhaseCold},
		{"past expiry is cold",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(6 * time.Minute)}, CachePhaseCold},
		{"apply already called it cold, so it is cold whatever the arithmetic says",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(30 * time.Second), ColdCache: true}, CachePhaseCold},
		{"an hour-tier prefix an hour idle is cold",
			&Ctx{CacheTTLMs: ms(time.Hour), IdleMs: ms(61 * time.Minute)}, CachePhaseCold},
		{"an hour-tier prefix with a minute left is pre-expiry",
			&Ctx{CacheTTLMs: ms(time.Hour), IdleMs: ms(59*time.Minute + 30*time.Second)}, CachePhasePreExpiry},
		{"an hour-tier prefix five minutes idle is warm, where a 5m prefix would be cold",
			&Ctx{CacheTTLMs: ms(time.Hour), IdleMs: ms(5 * time.Minute)}, CachePhaseWarm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.CachePhase(DefaultPreExpiry); got != tc.want {
				t.Errorf("CachePhase = %s, want %s", got, tc.want)
			}
		})
	}
}

// A wider configured window must move the boundary, or pre_expiry_seconds is inert — which is how
// a knob whose value nothing measures becomes a knob nothing can tune either.
func TestAWiderWindowMovesThePreExpiryBoundary(t *testing.T) {
	c := &Ctx{CacheTTLMs: 5 * 60 * 1000, IdleMs: (2 * time.Minute).Milliseconds()} // 3 minutes left
	if got := c.CachePhase(time.Minute); got != CachePhaseWarm {
		t.Fatalf("with a one-minute window, three minutes left must be %s, got %s", CachePhaseWarm, got)
	}
	if got := c.CachePhase(4 * time.Minute); got != CachePhasePreExpiry {
		t.Errorf("with a four-minute window, three minutes left must be %s, got %s", CachePhasePreExpiry, got)
	}
}

// THE ASYMMETRY, AS AN EXECUTABLE CLAIM. Unknown is permitted by a size-gated compactor and refused
// by the sweep, and both directions are load-bearing.
//
// Unknown is not rare: CacheTTLMs and IdleMs are both zero whenever the cache-aware path did not run
// — a non-Anthropic-family provider with no cache_control breakpoint, `cache_mode: off`, a bypassed
// turn, a session's first turn. Failing closed there would make summarize a DEAD component on those
// deployments while protecting nothing, because with cacheAware false MaxCachedIdx stays -1 and
// every offloader is already permitted to rewrite deep history. There is no live prefix whose
// invalidation the refusal would be avoiding.
//
// The sweep's own gate is tested against sweeping() in components/offload; what is asserted here is
// only that CacheAllows does not answer for it.
func TestCacheAllowsPermitsUnknownWhileTheExactPhaseTestDoesNot(t *testing.T) {
	unknown := (&Ctx{}).CachePhase(DefaultPreExpiry)
	if unknown != CachePhaseUnknown {
		t.Fatalf("fixture does not produce Unknown, it produces %s — the rest of this test is vacuous", unknown)
	}
	for _, state := range []string{CacheStatePreExpiry, CacheStateCold, CacheStatePreExpiryOrCold} {
		if !(Trigger{CacheState: state}).CacheAllows(unknown) {
			t.Errorf("cache_state %q refused an UNKNOWN phase: on any deployment where the "+
				"cache-aware path does not run — a non-caching provider, cache_mode: off, a "+
				"bypassed turn, a first turn — this makes the component dead while protecting "+
				"nothing, because MaxCachedIdx is already -1 there", state)
		}
	}
	// The other half: the exact comparison the sweep uses must NOT be satisfied by Unknown, or the
	// sweep would ask a question against a prefix that may not exist and pay fresh for the whole
	// transcript.
	if unknown == CachePhasePreExpiry {
		t.Error("Unknown compares equal to PreExpiry, so the sweep's exact test would fire on a " +
			"deployment whose TTL could not be read")
	}
}

// The zero Trigger must stay permissive, in the new dimension as well as the old ones. Every config
// that never mentions cache_state depends on it, and a future refactor that made the zero value
// restrictive would silently disable three components at once.
func TestTheZeroTriggerPermitsEveryCachePhase(t *testing.T) {
	var zero Trigger
	for _, p := range []CachePhase{CachePhaseUnknown, CachePhaseWarm, CachePhasePreExpiry, CachePhaseCold} {
		if !zero.CacheAllows(p) {
			t.Errorf("the zero Trigger refused phase %s; a Trigger that names no cache_state must "+
				"impose no cache constraint", p)
		}
	}
	if !zero.FracResolvable(nil) {
		t.Error("the zero Trigger declined for want of a resolvable window, but it configures no " +
			"fraction — the window's provenance cannot matter to it")
	}
}

// CacheRemaining is the shared arithmetic, and ok=false must mean unknown rather than zero: zero is
// a positive claim that the entry has expired, and a caller that conflated them would read every
// non-cache-aware turn as an expired prefix.
func TestCacheRemainingSeparatesUnknownFromExpired(t *testing.T) {
	if _, ok := (&Ctx{}).CacheRemaining(); ok {
		t.Error("a Ctx with no cache figures reported a known remaining lifetime")
	}
	d, ok := (&Ctx{CacheTTLMs: 300_000, IdleMs: 300_000}).CacheRemaining()
	if !ok || d != 0 {
		t.Errorf("an exactly-expired entry: got (%v, %v), want (0, true) — expiry is KNOWN, and "+
			"reporting it as unknown would hide it from every caller", d, ok)
	}
}
