package offload

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// caGatedCtx builds a Ctx sitting in a named cache phase — the cache_aware_summarizer counterpart
// of summarize_cachegate_test.go's sumCtx, built the same way and for the same reason: the raw
// Ctx fields a real request carries (the TTL the body asked for, this session's idle time), not
// a components.CachePhase value constructed directly, so the classification under test is the
// one apply's own numbers would produce.
func caGatedCtx(session string, st store.Store, phase string, window int, exact bool, billed int) *components.Ctx {
	c := &components.Ctx{
		Ctx: context.Background(), Session: session, Store: st, MaxCachedIdx: -1,
		CtxWindow: window, CtxWindowExact: exact, PrevBilledInput: billed,
	}
	const ttl = 5 * 60 * 1000 // a bare `ephemeral` mark: five minutes
	switch phase {
	case "pre_expiry":
		c.CacheTTLMs, c.IdleMs = ttl, ttl-30*1000 // 30s left, inside the one-minute window
	case "warm":
		c.CacheTTLMs, c.IdleMs = ttl, 30*1000 // 4.5 minutes left
	case "cold":
		c.CacheTTLMs, c.IdleMs, c.ColdCache = ttl, ttl+60*1000, true
	case "unknown":
		// Both zero: the cache-aware path did not run.
	default:
		panic("unknown phase " + phase)
	}
	return c
}

// caTranscript is [user goal, then n tool exchanges] — big enough to clear min_messages/min_tokens
// on its own so a test can isolate the fraction/cache-state conjuncts.
func caTranscript(n int) []bschemas.ChatMessage {
	msgs := []bschemas.ChatMessage{caMsg(bschemas.ChatMessageRoleUser, "fix the failing tests")}
	for i := 1; i <= n; i++ {
		msgs = append(msgs, caToolPair("working", "bash", `{"c":"x"}`, "t"+strconv.Itoa(i),
			strings.Repeat("line of tool output\n", 30))...)
	}
	return msgs
}

// THE DEFAULTS HAVE TO BE SAYABLE, same requirement summarize's own
// TestSummarizeTriggerDefaultsAndTheirOptOut pins — see that test for the full reasoning this one
// borrows. cache_aware_summarizer shares the probe (triggerKeysPresent) and the two defaulted
// constants (cacheAwareDefaultRequestFrac/cacheAwareDefaultCacheState), so this test is the half
// of that reasoning specific to THIS component: a zero Trigger fires on ~11 messages and 500
// tokens (components/trigger.go), which this component had no defense against until now.
func TestCacheAwareTriggerDefaultsAndTheirOptOut(t *testing.T) {
	t.Run("an absent key takes the shipped default", func(t *testing.T) {
		s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\n")
		if s.trigger.CacheState != components.CacheStateAny {
			t.Errorf("cache_state defaulted to %q, want %q", s.trigger.CacheState, components.CacheStateAny)
		}
		if s.trigger.MinRequestFrac != cacheAwareDefaultRequestFrac {
			t.Errorf("min_request_frac defaulted to %v, want %v", s.trigger.MinRequestFrac, cacheAwareDefaultRequestFrac)
		}
	})
	t.Run("an explicit zero fraction is honoured, not overwritten by the default", func(t *testing.T) {
		s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ntrigger:\n  min_request_frac: 0\n")
		if s.trigger.MinRequestFrac != 0 {
			t.Errorf("an explicitly written min_request_frac: 0 became %v — the presence probe is "+
				"not shared correctly with summarize's", s.trigger.MinRequestFrac)
		}
	})
	t.Run("explicit cache_state wins over the default", func(t *testing.T) {
		s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ntrigger:\n  cache_state: pre_expiry\n")
		if s.trigger.CacheState != components.CacheStatePreExpiry {
			t.Errorf("cache_state = %q, want pre_expiry", s.trigger.CacheState)
		}
	})
	t.Run("a withdrawn cache_state is refused with the replacement named", func(t *testing.T) {
		for _, state := range []string{"cold", "pre_expiry_or_cold"} {
			_, err := newCacheAwareSummarizer([]byte("trigger:\n  cache_state: " + state + "\n"))
			if err == nil {
				t.Fatalf("%s was accepted; it has no behaviour any more", state)
			}
			if !strings.Contains(err.Error(), "any") || !strings.Contains(err.Error(), "pre_expiry") {
				t.Errorf("%s: the error names no replacement: %v", state, err)
			}
		}
	})
	t.Run("the form advertises the defaults the constructor applies", func(t *testing.T) {
		want := map[string]any{
			"trigger.cache_state":      cacheAwareDefaultCacheState,
			"trigger.min_request_frac": cacheAwareDefaultRequestFrac,
		}
		seen := map[string]bool{}
		for _, f := range components.Fields("cache_aware_summarizer") {
			if w, ok := want[f.Key]; ok {
				seen[f.Key] = true
				if f.Default != w {
					t.Errorf("%s: form default %v, constructor applies %v", f.Key, f.Default, w)
				}
			}
		}
		for k := range want {
			if !seen[k] {
				t.Errorf("%s is not declared for cache_aware_summarizer at all", k)
			}
		}
	})
}

