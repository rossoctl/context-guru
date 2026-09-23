package offload

import (
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE SHIPPED DEFAULT COMPACTS AT 0.9 ON A LIVE CACHE, and this is the assertion that says so.
//
// It is the reverse of what this file asserted one release ago. The default was
// `pre_expiry_or_cold`, and the test here pinned that a warm turn at 0.9 DECLINED with
// `cache_state_declined_warm`. Three things retired that position, all recorded on
// summarizeDefaultCacheState: the corpus cost of firing warm and being wrong was −$0.84 in total,
// firing warm pays back in 2-3 turns, and — the structural half — a cold-gated component pays the
// FIRST full rewrite anyway, because the turn that observes a cold cache commissions the summary and
// forwards the transcript untouched.
//
// So the pair below is the same shape as before with the rows swapped: the default fires, and the one
// surviving restrictive state declines the same turn. Keeping it as a PAIR is what pins the
// difference to `cache_state` and nothing else — a test that only showed the default firing would
// still pass with the cache gate deleted outright, and one that only showed `pre_expiry` declining
// would still pass if the default had been broken into never firing.
func TestTheDefaultFiresOnALiveCacheAndPreExpiryDoesNot(t *testing.T) {
	msgs := sumTranscript(6)
	const window = 1_000_000

	assertBilledFigureOpenedTheFill(t, msgs, window)

	for _, tc := range []struct {
		name       string
		cacheState string
		wantFires  bool
		wantGate   string
	}{
		// No cache_state written at all, so this exercises the SHIPPED default through the
		// registered constructor rather than a string a test picked.
		{"the shipped default fires on a live cache", "", true, ""},
		{"cache_state: pre_expiry declines the same turn", "pre_expiry", false, "cache_state_declined_warm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheGatedSummarizeWithFrac(t, anyGateTrigger(tc.cacheState))
			// min_request_frac is deliberately absent above: the 0.9 default is part of what is
			// under test, because it is what the two configurations SHARE.
			if s.trigger.MinRequestFrac != summarizeDefaultRequestFrac {
				t.Fatalf("precondition: min_request_frac is %v, want the shipped %v — the two "+
					"configurations must differ only in cache_state",
					s.trigger.MinRequestFrac, summarizeDefaultRequestFrac)
			}
			model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
			s.modelClient = model

			ctx := sumCtx(store.NewMemory(store.Options{MaxEntries: 400}), "warm", window, true, 950_000)
			ctx.Session = "warm-" + tc.name // see anyGateTrigger's note on the single-flight registry
			// Assert the fixture really is warm. If sumCtx's TTL arithmetic or DefaultPreExpiry
			// moved so that 270s of remaining life read as pre-expiry, BOTH rows would fire and this
			// test would report an agreement it never demonstrated.
			if phase := ctx.CachePhase(components.DefaultPreExpiry); phase != components.CachePhaseWarm {
				t.Fatalf("precondition: fixture phase is %s, want Warm", phase)
			}

			in := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
			var rep components.Report
			if _, err := s.Offload(in, &rep, ctx); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			// Commissioned, not called inline, so drain before counting: a declined turn started
			// nothing and this returns at once, a fired turn started exactly one call.
			WaitForSummaryForTest(ctx.Session, 5*time.Second)
			if fired := atomic.LoadInt64(&model.calls) == 1; fired != tc.wantFires {
				t.Errorf("fired=%v want %v (gates: %v). The two configurations differ in one key, so "+
					"one of them has stopped doing what its documentation promises",
					fired, tc.wantFires, rep.Gates)
			}
			// NAMING THE GATE IS WHAT MAKES THE DECLINING ROW MEAN ANYTHING. Asserting only that
			// pre_expiry did not fire would pass on any decline at all — window_not_exact,
			// below_request_trigger, a gate nobody has written yet — while the row's whole purpose
			// is that the CACHE PHASE is what declined it.
			if tc.wantGate != "" && rep.Gates[tc.wantGate] == 0 {
				t.Errorf("gate %q not filed; gates: %v. pre_expiry declined for some other reason, "+
					"so this row is not exercising the cache gate", tc.wantGate, rep.Gates)
			}
		})
	}
}

