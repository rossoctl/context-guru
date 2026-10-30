package keepalive

import (
	"errors"
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
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

// #776: after a text turn a pre_expiry summary does not take a planned ping. It goes out once, as the
// extra request at the same slot as the sweep, extraSlotMargin before the normal hold deadline. At the
// shipped Idle (240 s) that is 210 s after the last ping, 90 s before expiry: outside the planned-ping
// window (64 s), so only the extra request's own wider window lets it run. Nothing after it.
func TestAfterATextTurnAPreExpirySummaryRunsAtTheExtraSlot(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>at the extra slot</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	pol := kaPolicy()
	pol.Idle = 240 * time.Second
	recordOne(t, k, pol, kaBody, clock.now())
	for i := int64(1); i <= 2; i++ {
		k.Sweep(clock.advance(241 * time.Second))
		waitPings(t, k, i)
	}
	if model.calls.Load() != 0 || fs.n() != 2 {
		t.Fatalf("planned pings: summary calls %d, plain %d; want 0 and 2", model.calls.Load(), fs.n())
	}

	k.Sweep(clock.advance(209 * time.Second))
	if model.calls.Load() != 0 {
		t.Fatalf("the summary ran %d times before its slot (210 s after the last ping)", model.calls.Load())
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

// #776, at the shipped 5-minute defaults (Idle 240 s, ttl 300 s): the extra slot is 210 s after the
// last ping, the cache then has 90 s left, and the extra summary's window (94 s) counts that as
// pre-expiry. The planned-ping window stays the 64 s #715/#717 set.
func TestTheExtraSlotAndWindowAtTheShippedDefaults(t *testing.T) {
	last := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e := &kaEntry{pol: Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2}, startedAt: last}
	if got := e.extraSlot().Sub(last); got != 210*time.Second {
		t.Errorf("extra slot = last ping + %v, want 210s", got)
	}
	ttl, idle := 300*time.Second, 240*time.Second
	planned := substituteWindow(0, ttl, idle)
	if planned != 64*time.Second {
		t.Errorf("planned-ping window = %v, want 64s (unchanged)", planned)
	}
	extra := extraSummaryWindow(planned, ttl, idle)
	if extra != 94*time.Second {
		t.Errorf("extra summary window = %v, want 94s", extra)
	}
	if got := cachePhaseAt(last, last.Add(210*time.Second), ttl, extra); got != components.CachePhasePreExpiry {
		t.Errorf("phase at the extra slot = %v, want pre_expiry", got)
	}
	if got := cachePhaseAt(last, last.Add(210*time.Second), ttl, planned); got != components.CachePhaseWarm {
		t.Errorf("phase at 210 s with the planned window = %v, want warm (the planned window is not widened)", got)
	}
	if got := extraSummaryWindow(200*time.Second, ttl, idle); got != 200*time.Second {
		t.Errorf("a wider configured window narrowed to %v", got)
	}
}

// #776: no span's hold limit changes. The hard-deadline callback (what the timer runs) is called by
// hand at the normal deadline, for every shape: it releases the entry each time.
func TestNoSpansHoldLimitChanges(t *testing.T) {
	for _, tc := range []struct {
		name, state, body string
	}{
		{"text turn, pre_expiry summary", "pre_expiry", kaBody},
		{"text turn, any summary", "any", kaBody},
		{"text turn, no summary", "", kaBody},
		{"tool_result turn, pre_expiry summary", "pre_expiry", kaToolBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t)
			if tc.state != "" {
				kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}),
					&fakeSummaryModel{err: errSummaryDown}, tc.state, 0)
			}
			recordOne(t, k, kaPolicy(), tc.body, clock.now())
			k.Sweep(clock.advance(281 * time.Second))
			k.Sweep(clock.advance(281 * time.Second))
			waitPings(t, k, 2)
			if e := k.live[sess1.id()]; e != nil && e.hold() != 3*kaPolicy().Idle {
				t.Errorf("hold = %v, want %v ((K+1) x X)", e.hold(), 3*kaPolicy().Idle)
			}
			clock.advance(280 * time.Second) // the normal hold deadline: the last ping + Idle
			k.expireAuto(sess1.id())
			if summaryHeld(k) {
				t.Error("held past the normal hold deadline")
			}
		})
	}
}