// THE BUG CLASS THIS SHARES WITH summarize: the gate must decide whether to PAY for a fresh
// summary, never whether an already-paid-for checkpoint stays in the forwarded body. A turn the
// cache-state gate declines must still replay its existing checkpoint, or it forwards the FULL
// transcript — bytes diverging from the cached prefix at the first summarized message, forcing
// the exact 1.25x suffix rewrite this component exists to avoid.
// ⭐ THE RESERVE DESIGN'S CENTRAL PROPERTY: a checkpoint commissioned under cache_state:
// pre_expiry while the cache was still believed live (phase PreExpiry) must NOT be spliced into
// a WARM turn — that is exactly the cache-destructive rewrite this mode exists to defer. This
// replaces an earlier version of this test that asserted the OPPOSITE (replay unconditionally on
// any cache-state decline) — correct for the bug class it was originally guarding against
// (summarize-style conflation of "pay for a fresh one" with "keep an old one live"), but pre_expiry
// no longer means "keep whatever exists live"; it means "hold this in reserve for a cold return".
func TestCacheAwarePreExpiryReserveDoesNotSpliceOnAWarmTurn(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\nresummarize_tokens: 50\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	model := &capturingModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-reserve-warm", st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	cp, ok := loadCheckpoint(ctx)
	if !ok {
		t.Fatalf("turn 1 commissioned no checkpoint (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	if !cp.Reserved {
		t.Fatal("a checkpoint commissioned at phase PreExpiry under cache_state: pre_expiry must " +
			"be marked Reserved")
	}
	if model.calls != 1 {
		t.Fatalf("turn 1 made %d model calls, want 1 (gates: %v)", model.calls, rep.Gates)
	}

	// Turn 2: warm. The reserve exists and matches, but must not be spliced.
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, caGatedCtx("ca-reserve-warm", st, "warm", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("turn 2 was not gated by cache state (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	if len(turn2.Input) != len(msgs) {
		t.Errorf("a warm turn spliced the reserve (%d -> %d messages) instead of forwarding the "+
			"full history untouched — splicing it now is exactly the cache-destructive rewrite "+
			"pre_expiry's reserve design exists to defer (gates: %v)", len(msgs), len(turn2.Input), rep.Gates)
	}
	if model.calls != 1 {
		t.Errorf("the warm turn made a model call (%d total): a reserved checkpoint must not be "+
			"refreshed on an ordinary warm turn", model.calls)
	}
	// Still reserved — nothing about a warm turn may change that.
	if cp, ok := loadCheckpoint(ctx); !ok || !cp.Reserved {
		t.Error("the reserve was consumed or lost on a warm turn that must not touch it")
	}
}

// The reserve graduates — splices, tail and all, and stops being Reserved — on a turn whose
// phase is Cold. From there the session behaves exactly like cache_state: any for this one
// checkpoint.
func TestCacheAwarePreExpiryReserveSplicesOnAColdTurn(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\nresummarize_tokens: 50\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	model := &capturingModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-reserve-cold", st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	cp, ok := loadCheckpoint(ctx)
	if !ok || !cp.Reserved {
		t.Fatalf("precondition: want a reserved checkpoint (gates: %v)", rep.Gates)
	}
	summaryText := cp.SummaryMsg

	// Turn 2: cold. The reserve must graduate — splice now, however stale its tail might be.
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, caGatedCtx("ca-reserve-cold", st, "cold", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if len(turn2.Input) == len(msgs) {
		t.Errorf("a cold turn did not splice the reserve (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	if got := schema.MessageText(turn2.Input[1]); got != summaryText {
		t.Errorf("the spliced summary differs from the reserve:\n got %q\nwant %q", got, summaryText)
	}
	if model.calls != 1 {
		t.Errorf("the cold turn made a model call (%d total): graduating a reserve must not pay "+
			"for a fresh one at the exact turn that is already paying full freight for everything "+
			"else", model.calls)
	}
	if cp, ok := loadCheckpoint(ctx); !ok || cp.Reserved {
		t.Error("the checkpoint is still marked Reserved after graduating — later warm turns would " +
			"wrongly keep forwarding full history for a checkpoint that is now live")
	}

	// Turn 3: warm again, same checkpoint. Now behaves exactly like `any` — replayed while
	// current.
	turn3 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	rep = components.Report{}
	if _, err := s.Offload(turn3, &rep, caGatedCtx("ca-reserve-cold", st, "warm", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if len(turn3.Input) == len(msgs) {
		t.Error("turn 3 forwarded full history for an already-graduated checkpoint on a warm turn")
	}
	if model.calls != 1 {
		t.Errorf("turn 3 made a model call (%d total): an already-graduated, current checkpoint "+
			"must simply be replayed", model.calls)
	}
}

// The fill conjunct — a turn below the fraction must decline even though min_messages/min_tokens
// pass easily, and it must say which gate declined it.
func TestCacheAwareFillConjunctDeclinesBelowTheFraction(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  cache_state: any\n") // frac left at the shipped 0.9 default
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})
	msgs := caTranscript(3)

	cases := []struct {
		name      string
		window    int
		exact     bool
		billed    int
		wantFires bool
		wantGate  string
	}{
		{"billed past the fraction fires", 1_000_000, true, 950_000, true, ""},
		{"billed well short of it declines", 1_000_000, true, 100_000, false, "below_request_trigger"},
		{"a guessed window declines even though non-zero", 1_000_000, false, 950_000, false, "window_not_exact"},
		{"an unknown window declines", 0, false, 950_000, false, "window_not_exact"},
		{"no previous billed figure declines", 1_000_000, true, 0, false, "window_not_exact"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
			ctx := caGatedCtx("ca-fill-"+strconv.Itoa(i), st, "unknown", tc.window, tc.exact, tc.billed)
			var rep components.Report
			if _, err := s.Offload(req, &rep, ctx); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			WaitForSummaryForTest(ctx.Session, 2*time.Second)
			_, ok := loadCheckpoint(ctx)
			if ok != tc.wantFires {
				t.Errorf("fired=%v want %v (gates: %v)", ok, tc.wantFires, rep.Gates)
			}
			if tc.wantGate != "" && rep.Gates[tc.wantGate] == 0 {
				t.Errorf("a turn that declined must say why: want gate %q, got %v", tc.wantGate, rep.Gates)
			}
		})
	}
}

// ⭐ COLD-TURN ORDERING, the property #400 exists for: a turn whose OWN cache is cold must not
// fire the side call concurrently with the forwarded request — it must defer until
// ResolveDeferredCacheAwareSummary releases it, so the side call always reads a prefix the
// forwarded request has already rewritten rather than racing it to rewrite the same one twice.
func TestCacheAwareDefersTheSideCallOnACacheState_Cold(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	model := &capturingModel{out: "<summary>explored.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-cold-order", st, "cold", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Events["cold_commission_deferred"] == 0 {
		t.Fatalf("a cold-cache turn did not defer (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	// The call must NOT have been dispatched yet — give it a moment to prove a negative.
	time.Sleep(100 * time.Millisecond)
	if model.calls != 0 {
		t.Fatalf("the side call fired on a cold turn WITHOUT waiting for the forwarded request to "+
			"rewrite the cache first — this is the exact double-rewrite #400 exists to prevent "+
			"(calls=%d)", model.calls)
	}
	// Now the proxy's hook fires, as it would right after the forwarded request's own usage is
	// observed.
	ResolveDeferredCacheAwareSummary(ctx.Session)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the deferred summary never resolved")
	}
	if model.calls != 1 {
		t.Errorf("model called %d times after resolution, want exactly 1", model.calls)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Error("resolving the deferral produced no checkpoint")
	}
}

// The fallback timer is the safety net for a host that never calls the resolve hook. It must
// still fire — losing the compaction forever is worse than firing late.
func TestCacheAwareColdDeferralFallbackFiresWithoutTheHook(t *testing.T) {
	orig := coldDeferralFallback
	coldDeferralFallback = 50 * time.Millisecond
	defer func() { coldDeferralFallback = orig }()

	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: any\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-cold-fallback", st, "cold", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// The fallback timer has not fired yet at this instant (it was just scheduled), so there is
	// nothing in flight for WaitForSummaryForTest to find — it would return immediately and
	// report success vacuously. Sleep past the (shrunk) fallback window first, THEN drain
	// whatever the timer started.
	time.Sleep(200 * time.Millisecond)
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the fallback never fired the deferred summary")
	}
	if model.calls != 1 {
		t.Errorf("model called %d times via the fallback, want exactly 1", model.calls)
	}
	if _, _, fallbackFired, _ := CacheAwareColdDeferralStats(); fallbackFired == 0 {
		t.Error("the fallback counter did not increment")
	}
}

// ⭐ KEEP-ALIVE SUBSTITUTION, the property its own tests in proxy exercise end to end — this one
// pins the offload-package half: a turn with real commission material always registers it
// (regardless of whether cache_state let it fire this turn), KeepAliveSubstitute offers it until
// a checkpoint exists, and a real request's arrival clears a stale registration.
func TestCacheAwareRegistersAKeepAliveCandidateEvenWhenCacheStateDeclines(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	// Warm: cache_state: pre_expiry declines to fire, but the fill conjunct passed.
	ctx := caGatedCtx("ca-ka-candidate", st, "warm", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(req, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("this turn must have been declined by cache_state for the test to mean anything "+
			"(gates: %v)", rep.Gates)
	}
	dispatch, _, _, ok := KeepAliveSubstitute(ctx.Session)
	if !ok {
		t.Fatal("no keep-alive candidate was registered for a turn with real commission material")
	}
	res := dispatch(5 * time.Second)
	if !res.Committed {
		t.Fatal("the substitute dispatch did not commit a summary")
	}
	if model.calls != 1 {
		t.Errorf("model called %d times, want 1", model.calls)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Error("the substitute dispatch committed nothing to the checkpoint")
	}

	// Once a checkpoint exists, the candidate must no longer be offered: the next real turn will
	// splice the checkpoint for free, so spending another call here would be pure waste.
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); ok {
		t.Error("a candidate was still offered after a checkpoint was written")
	}
}

// ClearKeepAliveCandidate (the unconditional form) drops a registration outright — used by
// tests for teardown, never by the real request path (see the two tests below for that).
func TestClearKeepAliveCandidateDropsAStaleRegistration(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caTranscript(3)...)}
	ctx := caGatedCtx("ca-ka-clear", st, "warm", 0, false, 0)
	var rep components.Report
	s.Offload(req, &rep, ctx)
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); !ok {
		t.Fatal("precondition: no candidate was registered")
	}
	ClearKeepAliveCandidate(ctx.Session)
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); ok {
		t.Error("ClearKeepAliveCandidate did not drop the registration")
	}
}

