package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fakeSummaryModel is the components.MessagesModel the registered keep-alive candidate calls
// through — a stand-in for cache_aware_summarizer's own resolved model, so these tests exercise
// the keeper's substitution decision without a network.
type fakeSummaryModel struct {
	calls atomic.Int64
	out   string
	err   error
}

func (m *fakeSummaryModel) Complete(context.Context, string) (string, error) {
	return "FLAT-STRING PATH", nil
}

func (m *fakeSummaryModel) CompleteMessages(context.Context, string, []bschemas.ChatMessage) (string, error) {
	m.calls.Add(1)
	if m.err != nil {
		return "", m.err
	}
	return m.out, nil
}

// kaCandidate registers a cache_aware_summarizer keep-alive candidate for a session, with a
// transcript big enough that offload's own min_tokens gate (checked inside commissionSync via
// the summarizer's model call, not re-validated here) is irrelevant — commissionSync pays no
// attention to span size, only to whether a checkpoint already exists and whether the
// single-flight/concurrency bound admits the call.
func kaCandidate(t *testing.T, session string, st store.Store, model components.MessagesModel) {
	t.Helper()
	kaCandidateWithCacheState(t, session, st, model, "", 0)
}

// kaCandidateWithCacheState is kaCandidate plus control over KeepAliveCandidateInfo, for the
// tests specifically about the `cache_state: pre_expiry` phase check in fireSummarySubstitute.
func kaCandidateWithCacheState(t *testing.T, session string, st store.Store, model components.MessagesModel,
	cacheState string, preExpirySeconds int) {
	t.Helper()
	msg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&msg, "fix the failing tests, there is a lot of context here")
	span := []bschemas.ChatMessage{msg}
	ask := append([]bschemas.ChatMessage(nil), span...)
	ask = append(ask, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser})
	call := func(ctx context.Context) (string, error) { return model.CompleteMessages(ctx, "", ask) }
	offload.RegisterKeepAliveCandidateForTest(session, st, call, "messages", span, 1, cacheState, preExpirySeconds)
	// The registry is a package GLOBAL in offload, keyed by session — and "sess-1" is the shared
	// session name every other test in this file's testKeeper/recordOne helpers uses. Left
	// registered, a candidate from this test silently hijacks an unrelated LATER test's ping into
	// a substitute call instead, which is exactly how TestPingThatWritesInsteadOfReadingStopsTheSession
	// was found failing under -race: a stale "sess-1" candidate from this test ran instead of that
	// test's own fake ping, so the write-vs-read guard it exercises never saw a ping at all.
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })
}

// ⭐ THE SUBSTITUTION ITSELF: a due ping for a session with a registered candidate becomes the
// summarizer's own call instead of a bare cache-read ping, counted once toward the keeper's own
// ping ledger and never also sent as an ordinary ping.
func TestKeepAliveSubstitutesCacheAwareSummarizerForAPing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidate(t, "sess-1", st, model)

	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 {
		t.Fatalf("summarySubstituted = %d, want 1", k.summarySubstituted.Load())
	}
	if fs.n() != 0 {
		t.Errorf("sent %d ordinary pings — the substitute must replace the ping, not add to it", fs.n())
	}
	if got := k.pings.Load(); got != 1 {
		t.Errorf("pings = %d, want 1 — a substituted ping must be counted once, as THE ping for "+
			"this idle span", got)
	}
	if model.calls.Load() != 1 {
		t.Errorf("the summarizer's model was called %d times, want 1", model.calls.Load())
	}
	if b, ok := st.Get(store.SumPrefix + "sess-1"); !ok || len(b) == 0 {
		t.Error("the substitute call produced no checkpoint — it ran but is not doing the " +
			"compaction half of its job")
	}
}

// A substitute attempt that FAILS (model error, timeout, empty reply) must fall back to an
// ordinary ping rather than leaving the idle span unpinged — fail open.
func TestKeepAliveSubstituteFallsBackToPingOnFailure(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{err: errors.New("upstream refused")}
	kaCandidate(t, "sess-1", st, model)

	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings after the substitute failed, want 1 (fail open)", fs.n())
	}
	if k.summarySubstituted.Load() != 0 {
		t.Errorf("summarySubstituted = %d, want 0 — the call failed", k.summarySubstituted.Load())
	}
	if k.summarySubstituteFailed.Load() != 1 {
		t.Error("the fallback was not counted as a substitute failure")
	}
	// Still counted once overall: the fallback ping IS the ping for this span.
	if got := k.pings.Load(); got != 1 {
		t.Errorf("pings = %d, want 1", got)
	}
}