// #776: a summary that takes a PLANNED ping (after a tool_result turn) keeps the normal #715/#717
// window, not the extra slot's wider one. With pre_expiry_seconds 60, a ping 220 s after the turn (80 s
// left) is refused and one at 242 s (58 s left) is taken.
func TestAPlannedPingSummaryKeepsTheNormalWindow(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		want int64
	}{{219 * time.Second, 0}, {241 * time.Second, 1}} {
		t.Run(tc.wait.String(), func(t *testing.T) { plannedWindowCase(t, tc.wait, tc.want) })
	}
}

func plannedWindowCase(t *testing.T, wait time.Duration, want int64) {
	{
		tc := struct {
			wait time.Duration
			want int64
		}{wait, want}
		k, _, clock := testKeeper(t)
		model := &fakeSummaryModel{out: "<summary>s</summary>"}
		kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 60)
		pol := kaPolicy()
		pol.Idle = tc.wait
		recordOne(t, k, pol, kaToolBody, clock.now())
		k.Sweep(clock.advance(tc.wait + time.Second))
		waitPings(t, k, 1)
		if got := k.summarySubstituted.Load(); got != tc.want {
			t.Errorf("ping at %v: substituted %d, want %d", tc.wait+time.Second, got, tc.want)
		}
	}
}

// #776: a cache_state any summary uses the same slot.
func TestAfterATextTurnAnAnySummaryUsesTheExtraSlot(t *testing.T) {
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
	if model.calls.Load() != 1 || fs.n() != 2 {
		t.Errorf("calls %d, plain %d; want 1 and 2", model.calls.Load(), fs.n())
	}
}

// #776: a sweep candidate and a pre_expiry summary in the same span: ONE request, the summary.
func TestASweepAndAPreExpirySummarySendOneExtraRequest(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{out: "<summary>s</summary>"}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, store.NewMemory(store.Options{MaxEntries: 400}), asker, "claude-sonnet-5", sweepConversation(3))
	twoPlainPings(t, k, fs, clock, model)
	k.Sweep(clock.advance(250 * time.Second))
	waitPings(t, k, 3)
	k.Sweep(clock.advance(10 * time.Second))
	if model.calls.Load() != 1 || asker.calls.Load() != 0 || fs.n() != 2 {
		t.Errorf("summary calls %d, asks %d, plain %d; want 1, 0, 2 (one extra request)",
			model.calls.Load(), asker.calls.Load(), fs.n())
	}
}

// #776: a summary that declines in the extra slot leaves the same slot to the sweep question. Still one
// request goes out, and nothing after it.
func TestADecliningSummaryLeavesTheSlotToTheSweep(t *testing.T) {
	k, fs, clock := testKeeper(t)
	model := &fakeSummaryModel{err: errSummaryDown}
	kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, "pre_expiry", 0)
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, store.NewMemory(store.Options{MaxEntries: 400}), asker, "claude-sonnet-5", sweepConversation(3))
	twoPlainPings(t, k, fs, clock, model)
	k.Sweep(clock.advance(250 * time.Second))
	waitSweepOutcome(t, k, fs)
	for i := 0; i < 3; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if model.calls.Load() != 1 || asker.calls.Load() != 1 || fs.n() != 2 {
		t.Errorf("summary calls %d, asks %d, plain %d; want 1, 1, 2", model.calls.Load(), asker.calls.Load(), fs.n())
	}
}

// #776: a missed extra slot (a real request in flight) is never sent late, and the span is released on
// time.
func TestAMissedExtraSlotIsNotSentLate(t *testing.T) {
	for _, state := range []string{"any", "pre_expiry"} {
		t.Run(state, func(t *testing.T) {
			k, fs, clock := testKeeper(t)
			model := &fakeSummaryModel{out: "<summary>s</summary>"}
			kaCandidateWithCacheState(t, "sess-1", store.NewMemory(store.Options{MaxEntries: 400}), model, state, 0)
			twoPlainPings(t, k, fs, clock, model)
			k.mu.Lock()
			k.inflight[sess1.id()]++
			k.mu.Unlock()
			k.Sweep(clock.advance(250 * time.Second)) // the slot, missed: a request is in flight
			k.mu.Lock()
			k.inflight[sess1.id()]--
			k.mu.Unlock()
			k.Sweep(clock.advance(31 * time.Second)) // past the normal deadline
			if model.calls.Load() != 0 {
				t.Errorf("the summary went out %d times after its missed slot", model.calls.Load())
			}
			if summaryHeld(k) {
				t.Error("the span was held past the normal deadline")
			}
		})
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

var errSummaryDown = errors.New("summary model down")