// ⭐ THE REAL REQUEST PATH'S OWN CLEAR: ClearStaleKeepAliveCandidate — what proxy.go actually
// calls, with a timestamp captured BEFORE the pipeline ran — must NOT drop the candidate THIS
// SAME call just registered. An unconditional clear placed after the pipeline (the bug this
// function replaces) deleted that candidate in the same request that wrote it, which is why
// keep-alive substitution could never fire in practice: every real request cleared its own
// registration a few lines after making it.
func TestClearStaleKeepAliveCandidateKeepsThisRequestsOwnRegistration(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	ctx := caGatedCtx("ca-ka-clear-fresh", st, "warm", 0, false, 0)

	// Mirrors proxy.go's own call order: capture the instant BEFORE the pipeline runs, run the
	// pipeline (which registers the candidate), THEN clear using that captured instant.
	requestStartedAt := time.Now()
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caTranscript(3)...)}
	var rep components.Report
	s.Offload(req, &rep, ctx)
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); !ok {
		t.Fatal("precondition: no candidate was registered")
	}
	ClearStaleKeepAliveCandidate(ctx.Session, requestStartedAt)
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); !ok {
		t.Error("ClearStaleKeepAliveCandidate dropped the candidate THIS request's own pipeline " +
			"run just registered — this is the bug that made keep-alive substitution impossible " +
			"on every real request")
	}
}