// The keep-alive $ cap is respected on the SUBSTITUTE's own (larger) projected cost, not the
// bare ping's — a call that would be affordable as a 1-token ping can still be unaffordable as a
// full summary, and that must fall back rather than silently overspending.
func TestKeepAliveSubstituteRespectsTheCostCapAndFallsBackToAPing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	// Output-heavy pricing: the ping's own 1-token budget is cheap, but the summary's assumed
	// 2000-token budget is not. CacheRead is kept small enough that the PING itself still
	// passes the record-time guard (pingable()), so the entry is tracked at all.
	k.h.opts.Prices = fixedPrice{modelinfo.Price{CacheRead: 3.8e-7, Output: 19e-6}}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>should never be reached</summary>"}
	kaCandidate(t, "sess-1", st, model)

	pol := kaPolicy()
	pol.MaxUSDPerPing = 0.02 // ping ≈ $0.0076; summary ≈ $0.0456 — only the summary trips this
	tn := &Tenancy{ID: "t1", Cache: pol}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	at := clock.now()
	k.record(tn, "sess-1", "", at.Add(-time.Second), []byte(kaBody),
		upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
		http.StatusOK, Usage{CacheRead: 20000, CacheWrite: 0}, true)
	k.record(tn, "sess-1", "", at, []byte(kaBody),
		upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
		http.StatusOK, Usage{CacheRead: 20000, CacheWrite: 0}, true)
	if k.Stats().Live != 1 {
		t.Fatalf("the session was not tracked — its PING cost must stay inside the cap for this "+
			"test to isolate the substitute's own guard (live=%d)", k.Stats().Live)
	}

	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings, want 1 — the substitute should have been refused on "+
			"cost and fallen back", fs.n())
	}
	if model.calls.Load() != 0 {
		t.Errorf("the summarizer's model was called %d times — an over-budget substitute must "+
			"never be DISPATCHED, not merely refused after paying for it", model.calls.Load())
	}
	if k.summarySubstituteOverBudget.Load() != 1 {
		t.Error("the over-budget refusal was not counted")
	}
	if k.summarySubstituted.Load() != 0 {
		t.Error("summarySubstituted was incremented despite the cost cap")
	}
}

// A `cache_state: pre_expiry` candidate must only be substituted when THIS ping actually lands
// inside the component's own pre-expiry window — not merely whenever the keeper decides to ping
// at all. HeadTTL1h gives the entry a one-hour cache lifetime while kaPolicy's Idle (280s) still
// makes the keeper ping it in under five minutes, so the ping is nowhere near pre-expiry by the
// component's own (default 60s) reckoning: a real divergence between "the keeper is due" and
// "the candidate's window is live", not a contrived one.
func TestKeepAliveSubstituteRespectsPreExpiryPhaseNotJustKeeperTiming(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>should not be used yet</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 60)

	pol := kaPolicy()
	pol.HeadTTL1h = true
	recordOne(t, k, pol, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings, want 1 — a pre_expiry candidate must not be substituted "+
			"on a ping that lands nowhere near its own pre-expiry window", fs.n())
	}
	if model.calls.Load() != 0 {
		t.Errorf("the summarizer's model was called %d times — a phase-mismatched substitute must "+
			"never be dispatched", model.calls.Load())
	}
	if k.summarySubstitutePhaseMismatch.Load() != 1 {
		t.Error("the phase mismatch was not counted")
	}
	if k.summarySubstituted.Load() != 0 {
		t.Error("summarySubstituted was incremented despite the phase mismatch")
	}
}

// buildRefreshCandidate seeds a `pre_expiry` reserve covering one short message, then registers
// a keep-alive candidate whose span is that SAME message plus a tail message of tailText — the
// one lever these money-gate tests need to control info.RefreshTailTokens without reaching into
// offload's package-private sumCheckpoint/spanHash. tailText is taken literally rather than
// computed from a token estimate: a real BPE tokenizer merges a repeated single character far
// more aggressively than ordinary text, so the two tests below pass VARIED filler sized with a
// wide margin on either side of their break-even instead of a precise token count.
func buildRefreshCandidate(t *testing.T, session string, st store.Store, model *fakeSummaryModel, tailText string) {
	t.Helper()
	prefixMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&prefixMsg, "fix the failing tests, there is a lot of context here")
	tailMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant}
	schema.SetMessageText(&tailMsg, tailText)
	span := []bschemas.ChatMessage{prefixMsg, tailMsg}

	offload.SeedReservedCheckpointForTest(session, st, span[:1], "<summary>explored the handler</summary>")

	ask := append([]bschemas.ChatMessage(nil), span...)
	ask = append(ask, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser})
	call := func(ctx context.Context) (string, error) { return model.CompleteMessages(ctx, "", ask) }
	offload.RegisterKeepAliveCandidateForTest(session, st, call, "messages", span, 1, "pre_expiry", 60)
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })
}

