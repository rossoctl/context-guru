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
			&Ctx{CacheTTLMs: ttl, IdleMs: -1}, CachePhaseUnknown},
		// ZERO IDLE IS A FACT, NOT A GAP, and reading it as one was a live defect. Two turns of one
		// session arriving in the same millisecond — which an agent issuing parallel sub-requests
		// produces routinely — used to classify Unknown, and the compaction gate PERMITS Unknown.
		// A run observed the gate opening over a live 8-message cached prefix on 13 of 26 turns
		// this way. Zero idle is the warmest cache there can be.
		{"zero idle is the warmest possible cache, not unknown",
			&Ctx{CacheTTLMs: ttl, IdleMs: 0}, CachePhaseWarm},
		{"idle time with no TTL is unknown, not cold",
			&Ctx{IdleMs: ms(9 * time.Minute)}, CachePhaseUnknown},
		{"a backwards clock stays unknown rather than inventing zero idle",
			&Ctx{CacheTTLMs: ttl, IdleMs: -5_000}, CachePhaseUnknown},
		{"plenty of lifetime left is warm",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(30 * time.Second)}, CachePhaseWarm},
		{"just outside the window is still warm",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(3*time.Minute + 59*time.Second)}, CachePhaseWarm},
		{"exactly one window's width left is pre-expiry",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(4 * time.Minute)}, CachePhasePreExpiry},
		{"a few seconds left is pre-expiry",
			&Ctx{CacheTTLMs: ttl, IdleMs: ms(4*time.Minute + 55*time.Second)}, CachePhasePreExpiry},
		// AT NOMINAL EXPIRY THIS CLASSIFIER SAYS COLD, which is the right answer for the reader
		// that needs a LIVE entry: extract_llm_sweep's prefix ask must stand down as soon as the
		// entry may be gone. The COMPACTION gate needs the opposite caution and therefore a
		// different threshold — see TestTheCompactionDeadZoneAtNominalExpiry.
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
	for _, state := range []string{CacheStatePreExpiry} {
		if !(Trigger{CacheState: state}).CacheAllows(nil, unknown) {
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
		if !zero.CacheAllows(nil, p) {
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

// AT NOMINAL EXPIRY THE PHASE IS COLD, AND THE ONE SURVIVING RESTRICTIVE STATE DECLINES THERE.
//
// This used to assert a DEAD ZONE: a window one minute wide, just past nominal expiry, where the
// strict cold test said "not certainly gone" while CachePhase said Cold, so `cold` and
// `pre_expiry_or_cold` both declined. Those states are withdrawn and the strict test went with them,
// so the dead zone is no longer a thing that exists — but the case it was built from still matters,
// because `pre_expiry` must not fire here.
//
// CachePhase answers "might this entry be gone?", which is extract_llm_sweep's question and the
// reason nominal expiry counts as Cold. A component whose model call needs a LIVE prefix to reuse
// (cache_aware_summarizer's question) therefore gets a decline at nominal expiry rather than a hit
// on an entry that may already have lapsed — which is the conservative direction for that caller.
func TestPreExpiryDeclinesOnceTheEntryReachesNominalExpiry(t *testing.T) {
	const ttlMs = int64(5 * 60 * 1000)
	// Just past nominal expiry.
	c := &Ctx{CacheAware: true, MaxCachedIdx: -1, CacheTTLMs: ttlMs, IdleMs: ttlMs + 1_000}

	if got := c.CachePhase(DefaultPreExpiry); got != CachePhaseCold {
		t.Fatalf("precondition: CachePhase = %s, want %s — the rest of this test is vacuous",
			got, CachePhaseCold)
	}
	remaining, ok := c.CacheRemaining()
	if !ok {
		t.Fatal("precondition: the remaining lifetime must be known for this case to mean anything")
	}
	if remaining > 0 {
		t.Fatalf("precondition: %v of life left, so this is not the expiry case", remaining)
	}

	tr := Trigger{CacheState: CacheStatePreExpiry}
	if tr.CacheAllows(c, c.CachePhase(tr.PreExpiry())) {
		t.Error("cache_state pre_expiry permitted a turn at nominal expiry. That state exists for a " +
			"call that reuses the cached prefix, and an entry at expiry may already be gone — " +
			"firing here pays fresh prefill for the whole conversation with nothing to hit")
	}

	// And `any` fires here, because it imposes no cache constraint at all. This is the pair that
	// keeps the assertion above from passing on a Trigger that simply declines everything.
	if !(Trigger{CacheState: CacheStateAny}).CacheAllows(c, CachePhaseCold) {
		t.Error("cache_state any declined a Cold phase; it must impose no cache constraint")
	}
}

// UNKNOWN OVER A LIVE PREFIX MUST BE REFUSED, and this is the case every existing guard in this file
// missed: they all set MaxCachedIdx: -1, which is the one value that makes Unknown safe.
//
// CachePhaseUnknown means "the cache-aware path could not tell us how much life this entry has left".
// It does NOT mean there is no entry. This package's own comment used to assert the stronger claim —
// that on Unknown deployments MaxCachedIdx stays -1, so there is no live prefix whose invalidation we
// would be avoiding — and two reachable shapes falsify it:
//
//   - apply's legacy no-Tracker path, which every library consumer of BodyFull/BodyOpts takes, reads
//     MaxCachedIdx from the store and never sets CacheTTLMs at all;
//   - `cache_mode: on` against a provider whose TTL this repo does not derive.
//
// A REVIEW OBSERVED THE GATE OPENING over a live 8-message cached prefix on 13 of 26 turns. And the
// consequence is session-persistent rather than confined to the turn: once a checkpoint exists the
// replay runs before and independently of this gate, so one wrongly-permitted turn rewrites the
// forwarded prefix for the rest of the session, warm turns included.
func TestUnknownIsRefusedWhenAPrefixIsLive(t *testing.T) {
	// No readable TTL, so the phase is Unknown — but MaxCachedIdx says eight messages are already
	// committed to the provider's cache.
	live := &Ctx{CacheAware: true, MaxCachedIdx: 8}
	if got := live.CachePhase(DefaultPreExpiry); got != CachePhaseUnknown {
		t.Fatalf("precondition: phase = %s, want %s — this case is only interesting on Unknown",
			got, CachePhaseUnknown)
	}
	for _, state := range []string{CacheStatePreExpiry} {
		tr := Trigger{CacheState: state}
		if tr.CacheAllows(live, CachePhaseUnknown) {
			t.Errorf("cache_state %q permitted compaction on an UNKNOWN phase with max_cached_idx=8. "+
				"There is a live prefix here and no evidence about its lifetime, so this rewrites "+
				"a cached prefix and pays a cache-write of the whole suffix", state)
		}
	}

	// THE COMPONENT MUST STILL WORK where Unknown genuinely means no prefix, or the guard has traded
	// one silent failure for another: declining everywhere would make summarize dead on every
	// deployment whose cache-aware path does not run.
	noPrefix := &Ctx{CacheAware: false, MaxCachedIdx: -1}
	for _, state := range []string{CacheStatePreExpiry} {
		tr := Trigger{CacheState: state}
		if !tr.CacheAllows(noPrefix, CachePhaseUnknown) {
			t.Errorf("cache_state %q refused an UNKNOWN phase with no cached prefix; there is "+
				"nothing to protect there and refusing disables the component on every "+
				"non-cache-aware deployment", state)
		}
	}

	// And a caller that never asked for a cache state is unaffected in both shapes: `""` and `any`
	// are the components that do not consult this gate at all.
	for _, state := range []string{"", CacheStateAny} {
		tr := Trigger{CacheState: state}
		if !tr.CacheAllows(live, CachePhaseUnknown) {
			t.Errorf("cache_state %q must impose no cache constraint, but it declined", state)
		}
	}
}

// The same defect through the DEPLOYED path's own arithmetic rather than through a hand-built Ctx:
// two turns of one session in the same millisecond. This is the shape a review reached live with four
// concurrent requests, and it is now Warm rather than Unknown — so the gate refuses it on the phase
// itself, before the MaxCachedIdx guard is even consulted.
func TestConcurrentTurnsAreWarmNotUnknown(t *testing.T) {
	const ttlMs = int64(5 * 60 * 1000)
	// idle == 0 is what apply now records when nowMs == prevAt.
	c := &Ctx{CacheAware: true, MaxCachedIdx: 8, CacheTTLMs: ttlMs, IdleMs: 0}
	if got := c.CachePhase(DefaultPreExpiry); got != CachePhaseWarm {
		t.Fatalf("phase = %s, want %s: a turn arriving in the same millisecond as the previous one "+
			"has the entry's whole lifetime ahead of it, which is the WARMEST state there is — "+
			"reading it as Unknown is what let the compaction gate open over a live prefix", got, CachePhaseWarm)
	}
	tr := Trigger{CacheState: CacheStatePreExpiry}
	if tr.CacheAllows(c, c.CachePhase(tr.PreExpiry())) {
		t.Error("cache_state pre_expiry permitted compaction on a prefix cached 0 ms ago")
	}
}
