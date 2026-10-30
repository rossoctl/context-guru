package keepalive

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// #776: the sweep's question may take only the LAST planned ping, and only on a plan of 2 or more.
func TestSweepMayAskOnlyAtTheLastPingOfAPlanOfTwoOrMore(t *testing.T) {
	for _, tc := range []struct {
		ping, max int
		want      bool
	}{
		{1, 1, false}, // a one-ping plan: the question would be the only ping
		{1, 2, false}, {2, 2, true},
		{1, 3, false}, {2, 3, false}, {3, 3, true},
		{1, 0, false},
	} {
		if got := sweepMayAsk(tc.ping, tc.max); got != tc.want {
			t.Errorf("sweepMayAsk(ping %d, plan %d) = %v, want %v", tc.ping, tc.max, got, tc.want)
		}
	}
}

// #776: ping 1 is a plain ping even when the sweep has candidates, and the question takes ping 2.
// Before, the question took ping 1; after a turn that ends at a text turn it did not renew the
// turn's cache entry, and ping 2 wrote the whole conversation again.
func TestThePingOneIsPlainAndTheSweepTakesPingTwo(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordOne(t, k, kaPolicy(), kaBody, clock.now())

	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || k.sweepSubstituted.Load() != 0 || fs.n() != 1 {
		t.Fatalf("ping 1: asks %d, substituted %d, plain %d; want 0, 0, 1 (ping 1 is always plain)",
			asker.calls.Load(), k.sweepSubstituted.Load(), fs.n())
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fs)
	waitPings(t, k, 2)
	if asker.calls.Load() != 1 || k.sweepSubstituted.Load() != 1 || fs.n() != 1 {
		t.Fatalf("ping 2: asks %d, substituted %d, plain %d; want 1, 1, 1 (the question takes ping 2)",
			asker.calls.Load(), k.sweepSubstituted.Load(), fs.n())
	}
}

// #776: on a 3-ping plan the question takes ping 3, never 1 or 2.
func TestOnAThreePingPlanTheSweepTakesPingThree(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	pol := kaPolicy()
	pol.MaxPings = 3
	recordOne(t, k, pol, kaBody, clock.now())
	for i := int64(1); i <= 3; i++ {
		k.Sweep(clock.advance(281 * time.Second))
		waitPings(t, k, i)
		wantAsks := int64(0)
		if i == 3 {
			waitSweepOutcome(t, k, fs)
			wantAsks = 1
		}
		if got := asker.calls.Load(); got != wantAsks {
			t.Fatalf("after ping %d the question was asked %d times, want %d", i, got, wantAsks)
		}
	}
	if fs.n() != 2 || k.sweepSubstituted.Load() != 1 {
		t.Errorf("plain %d, substituted %d, want 2 and 1", fs.n(), k.sweepSubstituted.Load())
	}
}

// #776: a one-ping plan (the 1-hour tier's default) never asks. Its only ping would be the question,
// and after a text turn nothing would renew the entry, so the cache would end a lifetime early.
func TestAOnePingPlanNeverAsks(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	pol := kaPolicy()
	pol.MaxPings = 1
	recordOne(t, k, pol, kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || k.sweepSubstituted.Load() != 0 || fs.n() != 1 {
		t.Fatalf("asks %d, substituted %d, plain %d; want 0, 0, 1", asker.calls.Load(), k.sweepSubstituted.Load(), fs.n())
	}
}

// With no registration the sweep's lookup runs, and counts no_candidate, only where it may ask: once
// in a 2-ping span, on ping 2.
func TestTheSweepLooksForACandidateOnlyWhereItMayAsk(t *testing.T) {
	k, fs, clock := testKeeper(t)
	before := k.Stats().SweepNoCandidate
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if got := k.Stats().SweepNoCandidate - before; got != 0 {
		t.Errorf("ping 1 counted %d sweep no-candidate lookups, want 0", got)
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	if got := k.Stats().SweepNoCandidate - before; got != 1 {
		t.Errorf("the span counted %d sweep no-candidate lookups, want 1 (ping 2 only)", got)
	}
	if fs.n() != 2 {
		t.Errorf("plain pings %d, want 2", fs.n())
	}
}