// tailFiller is varied, realistic-shaped filler text (not a repeated single character, which a
// real BPE tokenizer compresses far more aggressively than ordinary prose) — n repeats of an
// 11-word sentence, roughly 12-14 BPE tokens each under o200k_base.
func tailFiller(n int) string {
	return strings.Repeat("the quick brown fox jumps over the lazy dog near the fence. ", n)
}

// ⭐ ISSUE #415: a refresh below break-even must stay a plain ping, with the model never even
// called — the money gate refuses BEFORE dispatch, not after paying for a call it then discards.
func TestKeepAliveRefreshBelowBreakEvenPingsNormally(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	// CacheRead priced at 0 isolates the projected cost to the output-budget guess alone (a
	// fixed $0.02 at these prices), so the break-even point is simply "does the tail's own
	// cache-write value clear $0.02" — exactly the number issue #415's own worked examples use.
	k.h.opts.Prices = fixedPrice{modelinfo.Price{CacheRead: 0, Output: 1e-5, CacheWrite: 5e-5}}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>should never be reached</summary>"}
	// Two sentences of new material, on the order of 25 real tokens: at $0.00005/token
	// cache-write, worth roughly $0.001 — far short of the $0.02 refresh it would cost.
	buildRefreshCandidate(t, "sess-1", st, model, tailFiller(2))

	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings, want 1 — a below-break-even refresh must fall back to a "+
			"plain ping", fs.n())
	}
	if model.calls.Load() != 0 {
		t.Errorf("the summarizer's model was called %d times — a below-break-even refresh must "+
			"never be DISPATCHED, not merely refused after paying for it", model.calls.Load())
	}
	if k.summarySubstituteRefreshNotWorthIt.Load() != 1 {
		t.Error("the below-break-even refusal was not counted")
	}
	if k.summarySubstituteRefreshPaid.Load() != 0 {
		t.Error("summarySubstituteRefreshPaid was incremented despite being below break-even")
	}
	if k.summarySubstituted.Load() != 0 {
		t.Error("summarySubstituted was incremented despite the refresh being refused")
	}
}

// The identical setup, but with enough new material that the refresh pays for itself: the
// summary call fires INSTEAD of the ping, counted as both the refresh decision and the ping.
func TestKeepAliveRefreshAboveBreakEvenRefreshes(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	k.h.opts.Prices = fixedPrice{modelinfo.Price{CacheRead: 0, Output: 1e-5, CacheWrite: 5e-5}}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, refreshed</summary>"}
	// 100 repeats of an 11-word sentence, on the order of 1,300 real tokens: at $0.00005/token
	// cache-write, worth roughly $0.065 — comfortably past the same $0.02 refresh cost as the
	// test above.
	buildRefreshCandidate(t, "sess-1", st, model, tailFiller(100))

	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 {
		t.Fatalf("summarySubstituted = %d, want 1 once the refresh clears break-even",
			k.summarySubstituted.Load())
	}
	if fs.n() != 0 {
		t.Errorf("sent %d ordinary pings — an above-break-even refresh should have replaced it", fs.n())
	}
	if model.calls.Load() != 1 {
		t.Errorf("the summarizer's model was called %d times, want 1", model.calls.Load())
	}
	if k.summarySubstituteRefreshPaid.Load() != 1 {
		t.Error("the above-break-even refresh was not counted as paid")
	}
	if k.summarySubstituteRefreshNotWorthIt.Load() != 0 {
		t.Error("summarySubstituteRefreshNotWorthIt was incremented despite clearing break-even")
	}
	if got := k.pings.Load(); got != 1 {
		t.Errorf("pings = %d, want 1 — a refresh must be counted once, as THE ping for this idle span", got)
	}
}