// The other half: a candidate registered by an EARLIER request (registeredAt before this
// request's own requestStartedAt) must still be dropped — a superseded span must not survive
// into a keep-alive ping once a newer turn has moved the conversation on.
func TestClearStaleKeepAliveCandidateDropsAnEarlierRequestsRegistration(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	ctx := caGatedCtx("ca-ka-clear-stale", st, "warm", 0, false, 0)

	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caTranscript(3)...)}
	var rep components.Report
	s.Offload(req, &rep, ctx) // an EARLIER request's pipeline run
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); !ok {
		t.Fatal("precondition: no candidate was registered")
	}

	// A LATER request's own requestStartedAt, captured strictly after the registration above.
	requestStartedAt := time.Now()
	ClearStaleKeepAliveCandidate(ctx.Session, requestStartedAt)
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); ok {
		t.Error("ClearStaleKeepAliveCandidate kept a candidate registered by an earlier request")
	}
}

// A turn too small to be a candidate at all must not register one — there is nothing worth
// substituting for an idle span that was never going to be summarized anyway.
func TestCacheAwareDoesNotRegisterAKeepAliveCandidateWhenNotBigEnough(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  cache_state: any\n") // frac left at the shipped 0.9 default
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caTranscript(3)...)}
	// A real but tiny billed figure: FracResolvable passes, Fires does not.
	ctx := caGatedCtx("ca-ka-toosmall", st, "unknown", 1_000_000, true, 100)
	var rep components.Report
	s.Offload(req, &rep, ctx)
	if rep.Gates["below_request_trigger"] == 0 {
		t.Fatalf("precondition: want below_request_trigger (gates: %v)", rep.Gates)
	}
	if _, _, _, ok := KeepAliveSubstitute(ctx.Session); ok {
		t.Error("a candidate was registered for a turn that was never big enough to summarize")
	}
}

