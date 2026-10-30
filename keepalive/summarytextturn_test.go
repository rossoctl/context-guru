package keepalive

import (
	"errors"
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/store"
)

// summaryHeld returns whether the keeper still holds sess1's entry.
func summaryHeld(k *Keeper) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.live[sess1.id()]
	return ok
}

// twoPlainPings records a text turn and sends both planned pings, checking they went out plain.
func twoPlainPings(t *testing.T, k *Keeper, fs *fakeDispatcher, clock *testClock, model *fakeSummaryModel) {
	t.Helper()
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	for i := int64(1); i <= 2; i++ {
		k.Sweep(clock.advance(281 * time.Second))
		waitPings(t, k, i)
		if model.calls.Load() != 0 || fs.n() != int(i) {
			t.Fatalf("ping %d: summary calls %d, plain %d; want 0 and %d (every planned ping is plain after a text turn)",
				i, model.calls.Load(), fs.n(), i)
		}
	}
}

// #776 (a1): after a text turn a pre_expiry summary does not take a planned ping. It goes out once, as
// the extra request in the middle of its pre-expiry window: the last ping + ttl - window/2. kaPolicy's
// Idle is 280 s, so the window is the 60 s default and the slot is 270 s (at the shipped 240 s Idle the
// window is 64 s and the slot 268 s; see TestTheExtraSlotAtTheShippedDefaults). Nothing after it.
func TestAfterATextTurnAPreExpirySummaryRunsInItsWindow(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>at the extra slot</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	twoPlainPings(t, k, fs, clock, model)

	k.Sweep(clock.advance(269 * time.Second))
	if model.calls.Load() != 0 {
		t.Fatalf("the summary ran %d times before its slot (270 s after the last ping)", model.calls.Load())
	}
	k.Sweep(clock.advance(1 * time.Second))
	waitPings(t, k, 3)
	if model.calls.Load() != 1 || k.summarySubstituted.Load() != 1 || fs.n() != 2 || k.Stats().SummaryExtraSent != 1 {
		t.Fatalf("slot: calls %d, substituted %d, plain %d, extra %d; want 1, 1, 2, 1",
			model.calls.Load(), k.summarySubstituted.Load(), fs.n(), k.Stats().SummaryExtraSent)
	}
	if k.summarySubstitutePhaseMismatch.Load() != 0 {
		t.Errorf("the summary's own phase gate refused at its slot (%d)", k.summarySubstitutePhaseMismatch.Load())
	}
	for i := 0; i < 3; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if model.calls.Load() != 1 || fs.n() != 2 {
		t.Errorf("after the summary: calls %d, plain %d; want 1 and 2 (nothing after it)", model.calls.Load(), fs.n())
	}
}

// #776 (a1): the hold is extended, once, only for a span that ended at a text turn and has a pre_expiry
// summary waiting. The hard-deadline callback (what the timer runs) is called by hand at the normal
// deadline: it must keep that entry and release every other shape.
func TestTheHoldIsExtendedOnlyForAWaitingPreExpirySummary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string // "" = no summary candidate
		body      string
		wantHeld  bool
		wantCount int64
	}{
		{"text turn, pre_expiry summary", "pre_expiry", kaBody, true, 1},
		{"text turn, any summary", "any", kaBody, false, 0},
		{"text turn, no summary", "", kaBody, false, 0},
		{"tool_result turn, pre_expiry summary", "pre_expiry", kaToolBody, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t)
			if tc.state != "" {
				kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}),
					&fakeSummaryModel{out: "<summary>s</summary>"}, tc.state, 0)
			}
			recordOne(t, k, kaPolicy(), tc.body, clock.now())
			k.Sweep(clock.advance(281 * time.Second))
			k.Sweep(clock.advance(281 * time.Second))
			waitPings(t, k, 2)
			clock.advance(280 * time.Second) // the normal hold deadline: the last ping + Idle
			k.expireAuto(sess1.id())         // what the hard-deadline timer runs
			if got := summaryHeld(k); got != tc.wantHeld {
				t.Errorf("held after the normal deadline = %v, want %v", got, tc.wantHeld)
			}
			if got := k.Stats().HoldExtensions; got != tc.wantCount {
				t.Errorf("hold extensions = %d, want %d", got, tc.wantCount)
			}
		})
	}
}

