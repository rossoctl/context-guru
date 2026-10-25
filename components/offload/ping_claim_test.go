package offload

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

func withCandidates(t *testing.T, key string, summary, sweep bool) {
	t.Helper()
	if summary {
		keepAliveCandMu.Lock()
		keepAliveCand[key] = &keepAliveCandidate{}
		keepAliveCandMu.Unlock()
	}
	if sweep {
		sweepPingMu.Lock()
		sweepPingCand[key] = &sweepPingCandidate{}
		sweepPingMu.Unlock()
	}
	t.Cleanup(func() {
		keepAliveCandMu.Lock()
		delete(keepAliveCand, key)
		keepAliveCandMu.Unlock()
		sweepPingMu.Lock()
		delete(sweepPingCand, key)
		sweepPingMu.Unlock()
	})
}

func TestClaimPingChoosesPerThread(t *testing.T) {
	for _, tc := range []struct {
		name           string
		summary, sweep bool
		want           PingChoice
		tookPing       int64
	}{
		{"both registered: the summary owns the ping", true, true, PingSummary, 1},
		{"summary only", true, false, PingSummary, 0},
		{"sweep only: the sweep may ask", false, true, PingSweep, 0},
		{"nothing registered", false, false, PingNone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "claim-" + tc.name
			withCandidates(t, key, tc.summary, tc.sweep)
			before := sweepSummaryOwnedPing.Load()
			c := ClaimPing(key)
			defer c.Release()
			if c.Choice != tc.want {
				t.Errorf("choice = %q, want %q", c.Choice, tc.want)
			}
			if got := sweepSummaryOwnedPing.Load() - before; got != tc.tookPing {
				t.Errorf("sweep_summary_owned_ping rose by %d, want %d", got, tc.tookPing)
			}
		})
	}
}

func TestClaimPingGivesTheSweepNothingWhileASummaryRuns(t *testing.T) {
	key := "claim-inflight"
	withCandidates(t, key, false, true)
	j, ok := inFlight.begin(key)
	if !ok {
		t.Fatal("could not start the flight")
	}
	defer inFlight.finish(key, j)
	before := sweepSummaryInFlight.Load()
	c := ClaimPing(key)
	defer c.Release()
	if c.Choice != PingPlain {
		t.Errorf("choice = %q while a summary runs on the thread, want plain", c.Choice)
	}
	if sweepSummaryInFlight.Load()-before != 1 {
		t.Error("sweep_summary_in_flight was not counted")
	}
}

// A thread is claimed once: a second claim before Release is plain, and Release frees it.
func TestClaimPingAllowsOneHolderPerThread(t *testing.T) {
	key := "claim-single"
	withCandidates(t, key, false, true)
	first := ClaimPing(key)
	if first.Choice != PingSweep {
		t.Fatalf("first choice = %q", first.Choice)
	}
	busyBefore := sweepPingThreadBusy.Load()
	if second := ClaimPing(key); second.Choice != PingPlain {
		t.Errorf("second claim while held = %q, want plain", second.Choice)
	}
	if sweepPingThreadBusy.Load()-busyBefore != 1 {
		t.Error("sweep_ping_thread_busy was not counted")
	}
	first.Release()
	third := ClaimPing(key)
	defer third.Release()
	if third.Choice != PingSweep {
		t.Errorf("claim after release = %q, want sweep", third.Choice)
	}
}

// Many claimers racing for one thread: exactly one wins a non-plain choice.
func TestClaimPingRaceHasOneWinner(t *testing.T) {
	key := "claim-race"
	withCandidates(t, key, true, true)
	var wins, claimed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	release := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := ClaimPing(key)
			if c.Choice != PingPlain {
				wins.Add(1)
			}
			claimed.Add(1)
			<-release
			c.Release()
		}()
	}
	close(start)
	for claimed.Load() < 16 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d claimers won the thread at once, want 1", wins.Load())
	}
}

func TestColdApplyPhaseIsTheSummaryReservesRule(t *testing.T) {
	for p, want := range map[components.CachePhase]bool{
		components.CachePhaseCold: true, components.CachePhaseUnknown: true,
		components.CachePhaseWarm: false, components.CachePhasePreExpiry: false,
	} {
		if ColdApplyPhase(p) != want {
			t.Errorf("ColdApplyPhase(%v) = %v, want %v", p, !want, want)
		}
	}
}
