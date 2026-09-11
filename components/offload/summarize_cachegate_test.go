package offload

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// sumCtx builds a Ctx sitting in a named cache phase.
//
// The phases are spelled as strings rather than passed as components.CachePhase values on purpose:
// a test that constructs the phase directly would agree with CachePhase by fiat. These set the raw
// Ctx fields a real request carries — the TTL the body asked for and this session's idle time — so
// the classification under test is the one apply's numbers actually produce.
func sumCtx(st store.Store, phase string, window int, exact bool, billed int) *components.Ctx {
	c := &components.Ctx{
		Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1,
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
		// Both zero: the cache-aware path did not run. Not a contrivance — it is what a
		// non-caching provider, `cache_mode: off`, a bypassed turn and a first turn all produce.
	default:
		panic("unknown phase " + phase)
	}
	return c
}

// newCacheGatedSummarize builds summarize through its REGISTERED CONSTRUCTOR, so the defaults under
// test are the shipped ones rather than a struct literal a test wrote.
//
// min_request_frac: 0 is set deliberately, and it is not a way of dodging the new default: it
// ISOLATES the cache-state conjunct, which is what these tests are about. The fill conjunct has its
// own test below, where the window is the subject rather than a fixture detail.
func newCacheGatedSummarize(t *testing.T, extra string) *Summarize {
	t.Helper()
	c, err := newSummarize([]byte("keep_last: 1\nmin_tokens: 10\nresummarize_tokens: 200\n" +
		"trigger:\n  min_messages: 2\n  min_request_tokens: 10\n  min_request_frac: 0\n" + extra))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	return c.(*Summarize)
}

// sumTranscript is [user goal, then n tool exchanges].
func sumTranscript(n int) []bschemas.ChatMessage {
	msgs := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser}}
	schema.SetMessageText(&msgs[0], "fix the failing tests")
	for i := 1; i <= n; i++ {
		id := "t" + strconv.Itoa(i)
		msgs = append(msgs, callMsg(id), bulkResult(id))
	}
	return msgs
}