// usageRecordingModel is capturingModel plus a real write into the ambient cheapmodel sink via
// cheapmodel.ReplayUsage — capturingModel itself never touches it (it is a pure test double with
// no real HTTP call), so without this a test asserting on deferred-usage/dashboard plumbing would
// pass vacuously: deferUsage's own guard (`calls == 0`) silently skips persisting anything when
// the sink recorded nothing, which is exactly the state every OTHER test using capturingModel
// leaves it in.
type usageRecordingModel struct {
	capturingModel
}

func (m *usageRecordingModel) CompleteMessages(ctx context.Context, system string, msgs []bschemas.ChatMessage) (string, error) {
	out, err := m.capturingModel.CompleteMessages(ctx, system, msgs)
	cheapmodel.ReplayUsage(ctx, "fake-model", 100, 50, 10, 20)
	return out, err
}

// ⭐ THE SUMMARY CALL'S OWN USAGE MUST REACH A DASHBOARD. It was otherwise invisible: it never
// appears on the triggering turn's Report (the call hasn't happened yet) nor the splicing turn's
// own synchronous work (the call ran on a different goroutine). takeDeferredUsage is the one
// place it can surface, since every turn calls it unconditionally before anything else.
func TestCacheAwareCommissionUsageReachesTheSplicingTurnsReport(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = &usageRecordingModel{capturingModel{out: "<summary>explored the handler.</summary>"}}

	in := caFixture()
	c := caCtx("ca-dashboard-row")
	t1, _ := caTurn(t, s, c, in)
	if len(t1.Input) != len(in) {
		t.Fatalf("turn 1 must forward untouched")
	}
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed")
	}
	_, rep2 := caTurn(t, s, c, in)
	if len(rep2.Calls) != 1 {
		t.Fatalf("turn 2's Report carries %d ModelCall rows, want exactly 1 (Calls: %+v)", len(rep2.Calls), rep2.Calls)
	}
	got := rep2.Calls[0]
	if got.Component != "cache_aware_summarizer" {
		t.Errorf("Component = %q, want cache_aware_summarizer", got.Component)
	}
	if got.Strategy != "messages/turn" {
		t.Errorf("Strategy = %q, want %q (path/trigger)", got.Strategy, "messages/turn")
	}
	if got.PromptTokens == 0 && got.CompletionTokens == 0 && got.CacheRead == 0 && got.CacheWrite == 0 {
		t.Error("the dashboard row carries no usage at all")
	}
	if !got.Accepted {
		t.Error("a landed summary's row should read Accepted")
	}
}