// #776 (a1): the boundary. The extended hold ends extraHoldMargin past the slot and never later; the
// second firing of the timer releases the entry whatever is still waiting. One extension only.
func TestTheExtendedHoldEndsAfterTheSlotAndOnlyOnce(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>s</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	twoPlainPings(t, k, fs, clock, model)
	lastPing := clock.now()

	clock.advance(280 * time.Second)
	k.expireAuto(sess1.id()) // the normal deadline: extended
	k.mu.Lock()
	e := k.live[sess1.id()]
	k.mu.Unlock()
	if e == nil {
		t.Fatal("released at the normal deadline although a pre_expiry summary was waiting")
	}
	if slot := e.extraSlot(true, 0); !slot.Equal(lastPing.Add(270 * time.Second)) {
		t.Errorf("slot = last ping + %v, want + 270s", slot.Sub(lastPing))
	}
	// The eager release in Sweep must leave it while the summary waits.
	k.mu.Lock()
	waiting := k.preExpiryExtraWaitingLocked(e)
	k.mu.Unlock()
	if !waiting {
		t.Fatal("the sweep's eager release would drop an extended hold before its slot")
	}
	// A second firing (the extended timer) releases, even with the summary still unsent.
	k.mu.Lock()
	until := e.timer != nil
	k.mu.Unlock()
	if !until {
		t.Fatal("the extension armed no timer")
	}
	k.expireAuto(sess1.id())
	if summaryHeld(k) {
		t.Error("the extended timer did not release: the hold may be extended only once")
	}
	if k.Stats().HoldExtensions != 1 {
		t.Errorf("extensions %d, want 1", k.Stats().HoldExtensions)
	}
}

// #776 (a1): the slot and the extended hold at the shipped 5-minute defaults (Idle 240 s, 2 pings):
// the summary goes out 268 s after the last ping (the middle of its 64 s window, 32 s before expiry)
// and the hold ends 15 s after that, 283 s after the last ping: 43 s past the normal deadline.
func TestTheExtraSlotAtTheShippedDefaults(t *testing.T) {
	last := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e := &kaEntry{pol: Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2}, startedAt: last,
		provider: bschemas.Anthropic}
	if got := e.extraSlot(true, 0).Sub(last); got != 268*time.Second {
		t.Errorf("pre_expiry slot = last ping + %v, want 268s", got)
	}
	if got := e.extraSlot(false, 0).Sub(last); got != 210*time.Second {
		t.Errorf("sweep / any slot = last ping + %v, want 210s", got)
	}
	if got := e.extraSlot(true, 0).Add(extraHoldMargin).Sub(last.Add(e.pol.Idle)); got != 43*time.Second {
		t.Errorf("the extension = %v past the normal deadline, want 43s", got)
	}
}

// #776 (a1): a cache_state any summary has no window, so it uses the sweep's slot (30 s before the
// normal hold deadline) and gets no extension.
func TestAfterATextTurnAnAnySummaryUsesTheSweepSlot(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>any</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "any", 0)
	twoPlainPings(t, k, fs, clock, model)
	k.Sweep(clock.advance(249 * time.Second))
	if model.calls.Load() != 0 {
		t.Fatalf("the any summary ran before its slot")
	}
	k.Sweep(clock.advance(1 * time.Second))
	waitPings(t, k, 3)
	if model.calls.Load() != 1 || fs.n() != 2 || k.Stats().HoldExtensions != 0 {
		t.Errorf("calls %d, plain %d, extensions %d; want 1, 2, 0", model.calls.Load(), fs.n(), k.Stats().HoldExtensions)
	}
}

// #776 (a1): a sweep candidate and a pre_expiry summary in the same span: ONE request in the extra
// slot, the summary (the claim's owner), at its own later slot. The sweep never asks after it.
func TestASweepAndAPreExpirySummarySendOneExtraRequest(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>s</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, store.NewMemory(store.Options{MaxEntries: 400}), asker, "claude-sonnet-5", sweepConversation(3))
	twoPlainPings(t, k, fs, clock, model)
	k.Sweep(clock.advance(250 * time.Second)) // the sweep's own slot: the summary owns the span, so not yet
	if asker.calls.Load() != 0 || model.calls.Load() != 0 {
		t.Fatalf("at the sweep's slot: asks %d, summary calls %d; want 0 and 0", asker.calls.Load(), model.calls.Load())
	}
	k.Sweep(clock.advance(20 * time.Second)) // the summary's slot (270 s)
	waitPings(t, k, 3)
	k.Sweep(clock.advance(10 * time.Second))
	if model.calls.Load() != 1 || asker.calls.Load() != 0 || fs.n() != 2 {
		t.Errorf("summary calls %d, asks %d, plain %d; want 1, 0, 2 (one extra request)",
			model.calls.Load(), asker.calls.Load(), fs.n())
	}
}

// #776: OpenAI has no text-turn rule (automatic prefix caching, no breakpoints), so a summary there
// still takes a planned ping, as on main.
func TestOnOpenAIASummaryStillTakesAPlannedPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>openai</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	pol := kaPolicy()
	pol.Idle = modelinfo.OpenAIDefaultKeepAliveIdle
	body := `{"model":"azure/gpt-5.6-luna","max_output_tokens":2048,"input":[{"role":"user","content":"hi"}]}`
	for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} {
		k.Record(Record{Key: sess1, StartedAt: at, Body: []byte(body), Provider: bschemas.OpenAI,
			Route: "/v1/responses", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(sess1)})
	}
	k.Sweep(clock.advance(modelinfo.OpenAIDefaultKeepAliveIdle + time.Second))
	waitPings(t, k, 1)
	if model.calls.Load() != 1 || fs.n() != 0 {
		t.Errorf("summary calls %d, plain %d; want 1 and 0", model.calls.Load(), fs.n())
	}
}