// The same `pre_expiry` candidate DOES substitute once the ping actually lands inside its
// window — proving the test above is a real gate and not a permanent refusal.
func TestKeepAliveSubstituteFiresOncePreExpiryPhaseIsLive(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>fires inside the window</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 60)

	// Default 5-minute TTL (no HeadTTL1h): a ping at 280s idle has 20s of a 5-minute lifetime
	// left, which is inside the default 60s pre-expiry window.
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	k.sweep(clock.advance(281 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 {
		t.Fatalf("summarySubstituted = %d, want 1 once the ping lands inside the pre-expiry window",
			k.summarySubstituted.Load())
	}
	if fs.n() != 0 {
		t.Errorf("sent %d ordinary pings — the substitute should have replaced it", fs.n())
	}
}

// ⭐ THE PIN: a summary call takes the place of exactly ONE ping. It counts toward
// keepalive_max_pings like any other ping, it does not add an extra ping, and it does not
// extend K — the cache strategy's own MaxPings is still what decides how long the session's
// cache stays warm. "The preset never overrides the cache strategy."
//
// With MaxPings=2 and no reserve yet: ping 1 creates the session's first reserve (there is
// nothing to compare it against yet, so it is not gated by the #415 refresh test — see
// KeepAliveCandidateInfo.IsRefresh). Ping 2 finds that same reserve already covering every
// message the (unchanged) candidate span has — nothing new to weigh — so it refuses
// substitution (`checkpoint_exists`, exactly like `any`) and falls back to an ordinary ping.
// There is no third ping: K is 2.
func TestKeepAliveSummaryCallCountsAsExactlyOnePing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 60)

	start := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, start, upstream{base: "http://up", path: "/v1/messages"})

	// Ping 1, at 281s idle: inside the default 60s pre-expiry window of the default 5-minute
	// TTL — creates the reserve.
	if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("ping 1 did not fire (%d)", n)
	}
	waitPings(t, k, 1)
	if k.summarySubstituted.Load() != 1 {
		t.Fatalf("ping 1 was not the summary substitute (summarySubstituted=%d)", k.summarySubstituted.Load())
	}
	if fs.n() != 0 {
		t.Fatalf("ping 1 sent an ordinary ping too (%d) — it must be ONE ping, not two", fs.n())
	}
	if model.calls.Load() != 1 {
		t.Fatalf("ping 1 made %d model calls, want 1", model.calls.Load())
	}

	// Ping 2, 280s after ping 1 restarted the clock (same arithmetic as
	// TestMaxPingsAndPingRestartsTheClock): still inside the pre-expiry window, but the
	// reserve from ping 1 is already current — a plain ping, not a second summary call.
	if n := k.sweep(clock.advance(280 * time.Second)); n != 1 {
		t.Fatalf("ping 2 did not fire (%d)", n)
	}
	waitPings(t, k, 2)
	if fs.n() != 1 {
		t.Fatalf("ping 2 sent %d ordinary pings, want 1 — the current reserve must refuse a second "+
			"summary call", fs.n())
	}
	if model.calls.Load() != 1 {
		t.Fatalf("ping 2 made another model call (%d total) — the reserve was already current", model.calls.Load())
	}
	if k.summarySubstituteCheckpointExists.Load() != 1 {
		t.Error("ping 2's refusal was not counted as checkpoint_exists")
	}
	if k.summarySubstituted.Load() != 1 {
		t.Errorf("summarySubstituted = %d, want 1 — only ping 1 was a substitute", k.summarySubstituted.Load())
	}

	// No ping 3: MaxPings is 2, same as the plain-ping K bound tested in
	// TestMaxPingsAndPingRestartsTheClock. The reserve existing does not extend K.
	if n := k.sweep(clock.advance(1000 * time.Second)); n != 0 {
		t.Fatalf("fired a third ping (%d); MaxPings is 2 regardless of the reserve", n)
	}
	if got := k.pings.Load(); got != 2 {
		t.Errorf("pings = %d, want 2 total — one summary call plus one plain ping, never a third", got)
	}
}