// ⭐ A REPLY CUT OFF MID-GENERATION MUST NOT BECOME A CHECKPOINT. ensureSummaryTags/
// sanitizeSummary would otherwise manufacture a complete-looking <summary>...</summary> wrapper
// around a truncated reply — sanitizeSummary strips any closing tag as an untrusted control
// string, and ensureSummaryTags then unconditionally adds one back, so by the time either has
// run a truncated reply and a complete one are byte-indistinguishable. The check has to happen
// on the RAW text, before either of them.
func TestCacheAwareRejectsATruncatedSummaryOnTheAsyncPath(t *testing.T) {
	before := CacheAwareSummarizerTruncated()
	_, committedBefore, _, _ := CacheAwareAsyncStats()
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	// Missing the closing tag entirely — a reply cut off before the model could finish.
	s.modelClient = &capturingModel{out: "<summary>explored the handler, got cut off mid"}

	in := caFixture()
	c := caCtx("ca-truncated")
	t1, _ := caTurn(t, s, c, in)
	if len(t1.Input) != len(in) {
		t.Fatalf("turn 1 must forward untouched")
	}
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the detached call never resolved")
	}
	if CacheAwareSummarizerTruncated() != before+1 {
		t.Error("a truncated reply was not counted")
	}
	if _, committed, _, _ := CacheAwareAsyncStats(); committed != committedBefore {
		t.Error("a truncated reply committed a checkpoint")
	}
	if _, ok := loadCheckpoint(c); ok {
		t.Error("a checkpoint exists despite the reply being truncated")
	}
	// And the next turn must forward untouched rather than splice a fragment.
	t2, _ := caTurn(t, s, c, in)
	if len(t2.Input) != len(in) {
		t.Errorf("turn 2 spliced after a truncated reply: %d -> %d", len(in), len(t2.Input))
	}
	// Turn 2 ALSO sees no checkpoint yet, so it recommissions and will truncate again — drain it
	// before this test returns, or it resolves later and leaks a counter bump into a different
	// test reading the same process-wide counter.
	WaitForSummaryForTest(c.Session, 5*time.Second)
}

// Same property on the keep-alive substitute path (commissionSync), which has its own copy of
// the check because it is the one caller that cannot use the detached goroutine.
func TestCacheAwareRejectsATruncatedSummaryOnTheKeepAliveSubstitutePath(t *testing.T) {
	before := CacheAwareSummarizerTruncated()
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	s.modelClient = &capturingModel{out: "<summary>cut off before the closing tag"}
	ctx := caGatedCtx("ca-truncated-ka", store.NewMemory(store.Options{MaxEntries: 400}), "warm", 0, false, 0)

	t1, rep := caTurn(t, s, ctx, caTranscript(3))
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("precondition: want cache_state_declined_warm (gates: %v)", rep.Gates)
	}
	_ = t1
	dispatch, _, _, ok := KeepAliveSubstitute(ctx.Session)
	if !ok {
		t.Fatal("no keep-alive candidate was registered")
	}
	res := dispatch(5 * time.Second)
	if res.Committed {
		t.Fatal("a truncated reply committed a checkpoint via the keep-alive substitute")
	}
	if CacheAwareSummarizerTruncated() != before+1 {
		t.Error("a truncated reply was not counted on the keep-alive substitute path")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Error("a checkpoint exists despite the reply being truncated")
	}
}

// A keep-alive ping for a session already holding a CURRENT pre_expiry reserve must stay a plain
// ping: KeepAliveSubstitute refuses with KeepAliveReasonCheckpointExists, exactly as it already
// does for `any` whenever a checkpoint exists — there is nothing to gain from paying for a
// summary identical to the one already held.
func TestKeepAliveSubstituteRefusesACurrentReserve(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\nresummarize_tokens: 50\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	model := &capturingModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-ping-current", st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	if cp, ok := loadCheckpoint(ctx); !ok || !cp.Reserved {
		t.Fatalf("precondition: want a reserved checkpoint (gates: %v)", rep.Gates)
	}
	if model.calls != 1 {
		t.Fatalf("turn 1 made %d model calls, want 1", model.calls)
	}

	// The reserve was just commissioned from exactly this span, with no tail yet — it must read
	// as current, not stale.
	_, _, reason, ok := KeepAliveSubstitute(ctx.Session)
	if ok {
		t.Fatal("KeepAliveSubstitute offered a dispatch for a session whose reserve is current")
	}
	if reason != KeepAliveReasonCheckpointExists {
		t.Errorf("reason = %q, want %q", reason, KeepAliveReasonCheckpointExists)
	}
	if model.calls != 1 {
		t.Errorf("the lookup alone made %d model calls, want 1 (unchanged)", model.calls)
	}
}