// #776 (a1): at the shipped Idle (240 s) the pre_expiry slot (268 s) falls after the normal deadline
// (240 s). The Sweep loop's eager release must leave that span between the two, so the summary still
// goes out at its slot; a span with no summary waiting is released at the same tick.
func TestTheEagerReleaseWaitsForAPreExpirySummary(t *testing.T) {
	for _, withSummary := range []bool{true, false} {
		t.Run(map[bool]string{true: "summary waiting", false: "nothing waiting"}[withSummary], func(t *testing.T) {
			eagerReleaseCase(t, withSummary)
		})
	}
}

func eagerReleaseCase(t *testing.T, withSummary bool) {
	{
		k, fs, clock := testKeeper(t)
		model := &fakeSummaryModel{out: "<summary>s</summary>"}
		if withSummary {
			kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
		}
		pol := kaPolicy()
		pol.Idle = 240 * time.Second
		recordOne(t, k, pol, kaBody, clock.now())
		k.Sweep(clock.advance(241 * time.Second))
		k.Sweep(clock.advance(241 * time.Second))
		waitPings(t, k, 2)
		k.Sweep(clock.advance(250 * time.Second)) // past the normal deadline, before the slot
		if got := summaryHeld(k); got != withSummary {
			t.Fatalf("summary waiting %v: held after the eager-release tick = %v, want %v", withSummary, got, withSummary)
		}
		if !withSummary {
			return
		}
		k.Sweep(clock.advance(18 * time.Second)) // the slot, 268 s after the last ping
		waitPings(t, k, 3)
		if model.calls.Load() != 1 || fs.n() != 2 {
			t.Errorf("calls %d, plain %d; want 1 and 2", model.calls.Load(), fs.n())
		}
	}
}

// #776 (a1): the extended hold never runs past the cache's own expiry. With pre_expiry_seconds 20 the
// window is 20 s and the slot 290 s after the last ping, so slot + 15 s would be 305 s: it is capped at
// the expiry, 300 s.
func TestTheExtendedHoldNeverPassesTheCacheExpiry(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>s</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 20)
	twoPlainPings(t, k, fs, clock, model)
	lastPing := clock.now()
	clock.advance(280 * time.Second)
	k.expireAuto(sess1.id())
	k.mu.Lock()
	e := k.live[sess1.id()]
	k.mu.Unlock()
	if e == nil {
		t.Fatal("not extended")
	}
	k.mu.Lock()
	until := e.holdUntil
	k.mu.Unlock()
	if got := until.Sub(lastPing); got != 300*time.Second {
		t.Errorf("the extended hold ends at last ping + %v, want 300s (the cache expiry)", got)
	}
}

// #776 (a1): a pre_expiry summary that declines at its slot leaves nothing else: the sweep never asks
// after it, because the sweep's own slot has passed (one request in the extra slot, nothing after).
func TestADecliningPreExpirySummaryIsNotFollowedByTheSweep(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{err: errSummaryDown}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, store.NewMemory(store.Options{MaxEntries: 400}), asker, "claude-sonnet-5", sweepConversation(3))
	twoPlainPings(t, k, fs, clock, model)
	k.Sweep(clock.advance(270 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.Stats().SweepExtraSkipped == 0 {
		time.Sleep(time.Millisecond)
	}
	k.Sweep(clock.advance(5 * time.Second))
	if model.calls.Load() != 1 || asker.calls.Load() != 0 || fs.n() != 2 {
		t.Errorf("summary calls %d, asks %d, plain %d; want 1, 0, 2", model.calls.Load(), asker.calls.Load(), fs.n())
	}
}

// #776 (a1): only a pre_expiry summary holds a span past the normal deadline. An any summary still unsent
// at the deadline (its slot was missed: a real request was in flight) is released there, as before.
func TestAnUnsentAnySummaryDoesNotHoldTheSpan(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>s</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "any", 0)
	twoPlainPings(t, k, fs, clock, model)
	k.mu.Lock()
	k.inflight[sess1.id()]++
	k.mu.Unlock()
	k.Sweep(clock.advance(250 * time.Second)) // the slot, missed: a request is in flight
	k.mu.Lock()
	k.inflight[sess1.id()]--
	k.mu.Unlock()
	k.Sweep(clock.advance(31 * time.Second)) // past the normal deadline
	if summaryHeld(k) {
		t.Error("an unsent any summary held the span past the normal deadline")
	}
}

var errSummaryDown = errors.New("summary model down")