// The identical setup with MaxPings=3: one summary call, then two plain pings, then nothing.
// Confirms the pin holds for a K other than 2 — the summary call is not special-cased into its
// own, separate budget.
func TestKeepAliveSummaryCallCountsAsOnePingWithThreeMaxPings(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidateWithCacheState(t, "sess-1", st, model, "pre_expiry", 60)

	pol := kaPolicy()
	pol.MaxPings = 3
	start := clock.now()
	recordOne(t, k, pol, kaBody, start, upstream{base: "http://up", path: "/v1/messages"})

	if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("ping 1 did not fire (%d)", n)
	}
	waitPings(t, k, 1)
	if k.summarySubstituted.Load() != 1 {
		t.Fatalf("ping 1 was not the summary substitute (summarySubstituted=%d)", k.summarySubstituted.Load())
	}

	if n := k.sweep(clock.advance(280 * time.Second)); n != 1 {
		t.Fatalf("ping 2 did not fire (%d)", n)
	}
	waitPings(t, k, 2)
	if fs.n() != 1 {
		t.Fatalf("ping 2 sent %d ordinary pings, want 1", fs.n())
	}

	if n := k.sweep(clock.advance(280 * time.Second)); n != 1 {
		t.Fatalf("ping 3 did not fire (%d)", n)
	}
	waitPings(t, k, 3)
	if fs.n() != 2 {
		t.Fatalf("ping 3 sent %d ordinary pings total, want 2", fs.n())
	}
	if model.calls.Load() != 1 {
		t.Fatalf("the model was called %d times total, want 1 — only ping 1 was ever a summary call",
			model.calls.Load())
	}

	// No ping 4: MaxPings is 3.
	if n := k.sweep(clock.advance(1000 * time.Second)); n != 0 {
		t.Fatalf("fired a fourth ping (%d); MaxPings is 3", n)
	}
	if got := k.pings.Load(); got != 3 {
		t.Errorf("pings = %d, want 3 total — one summary call plus two plain pings", got)
	}
}

// #423 × #402: the summarizer's keep-alive candidate is one THREAD's conversation, while the
// registry is keyed by session. Only the owning thread's ping may use it, and another thread's
// request or retirement must not clear it.
func TestKeepAliveSubstituteBelongsToTheThreadThatRegisteredIt(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>main thread so far.</summary>"}
	const sess = "sess-thread-cand"

	// The main thread's request registers the candidate.
	start := time.Now()
	kaCandidate(t, sess, st, model)
	k.noteCandidate("t1", sess, "", start)

	// A subagent's later request registers nothing; it must not clear the main thread's candidate.
	k.noteCandidate("t1", sess, "a:S", time.Now())
	if !offload.KeepAliveCandidateRegisteredSince(sess, time.Time{}) {
		t.Fatal("a subagent's request cleared the main thread's candidate")
	}

	tn := &Tenancy{ID: "t1", Cache: kaPolicy()}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	up := upstream{base: "http://up", path: "/v1/messages"}
	rec := func(th string, at time.Time) {
		for i := 0; i < 2; i++ {
			k.record(tn, sess, th, at.Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
		}
	}
	// The subagent goes idle a minute before the main thread, so its ping comes due ALONE first.
	// Both pings at once would hide a wrong owner: the second one finds the first one's checkpoint
	// or its in-flight call and falls back to a plain ping either way.
	t0 := clock.now()
	rec("a:S", t0.Add(-60*time.Second))
	rec("", t0)
	waitSub := func(want int64) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && k.summarySubstituted.Load()+int64(fs.n()) < want {
			time.Sleep(time.Millisecond)
		}
	}
	if n := k.sweep(t0.Add(-60*time.Second + 282*time.Second)); n != 1 {
		t.Fatalf("first sweep fired %d pings, want 1 (the subagent's)", n)
	}
	waitSub(1)
	if got := k.summarySubstituted.Load(); got != 0 {
		t.Fatalf("the subagent's ping used the main thread's candidate (summarySubstituted = %d)", got)
	}
	if got := fs.n(); got != 1 {
		t.Fatalf("subagent: ordinary pings = %d, want 1", got)
	}
	if n := k.sweep(t0.Add(282 * time.Second)); n != 1 {
		t.Fatalf("second sweep fired %d pings, want 1 (the main thread's)", n)
	}
	waitSub(2)
	if got := k.summarySubstituted.Load(); got != 1 {
		t.Errorf("the main thread's ping was not substituted (summarySubstituted = %d)", got)
	}

	// Control: the OWNING thread's later request does clear its stale candidate.
	kaCandidate(t, sess, st, model)
	k.noteCandidate("t1", sess, "", time.Now().Add(-time.Hour)) // registered after this start: re-own
	k.noteCandidate("t1", sess, "", time.Now())                 // the main thread moves on
	if offload.KeepAliveCandidateRegisteredSince(sess, time.Time{}) {
		t.Error("control: the owning thread's next request did not clear its stale candidate")
	}
}