// Once a reserve's own tail (the material registered by LATER turns, since the keeper's
// candidate is replaced wholesale on every turn) reaches resummarize_tokens, a ping must stop
// being a plain ping and instead become a fresh summary call that REFRESHES the reserve to
// cover the larger span — never left to go stale indefinitely just because something is already
// stored under this session's checkpoint key.
func TestKeepAliveSubstituteRefreshesAStaleReserve(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 1\nmin_tokens: 10\nresummarize_tokens: 50\ninstruction_role: user\n"+
		"trigger:\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	model := &capturingModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := caTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := caGatedCtx("ca-ping-stale", st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	cp1, ok := loadCheckpoint(ctx)
	if !ok || !cp1.Reserved {
		t.Fatalf("precondition: want a reserved checkpoint (gates: %v)", rep.Gates)
	}

	// Turn 2: warm (declines on cache state, as the reserve-does-not-splice-while-warm test also
	// exercises), but with a MUCH bigger transcript — registration happens regardless of the gate
	// (cache_aware_summarizer.go's own comment at the registerKeepAliveCandidate call site), so
	// the keep-alive candidate's span grows even though no new checkpoint is written here.
	grown := caTranscript(9)
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), grown...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, caGatedCtx("ca-ping-stale", st, "warm", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("turn 2 was not gated by cache state (gates: %v)", rep.Gates)
	}
	if model.calls != 1 {
		t.Fatalf("turn 2 made a model call (%d total) despite being gated", model.calls)
	}
	if cp, ok := loadCheckpoint(ctx); !ok || cp.CoveredCount != cp1.CoveredCount {
		t.Fatal("turn 2 changed the checkpoint — it must stay untouched until KeepAliveSubstitute " +
			"decides to refresh it")
	}

	dispatch, _, reason, ok := KeepAliveSubstitute(ctx.Session)
	if !ok {
		t.Fatalf("KeepAliveSubstitute refused a stale reserve (reason: %q)", reason)
	}
	res := dispatch(5 * time.Second)
	if !res.Committed {
		t.Fatalf("the refresh dispatch did not commit: %+v", res)
	}
	if model.calls != 2 {
		t.Fatalf("the refresh made %d model calls, want 2 total (1 commission + 1 refresh)", model.calls)
	}
	cp2, ok := loadCheckpoint(ctx)
	if !ok {
		t.Fatal("the refresh produced no checkpoint")
	}
	if cp2.CoveredCount <= cp1.CoveredCount {
		t.Errorf("the refreshed checkpoint covers %d messages, want more than the original %d — "+
			"it must cover the GROWN span, not re-derive the same one", cp2.CoveredCount, cp1.CoveredCount)
	}
	if !cp2.Reserved {
		t.Error("the refreshed checkpoint lost its Reserved mark — it must stay a reserve until a " +
			"cold turn graduates it, exactly like the one it replaced")
	}
}

// resummarize_tokens: 0 under cache_state: pre_expiry used to be refused at config time: before
// issue #415, the keep-alive refresh decision compared the reserve's tail directly against
// resummarize_tokens, so 0 meant "every ping sees the reserve as stale" — paying for an identical
// summary forever. #415 replaced that comparison with a money gate in proxy's
// fireSummarySubstitute (expected saving vs. projected cost): a tail is now merely an opportunity
// the gate PRICES, and a zero-or-tiny tail simply never earns back a nonzero call cost, so there
// is no longer a config-time trap to refuse — construction must succeed for both cache_state
// values, exactly like any other resummarize_tokens value.
func TestCacheAwareAllowsResummarizeTokensZeroUnderEitherCacheState(t *testing.T) {
	if _, err := newCacheAwareSummarizer([]byte("resummarize_tokens: 0\ntrigger:\n  cache_state: pre_expiry\n")); err != nil {
		t.Errorf("resummarize_tokens: 0 with cache_state: pre_expiry was rejected: %v", err)
	}
	if _, err := newCacheAwareSummarizer([]byte("resummarize_tokens: 0\ntrigger:\n  cache_state: any\n")); err != nil {
		t.Errorf("resummarize_tokens: 0 with cache_state: any was rejected: %v", err)
	}
}

// caThinkingMsg is an assistant turn carrying a thinking block (bifrost's normalized form of
// Anthropic's `thinking` content, a `Reasoning` string on ChatAssistantMessage).
func caThinkingMsg(text string) bschemas.ChatMessage {
	reasoning := "worked through the problem before answering"
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{Reasoning: &reasoning},
		Content:              &bschemas.ChatMessageContent{ContentStr: &text}}
}

