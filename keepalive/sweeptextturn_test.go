package keepalive

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// #776: which recorded shape lets the sweep's question stand in for a planned ping. Roles and block
// types only; a trailing system reminder is skipped.
func TestEndsAtTextTurn(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"tool_result then a system reminder", kaToolBody, false},
		{"user text", kaBody, true},
		{"tool_result without a reminder", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]}]}`, false},
		{"tool_result then text in the same turn", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"},{"type":"text","text":"x"}]}]}`, true},
		{"user string content", `{"messages":[{"role":"user","content":"x"},{"role":"system","content":"r"}]}`, true},
		{"assistant last", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]},{"role":"assistant","content":"x"}]}`, true},
		{"no messages", `{}`, true},
	} {
		if got := endsAtTextTurn([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: endsAtTextTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// #776, the owner's rule. After a text turn, both strategy pings are plain, and the question goes out
// once at the slot after the last one (here 3 x 281 s - 30 s), with nothing after it.
func TestAfterATextTurnBothPingsArePlainAndTheQuestionComesThird(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordOne(t, k, kaPolicy(), kaBody, clock.now())

	for i := int64(1); i <= 2; i++ {
		k.Sweep(clock.advance(281 * time.Second))
		waitPings(t, k, i)
		if asker.calls.Load() != 0 || fs.n() != int(i) {
			t.Fatalf("ping %d: asks %d, plain %d; want 0 and %d (every strategy ping is plain after a text turn)",
				i, asker.calls.Load(), fs.n(), i)
		}
	}
	// Not yet: the slot is Idle - 30 s after ping 2 (kaPolicy's Idle is 280 s, so 250 s).
	if n := k.Sweep(clock.advance(249 * time.Second)); n != 0 {
		t.Fatalf("the extra question fired %d jobs early", n)
	}
	if n := k.Sweep(clock.advance(1 * time.Second)); n != 1 {
		t.Fatalf("the extra slot fired %d jobs, want 1", n)
	}
	waitPings(t, k, 3)
	if asker.calls.Load() != 1 || fs.n() != 2 || k.Stats().SweepExtraAsked != 1 {
		t.Fatalf("slot 3: asks %d, plain %d, extra %d; want 1, 2, 1", asker.calls.Load(), fs.n(), k.Stats().SweepExtraAsked)
	}
	// Nothing after it: no ping, no second question, and the entry is released at the deadline.
	for i := 0; i < 3; i++ {
		k.Sweep(clock.advance(281 * time.Second))
	}
	if asker.calls.Load() != 1 || fs.n() != 2 {
		t.Errorf("after the question: asks %d, plain %d; want 1 and 2", asker.calls.Load(), fs.n())
	}
	if k.Stats().Live != 0 {
		t.Errorf("live entries %d after the deadline, want 0", k.Stats().Live)
	}
}

// #776: a one-ping plan after a text turn sends its plain ping, then the question one slot later.
func TestAfterATextTurnAOnePingPlanPingsThenAsks(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	pol := kaPolicy()
	pol.MaxPings = 1
	recordOne(t, k, pol, kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("ping 1: asks %d, plain %d; want 0 and 1", asker.calls.Load(), fs.n())
	}
	k.Sweep(clock.advance(255 * time.Second))
	waitPings(t, k, 2)
	if asker.calls.Load() != 1 || fs.n() != 1 {
		t.Fatalf("slot 2: asks %d, plain %d; want 1 and 1", asker.calls.Load(), fs.n())
	}
}

// #776: after a tool_result turn the question renews the cache (#777), so it may take ping 1, as on main.
func TestAfterAToolResultTurnTheQuestionTakesPingOne(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordOne(t, k, kaPolicy(), kaToolBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fs)
	waitPings(t, k, 1)
	if asker.calls.Load() != 1 || fs.n() != 0 {
		t.Fatalf("ping 1: asks %d, plain %d; want 1 and 0", asker.calls.Load(), fs.n())
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	k.Sweep(clock.advance(255 * time.Second))
	if asker.calls.Load() != 1 || fs.n() != 1 || k.Stats().SweepExtraAsked != 0 {
		t.Errorf("asks %d, plain %d, extra %d; want 1, 1, 0 (no extra slot after a tool_result turn)",
			asker.calls.Load(), fs.n(), k.Stats().SweepExtraAsked)
	}
}

// #776: the copy is released before the extra slot (the session is retired), so no question goes out.
func TestNoExtraQuestionWhenTheCopyIsReleased(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	k.RetireSession(sess1.Tenant, sess1.Session)
	k.Sweep(clock.advance(255 * time.Second))
	if asker.calls.Load() != 0 || fs.n() != 2 {
		t.Errorf("asks %d, plain %d; want 0 and 2", asker.calls.Load(), fs.n())
	}
}

// #776: with no sweep registration the extra slot sends nothing, and claims nothing.
func TestNoExtraRequestWithoutASweepCandidate(t *testing.T) {
	k, fs, clock := testKeeper(t)
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	if n := k.Sweep(clock.advance(255 * time.Second)); n != 0 {
		t.Errorf("the extra slot fired %d jobs with no candidate, want 0", n)
	}
	if fs.n() != 2 || k.Stats().SweepExtraSkipped != 0 {
		t.Errorf("plain %d, extra skipped %d; want 2 and 0", fs.n(), k.Stats().SweepExtraSkipped)
	}
}

// #776: after a text turn a planned ping runs no sweep lookup at all (it never counts sweep_no_candidate).
func TestAfterATextTurnAPlannedPingRunsNoSweepLookup(t *testing.T) {
	k, fs, clock := testKeeper(t)
	before := k.Stats().SweepNoCandidate
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	if got := k.Stats().SweepNoCandidate - before; got != 0 || fs.n() != 2 {
		t.Errorf("sweep no-candidate lookups %d, plain %d; want 0 and 2", got, fs.n())
	}
}

// #776: a span the keeper stopped (a ping wrote instead of reading) sends no extra question.
func TestNoExtraQuestionOnAStoppedSpan(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fs.mu.Lock()
	fs.resp = func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 100, CacheWrite: 48576, Output: 1}, 200, nil
	}
	fs.mu.Unlock()
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	pol := kaPolicy()
	pol.MaxPings = 1
	recordOne(t, k, pol, kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	k.Sweep(clock.advance(255 * time.Second))
	if asker.calls.Load() != 0 {
		t.Errorf("asks %d after a ping that wrote, want 0", asker.calls.Load())
	}
}