// AND THE DEFAULT MUST SURVIVE THE UNKNOWN-PHASE GUARD OVER A LIVE PREFIX.
//
// CacheAllows declines an UNKNOWN phase whenever a cached prefix demonstrably exists (CacheAware with
// MaxCachedIdx >= 0) — the state a review found the gate opening over a live 8-message prefix on 13
// of 26 turns. That guard is scoped to exclude `""` and `any`, because those callers never asked for
// a cache constraint and declining there would make the component DEAD on the deployments that
// present a cached prefix and no readable TTL: apply's legacy no-Tracker path, and `cache_mode: on`
// against a provider whose TTL this repo does not derive.
//
// That scoping used to protect an opt-out. It now protects the DEFAULT, which makes it considerably
// more load-bearing than when it was written: drop `t.CacheState != CacheStateAny` from that
// condition and summarize stops firing on those deployments entirely, with no other test failing.
func TestTheDefaultStillFiresWhenAnUnknownPhaseHidesALivePrefix(t *testing.T) {
	msgs := sumTranscript(6)
	const window = 1_000_000

	// Not inherited from the test above even though both use sumTranscript(6): this test's own
	// declining row asserts a SPECIFIC gate, which is only meaningful if the fill conjunct is open.
	assertBilledFigureOpenedTheFill(t, msgs, window)

	for _, tc := range []struct {
		name       string
		cacheState string
		wantFires  bool
		wantGate   string
	}{
		{"the default fires despite the live-prefix guard", "", true, ""},
		{"pre_expiry is declined by it", "pre_expiry", false, "cache_state_declined_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheGatedSummarizeWithFrac(t, anyGateTrigger(tc.cacheState))
			model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
			s.modelClient = model

			// An unknown phase — no readable TTL — over a prefix that provably exists.
			ctx := sumCtx(store.NewMemory(store.Options{MaxEntries: 400}), "unknown", window, true, 950_000)
			ctx.Session = "unknown-" + tc.name // see anyGateTrigger's note on the single-flight registry
			ctx.CacheAware, ctx.MaxCachedIdx = true, 8
			if phase := ctx.CachePhase(components.DefaultPreExpiry); phase != components.CachePhaseUnknown {
				t.Fatalf("precondition: fixture phase is %s, want Unknown", phase)
			}

			in := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
			var rep components.Report
			if _, err := s.Offload(in, &rep, ctx); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			WaitForSummaryForTest(ctx.Session, 5*time.Second)
			if fired := atomic.LoadInt64(&model.calls) == 1; fired != tc.wantFires {
				t.Errorf("fired=%v want %v (gates: %v). The default must impose no cache constraint "+
					"even where the phase is unreadable and a prefix is known to be live",
					fired, tc.wantFires, rep.Gates)
			}
			if tc.wantGate != "" && rep.Gates[tc.wantGate] == 0 {
				t.Errorf("gate %q not filed; gates: %v", tc.wantGate, rep.Gates)
			}
		})
	}
}

// anyGateTrigger writes the trigger block for one of the two configurations, omitting cache_state
// entirely for the default so the constructor supplies it. min_request_frac is left out of both,
// because the two configurations must differ in exactly one key.
//
// EACH SUBTEST ALSO GETS ITS OWN SESSION ID, which sumCtx does not give it. The async summarizer
// keys in-flight calls on the session in a package-global single-flight registry, so subtests
// sharing sumCtx's "s" are only safe while they run sequentially — a later t.Parallel() here would
// have one subtest's commissioned call satisfy another's wait, and the firing counts would silently
// stop belonging to the turns that produced them.
func anyGateTrigger(cacheState string) string {
	trigger := "trigger:\n  min_messages: 2\n"
	if cacheState != "" {
		trigger += "  cache_state: " + cacheState + "\n"
	}
	return trigger
}

// assertBilledFigureOpenedTheFill pins that the fill conjunct is open because of the PROVIDER's
// figure rather than our own count of the transcript.
//
// Both tests need it, and for the same reason: the fill gate is the half the two configurations
// SHARE, so if it were closed — or if it were opened by MessagesTokens, which runs a median 3.38x
// below billed input — then neither a fired turn nor a named gate would be attributable to
// cache_state, and both tests would be measuring the wrong conjunct.
func assertBilledFigureOpenedTheFill(t *testing.T, msgs []bschemas.ChatMessage, window int) {
	t.Helper()
	tokens := schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs})
	if float64(tokens) >= summarizeDefaultRequestFrac*float64(window) {
		t.Fatalf("fixture counts %d message-text tokens, which already clears the %.0f floor; these "+
			"tests can no longer show that the billed figure is what opened the fill gate",
			tokens, summarizeDefaultRequestFrac*float64(window))
	}
}