// ⭐ ISSUE: Anthropic's own "Thinking block preservation by model" strips every earlier
// thinking block from a last-turn-only model's cached context the moment a non-tool-result
// message is appended — exactly this component's own instruction. Live-measured on
// claude-haiku-4-5: the commission's own cache read landed ~2,100-2,150 tokens short of the
// real turn immediately before it, in two independent sessions. The gate declines before
// paying for that degraded read at all.
func TestCacheAwareDeclinesOnALastTurnOnlyModelWithAnEarlierThinkingBlock(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	msgs := append([]bschemas.ChatMessage{}, caFixture()...)
	// An earlier (not-last) assistant turn carrying a thinking block.
	msgs = append(msgs[:1], append([]bschemas.ChatMessage{caThinkingMsg("thinking about it")}, msgs[1:]...)...)
	c := caCtx("ca-thinking-haiku")
	c.ModelName = "claude-haiku-4-5"
	var rep components.Report
	req := &bschemas.BifrostChatRequest{Input: msgs}
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["thinking_would_be_stripped"] == 0 {
		t.Fatalf("want gate thinking_would_be_stripped (gates: %v)", rep.Gates)
	}
	if model.calls != 0 {
		t.Errorf("a declined commission still called the model (%d calls)", model.calls)
	}
	if CacheAwareSummarizerThinkingWouldStrip() == 0 {
		t.Error("the decline was not counted")
	}
}

// The identical transcript on Sonnet 5 (keep-all, per the same doc page) must proceed — the
// gate is model-specific, not a blanket refusal on any thinking block anywhere.
func TestCacheAwareProceedsOnAKeepAllModelWithAnEarlierThinkingBlock(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	msgs := append([]bschemas.ChatMessage{}, caFixture()...)
	msgs = append(msgs[:1], append([]bschemas.ChatMessage{caThinkingMsg("thinking about it")}, msgs[1:]...)...)
	c := caCtx("ca-thinking-sonnet")
	c.ModelName = "claude-sonnet-5"
	var rep components.Report
	req := &bschemas.BifrostChatRequest{Input: msgs}
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["thinking_would_be_stripped"] != 0 {
		t.Fatalf("Sonnet 5 is keep-all; must not be gated (gates: %v)", rep.Gates)
	}
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the commission never landed")
	}
	if model.calls != 1 {
		t.Errorf("want 1 model call, got %d", model.calls)
	}
}

// claude-haiku-4-5 with NO thinking blocks anywhere has nothing the gate protects — it must
// proceed exactly like any other turn.
func TestCacheAwareProceedsOnALastTurnOnlyModelWithNoThinkingBlocks(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	c := caCtx("ca-thinking-none")
	c.ModelName = "claude-haiku-4-5"
	var rep components.Report
	req := &bschemas.BifrostChatRequest{Input: caFixture()}
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["thinking_would_be_stripped"] != 0 {
		t.Fatalf("no thinking block exists; must not be gated (gates: %v)", rep.Gates)
	}
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the commission never landed")
	}
	if model.calls != 1 {
		t.Errorf("want 1 model call, got %d", model.calls)
	}
}

// TestThinkingLastTurnOnlyMatchesTheDocumentedModelsAndNoOthers exercises the embedded
// thinking_last_turn_models list directly, against both wire forms a real request uses (the
// dated id and the undated alias) for every family member the doc page names, and against the
// keep-all models whose family prefix a careless bare-generation match string would also catch
// (claude-opus-4-5/4-6, claude-sonnet-4-6, claude-opus-5, claude-sonnet-5, claude-haiku-5-5).
func TestThinkingLastTurnOnlyMatchesTheDocumentedModelsAndNoOthers(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	profiles := s.resolveProfiles()

	lastTurnOnly := []string{
		"claude-haiku-4-5-20251001",
		"claude-haiku-4-5",
		"claude-opus-4-20250514",
		"claude-opus-4-0",
		"claude-opus-4-1-20250805",
		"claude-opus-4-1",
		"claude-sonnet-4-20250514",
		"claude-sonnet-4-0",
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-5",
		"claude-3-7-sonnet-20250219",
		"claude-3-7-sonnet-latest",
	}
	for _, id := range lastTurnOnly {
		if !profiles.thinkingLastTurnOnly(id) {
			t.Errorf("thinkingLastTurnOnly(%q) = false, want true (documented last-turn-only)", id)
		}
	}

	keepAll := []string{
		"claude-opus-4-5-20251101",
		"claude-opus-4-5",
		"claude-opus-4-6-20260201",
		"claude-opus-4-6",
		"claude-opus-5",
		"claude-sonnet-4-6-20260201",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"claude-haiku-5-5",
	}
	for _, id := range keepAll {
		if profiles.thinkingLastTurnOnly(id) {
			t.Errorf("thinkingLastTurnOnly(%q) = true, want false (documented keep-all)", id)
		}
	}
}