// THIS IS THE TEST THE WHOLE CHANGE STANDS ON. If it is deleted, the feature is
// indistinguishable from one that made summarize stop working.
//
// summarize used to decline by `rep.Skipped = true; return nil, nil` BEFORE its checkpoint replay.
// That was survivable while every trigger was a size threshold, because a size threshold is roughly
// monotone: once a transcript is big enough to fire it stays big enough, so the gate kept firing and
// the replay kept happening. `cache_state: pre_expiry` is true on a small fraction of turns BY
// DESIGN — the prompt cache is only ever seconds from expiring for seconds at a time.
//
// So the sequence below is the ordinary one, not an edge case: turn 1 lands in the window and emits
// [head, summary, tail]; turn 2 does not, and under the old control flow emitted the FULL
// transcript. Those bytes diverge from the ones the provider cached at the first summarized
// message, so the whole suffix is re-written at 1.25x fresh — the exact cost this component's
// trigger was changed to avoid, paid on nearly every turn instead of saved on a few. The feature
// would have been a net loss.
//
// The tail is grown past resummarize_tokens on purpose, so the checkpoint is STALE-but-valid. That
// is the reachable state under a rarely-firing trigger (nothing rolls a checkpoint forward while
// the gate is shut), and it is the branch an implementation is most likely to get wrong: tryReuse
// returns ok=FALSE there, carrying `stale` as the only signal that re-emitting is still correct.
func TestSummarizeReplaysItsCheckpointOnATurnTheTriggerDeclines(t *testing.T) {
	model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	s := newCacheGatedSummarize(t, "")
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	// --- Turn 1: inside the pre-expiry window, so the gate opens and a checkpoint is made.
	msgs := sumTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := sumCtx(st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// Turn 1 COMMISSIONS the summary; it does not carry it — the model call happens off the hot
	// path so a slow summarizer can never hold a request past its own cache lifetime. Draining
	// here is what makes turn 2 the turn that splices. It waits on the same channel a real later
	// turn waits on, so this cannot pass merely by being slow enough on one machine.
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	// Preconditions. Without these a fixture that stopped reaching the fresh path would make
	// every assertion below pass vacuously — the summarized and never-summarized shapes are
	// indistinguishable once you stop checking that turn 1 did anything.
	// The precondition is now about the CHECKPOINT, not about turn 1's own body: turn 1 forwards
	// untouched by design and hands the result to the next turn. Asserting on turn1.Input would
	// fail on correct behaviour, and dropping the precondition entirely would let a fixture that
	// stopped summarizing at all pass vacuously — the whole point of having one.
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatalf("turn 1 commissioned no checkpoint, so there is nothing for turn 2 to replay "+
			"(gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	if n := atomic.LoadInt64(&model.calls); n != 1 {
		t.Fatalf("turn 1 made %d model calls, want 1 — the fresh path did not run (gates: %v)", n, rep.Gates)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatal("turn 1 saved no checkpoint, so the replay under test has nothing to replay")
	}
	// head + summary + keepLast(1), computed rather than read off turn 1: turn 1 forwards untouched
	// now and hands the summary to the next turn via the checkpoint.
	summaryShape, summaryText := 3, cpRef(t, ctx)

	// --- Turn 2: warm cache, so the gate SHUTS, and a tail grown past resummarize_tokens so the
	// standing checkpoint is stale rather than reusable as-is.
	grown := sumTranscript(9)
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), grown...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, sumCtx(st, "warm", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("turn 2 was not gated by the cache state, so the path under test never ran "+
			"(gates: %v, events: %v)", rep.Gates, rep.Events)
	}

	// The property.
	if len(turn2.Input) == len(grown) {
		t.Errorf("a gated turn sent the transcript FULL (%d messages) after earlier turns sent the "+
			"summarized shape (%d messages). Those bytes diverge from the cached prefix at the "+
			"first summarized message, so the provider re-writes the whole suffix at 1.25x fresh — "+
			"the cost the cache-state trigger exists to avoid, now paid on every quiet turn "+
			"(gates: %v, events: %v)", len(turn2.Input), summaryShape, rep.Gates, rep.Events)
	}
	// Byte-identical, or the replay is itself a flip.
	if got := schema.MessageText(turn2.Input[1]); got != summaryText {
		t.Errorf("the replayed summary differs from the checkpoint it came from, so the replay is itself a "+
			"cache flip:\n got %q\nwant %q", got, summaryText)
	}
	// A replay is free. Paying for one here would mean the gate is not gating the model call,
	// which is the only thing it was ever supposed to gate.
	if n := atomic.LoadInt64(&model.calls); n != 1 {
		t.Errorf("the gated turn made a model call (%d total): the gate must suppress the SPEND, "+
			"not the splice", n)
	}
	// And it must be counted as a replay rather than an act, or /stats reports a component
	// amortizing old work as one paying for new work (#176) — which under this trigger is the
	// dominant activation, so the misreading would be the normal case.
	if rep.Replays != 1 {
		t.Errorf("gated replay recorded %d replays, want 1 (events: %v)", rep.Replays, rep.Events)
	}
}

// A replay needs no model, so a deployment with no summarizer configured must still get one.
//
// The model used to be resolved ABOVE the replay and a missing one returned early, so this
// deployment sent the summarized shape on the turns it could summarize and the FULL transcript on
// every turn afterwards — the same byte-flip as the test above, arriving by a different route and
// on a deployment that never configured anything wrong.
func TestSummarizeReplaysItsCheckpointWhenNoModelIsAvailable(t *testing.T) {
	model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	s := newCacheGatedSummarize(t, "")
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := sumTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := sumCtx(st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// Turn 1 COMMISSIONS the summary; it does not carry it — the model call happens off the hot
	// path so a slow summarizer can never hold a request past its own cache lifetime. Draining
	// here is what makes turn 2 the turn that splices. It waits on the same channel a real later
	// turn waits on, so this cannot pass merely by being slow enough on one machine.
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatalf("turn 1 commissioned no checkpoint, so there is nothing to replay (gates: %v)", rep.Gates)
	}
	summaryText := cpRef(t, ctx)

	// The model goes away. Still in the pre-expiry window, so the trigger itself would allow a
	// fresh summary — the model's absence is the only reason there will not be one.
	s.modelClient = nil
	grown := sumTranscript(9)
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), grown...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, sumCtx(st, "pre_expiry", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["no_model"] == 0 {
		t.Fatalf("the fixture still had a model, so the path under test never ran (gates: %v)", rep.Gates)
	}
	if len(turn2.Input) == len(grown) {
		t.Errorf("a turn with no summarizer sent the transcript FULL (%d messages) after earlier "+
			"turns sent it summarized: the same cache flip, on a deployment that simply never "+
			"configured a cheap model (gates: %v)", len(turn2.Input), rep.Gates)
	}
	if got := schema.MessageText(turn2.Input[1]); got != summaryText {
		t.Errorf("replayed summary differs:\n got %q\nwant %q", got, summaryText)
	}
}

// `resummarize_tokens: 0` means "roll the checkpoint forward on every eligible turn", and tryReuse
// used to answer that by returning before it had even LOADED the checkpoint — so it could report
// neither reuse nor staleness.
//
// Harmless while the caller always had a fresh path available: it fell through and re-summarized.
// Under a gate that shuts, "neither" means "send it full", so such a config would oscillate between
// the two shapes on alternating turns — the worst available behaviour, and reachable from a value
// that is documented as merely a cadence.
func TestAZeroResummarizeTokensConfigStillReplaysRatherThanFlipping(t *testing.T) {
	model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	c, err := newSummarize([]byte("keep_last: 1\nmin_tokens: 10\nresummarize_tokens: 0\n" +
		"trigger:\n  min_messages: 2\n  min_request_tokens: 10\n  min_request_frac: 0\n"))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	s := c.(*Summarize)
	s.modelClient = model
	st := store.NewMemory(store.Options{MaxEntries: 400})

	msgs := sumTranscript(3)
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	ctx := sumCtx(st, "pre_expiry", 0, false, 0)
	var rep components.Report
	if _, err := s.Offload(turn1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// Turn 1 COMMISSIONS the summary; it does not carry it — the model call happens off the hot
	// path so a slow summarizer can never hold a request past its own cache lifetime. Draining
	// here is what makes turn 2 the turn that splices. It waits on the same channel a real later
	// turn waits on, so this cannot pass merely by being slow enough on one machine.
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("turn 1's summary never landed")
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatalf("turn 1 commissioned no checkpoint (gates: %v)", rep.Gates)
	}
	summaryText := cpRef(t, ctx)

	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	rep = components.Report{}
	if _, err := s.Offload(turn2, &rep, sumCtx(st, "warm", 0, false, 0)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if len(turn2.Input) == len(msgs) {
		t.Errorf("with resummarize_tokens: 0 a gated turn sent the transcript FULL, so this config "+
			"alternates between the summarized and full shapes turn by turn (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(turn2.Input[1]); got != summaryText {
		t.Errorf("replayed summary differs:\n got %q\nwant %q", got, summaryText)
	}
}

// The fill conjunct, with the window and the RULER as the subject.
//
// Two cases here are load-bearing for different reasons.
//
// The guessed-window case: the resolver said ok=true, so CtxWindow is non-zero and every existing
// code path treats it as usable -- but it came from the substring table of last resort, which
// answers 200,000 for every Opus against a real 1,000,000. A 0.9 fraction resolved against that
// fires at 180k. Declining is the only honest answer, and `CtxWindow > 0` cannot reach it: only
// the provenance can.
//
// The RULER cases: the fill is Ctx.PrevBilledInput -- the provider's own count for this session's
// previous turn -- and not schema.MessagesTokens. The fixture below is a handful of tokens of
// message text against a 1M window, so under the old arithmetic (frac*window compared against
// MessagesTokens) EVERY row would decline, including the two that must fire. The precondition
// asserts that gap explicitly, so this test cannot quietly stop being about the ruler.
func TestTheFillConjunctDeclinesRatherThanResolveAgainstAGuessedWindow(t *testing.T) {
	msgs := sumTranscript(3)
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	tokens := schema.MessagesTokens(req)
	if tokens < 20 {
		t.Fatalf("fixture is only %d tokens, too small to be a transcript at all", tokens)
	}

	const window = 1_000_000
	const floor = 0.9 * window
	// The whole point of the change: our own count of this transcript is nowhere near the
	// threshold, and the gate must nonetheless fire when the PROVIDER has billed past it.
	if float64(tokens) >= floor {
		t.Fatalf("fixture counts %d message-text tokens, which already clears the %.0f floor; "+
			"this test can no longer distinguish the two rulers", tokens, floor)
	}

	cases := []struct {
		name      string
		window    int
		exact     bool
		billed    int
		wantFires bool
		wantGate  string
	}{
		{"a session the provider has billed past the fraction fires", window, true, 950_000, true, ""},
		{"a session billed well short of it declines", window, true, 100_000, false, "below_request_trigger"},
		{"a GUESSED window declines even though it is non-zero", window, false, 950_000, false, "window_not_exact"},
		{"an unknown window declines rather than silently dropping the conjunct", 0, false, 950_000, false, "window_not_exact"},
		{"no previous billed figure declines: unknown fill is not an empty one", window, true, 0, false, "window_not_exact"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &countingModel{out: "SUMMARY: explored the handler, 3 tests fail."}
			// cache_state: any, so the phase cannot be what decides this subtest.
			s := newCacheGatedSummarizeWithFrac(t, "trigger:\n  min_messages: 2\n  cache_state: any\n")
			s.modelClient = model
			st := store.NewMemory(store.Options{MaxEntries: 400})
			in := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
			var rep components.Report
			ctx := sumCtx(st, "unknown", tc.window, tc.exact, tc.billed)
			if _, err := s.Offload(in, &rep, ctx); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			// The call is COMMISSIONED, not made inline, so drain before counting. A turn that
			// declined started nothing and this returns immediately; a turn that fired started
			// exactly one call and this waits for it.
			WaitForSummaryForTest(ctx.Session, 5*time.Second)
			fired := atomic.LoadInt64(&model.calls) == 1
			if fired != tc.wantFires {
				t.Errorf("fired=%v want %v (gates: %v)", fired, tc.wantFires, rep.Gates)
			}
			if tc.wantGate != "" && rep.Gates[tc.wantGate] == 0 {
				t.Errorf("a turn that declined must say why: want gate %q, got %v", tc.wantGate, rep.Gates)
			}
		})
	}
}

// newCacheGatedSummarizeWithFrac keeps the SHIPPED min_request_frac default (0.9) rather than
// zeroing it, so the fraction is the thing under test.
func newCacheGatedSummarizeWithFrac(t *testing.T, trigger string) *Summarize {
	t.Helper()
	c, err := newSummarize([]byte("keep_last: 1\nmin_tokens: 10\nresummarize_tokens: 200\n" + trigger))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	return c.(*Summarize)
}

// The defaults have to be SAYABLE and the opt-out has to work, because the whole compatibility
// story for this change is "an operator whose client does not cap the context sets cache_state:
// any". If that sentence is not true the change is a regression for those deployments.
//
// min_request_frac: 0 is the subtle half. It is a float64 whose zero means "no constraint", so an
// explicitly written 0 is indistinguishable from an absent key after decoding — a constructor that
// defaulted off the zero value would make "compact in the pre-expiry window at ANY size"
// unwritable. The default is installed off a raw-YAML presence probe instead, and this pins it.
func TestSummarizeTriggerDefaultsAndTheirOptOut(t *testing.T) {
	t.Run("an absent key takes the shipped default", func(t *testing.T) {
		s := newCacheGatedSummarizeWithFrac(t, "")
		if s.trigger.CacheState != components.CacheStatePreExpiryOrCold {
			t.Errorf("cache_state defaulted to %q, want %q", s.trigger.CacheState,
				components.CacheStatePreExpiryOrCold)
		}
		if s.trigger.MinRequestFrac != summarizeDefaultRequestFrac {
			t.Errorf("min_request_frac defaulted to %v, want %v", s.trigger.MinRequestFrac, summarizeDefaultRequestFrac)
		}
	})
	t.Run("an explicit zero fraction is honoured, not overwritten by the default", func(t *testing.T) {
		s := newCacheGatedSummarizeWithFrac(t, "trigger:\n  min_request_frac: 0\n")
		if s.trigger.MinRequestFrac != 0 {
			t.Errorf("an explicitly written min_request_frac: 0 became %v — the presence probe is "+
				"not working, and 'compact at any size in the pre-expiry window' is unwritable",
				s.trigger.MinRequestFrac)
		}
	})
	t.Run("cache_state any restores the size-only behaviour", func(t *testing.T) {
		s := newCacheGatedSummarizeWithFrac(t, "trigger:\n  cache_state: any\n")
		if !s.trigger.CacheAllows(nil, components.CachePhaseWarm) {
			t.Error("cache_state: any refused a warm turn, so the documented opt-out for a client " +
				"that does not cap its own context does not actually opt out")
		}
	})
	t.Run("an unrecognised cache_state is refused, not read as any", func(t *testing.T) {
		_, err := newSummarize([]byte("trigger:\n  cache_state: pre-expiry\n"))
		if err == nil {
			t.Error("a typo in the one key deciding WHEN this component fires was accepted; it " +
				"would silently turn the cache gate off while reading as though it were on")
		}
	})
	t.Run("the form advertises the defaults the constructor applies", func(t *testing.T) {
		// Field.Default is documented as what an ABSENT key means to the component, and
		// form.go's normalize() writes an enum's Default back into the saved document for an
		// unset value — so a wrong one here does not merely mislabel the settings page, it
		// persists a cache_state summarize never chose.
		want := map[string]any{
			"trigger.cache_state":      components.CacheStatePreExpiryOrCold,
			"trigger.min_request_frac": summarizeDefaultRequestFrac,
		}
		seen := map[string]bool{}
		for _, f := range components.Fields("summarize") {
			if w, ok := want[f.Key]; ok {
				seen[f.Key] = true
				if f.Default != w {
					t.Errorf("%s: form default %v, constructor applies %v", f.Key, f.Default, w)
				}
			}
		}
		for k := range want {
			if !seen[k] {
				t.Errorf("%s is not declared for summarize at all, so the settings page cannot "+
					"set the key that decides when it fires", k)
			}
		}
	})
}

// cpRef is the summary bytes a session's checkpoint holds — the reference every replay must match.
//
// It replaced `schema.MessageText(turn1.Input[1])` across this file when summaries moved off the
// hot path: turn 1 commissions the work and forwards untouched, so its own body no longer contains
// the summary. The PROPERTY under test is unchanged, and is the one this file exists for — every
// turn that splices must splice identical bytes, or the replay is itself the cache flip it was
// meant to prevent — but the authority for those bytes is now the checkpoint rather than turn 1.
func cpRef(t *testing.T, ctx *components.Ctx) string {
	t.Helper()
	cp, ok := loadCheckpoint(ctx)
	if !ok {
		t.Fatal("no checkpoint: turn 1 commissioned no summary, so there is no reference to " +
			"compare replays against")
	}
	return cp.SummaryMsg
}

// commissionThenSplice runs the two turns the async execution model always takes: one that
// COMMISSIONS a summary and forwards untouched, and one that SPLICES the result. It returns the
// second turn's request — the one carrying the summarized transcript — and that turn's report.
//
// It exists because "summarize this and show me the output" stopped being a single call. Every test
// that wants a summarized body needs both turns, and writing them out by hand invites the one
// mistake that does not fail loudly: forget the drain and the test becomes flaky rather than
// broken, which is the worst of the available failure modes.
func commissionThenSplice(t *testing.T, s *Summarize, msgs []bschemas.ChatMessage,
	ctx *components.Ctx) (*bschemas.BifrostChatRequest, *components.Report) {
	t.Helper()
	first := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep1 components.Report
	if _, err := s.Offload(first, &rep1, ctx); err != nil {
		t.Fatalf("the commissioning turn must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatalf("no summary landed within 5s (gates: %v, events: %v)", rep1.Gates, rep1.Events)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatalf("the commissioning turn produced no checkpoint, so the second turn has nothing "+
			"to splice (gates: %v, events: %v)", rep1.Gates, rep1.Events)
	}
	second := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep2 components.Report
	if _, err := s.Offload(second, &rep2, ctx); err != nil {
		t.Fatalf("the splicing turn must fail open: %v", err)
	}
	return second, &rep2
}
