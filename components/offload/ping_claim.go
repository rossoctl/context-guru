package offload

import (
	"sync"
	"sync/atomic"

	"github.com/rossoctl/context-guru/components"
)

// One ping, one substitute call (#404 point 10).
//
// A due keep-alive ping can be replaced by cache_aware_summarizer's call or by extract_llm_sweep's
// question. Each reads the cached prefix, so each refreshes the cache. Only one of them may run at a
// ping. ClaimPing is the single choice and the single guard for both: the keeper claims the thread
// once per ping, runs what the claim names, and releases it.
//
// The rule, per thread:
//   - The summarizer registered a candidate. Its trigger fired on the thread's last request, so the
//     summary owns the ping. The sweep does not ask, even if the summary then declines or fails (the
//     ordinary ping goes out). Counted as sweep_summary_owned_ping: the summarizer had a candidate, so it
//     owned the ping, and the sweep did not ask.
//   - Only the sweep registered a candidate. The sweep may ask.
//   - A summary is already running for the thread (the detached call). The sweep does not ask: both
//     would read and rewrite the same thread at once. Counted (sweep_summary_in_flight).
//   - The thread is already claimed (a slow call from an earlier ping). Plain ping, counted as
//     sweep_ping_thread_busy.
//
// WHAT THE GUARD DOES NOT COVER. The claim is exclusive between pings, and the in-flight check is a
// look at the summary's single-flight registry, not a hold on it: check, then act. A detached summary
// that a real request starts AFTER the look can overlap the sweep's question. The summary's own
// single-flight and the sweep's asked flag still protect the thread from two of the same kind. Closing
// this window means taking the summary's flight in ClaimPing: PR C (the cold apply) must decide that.

// PingChoice names what may stand in for one ping.
type PingChoice string

const (
	// PingPlain: send the ordinary ping, and try no substitute. The thread is busy.
	PingPlain PingChoice = "plain"
	// PingNone: neither component registered for the thread. The keeper still looks each one up, so
	// its no-candidate counters keep counting. Both lookups decline.
	PingNone PingChoice = "none"
	// PingSummary: cache_aware_summarizer's call may take the ping.
	PingSummary PingChoice = "summary"
	// PingSweep: extract_llm_sweep's question may take the ping.
	PingSweep PingChoice = "sweep"
)

var (
	pingGuardMu   sync.Mutex
	pingGuardBusy = map[string]struct{}{}

	sweepSummaryOwnedPing atomic.Int64
	sweepSummaryInFlight  atomic.Int64
	sweepPingThreadBusy   atomic.Int64
)

// PingClaim is the result of ClaimPing. The caller must call Release when the substitute attempt is
// over, whatever its outcome.
type PingClaim struct {
	Choice PingChoice
	key    string
	held   bool
}

// Release frees the thread for the next ping. Safe to call more than once, and on a plain claim.
func (c *PingClaim) Release() {
	if !c.held {
		return
	}
	c.held = false
	pingGuardMu.Lock()
	delete(pingGuardBusy, c.key)
	pingGuardMu.Unlock()
}

// HasSweepPingCandidate reports whether the sweep holds a registration for threadKey. The keeper reads
// it before an extra sweep question (#776), which goes out only when there is something to ask.
func HasSweepPingCandidate(threadKey string) bool {
	sweepPingMu.Lock()
	defer sweepPingMu.Unlock()
	_, ok := sweepPingCand[threadKey]
	return ok
}

// ClaimPing decides, for threadKey (thread.Key(session, thread)), what takes this ping, and holds the
// thread until Release. A plain claim holds nothing.
func ClaimPing(threadKey string) PingClaim {
	pingGuardMu.Lock()
	if _, busy := pingGuardBusy[threadKey]; busy {
		pingGuardMu.Unlock()
		sweepPingThreadBusy.Add(1)
		return PingClaim{Choice: PingPlain}
	}
	pingGuardBusy[threadKey] = struct{}{}
	pingGuardMu.Unlock()
	claim := PingClaim{key: threadKey, held: true}

	keepAliveCandMu.Lock()
	_, hasSummary := keepAliveCand[threadKey]
	keepAliveCandMu.Unlock()
	sweepPingMu.Lock()
	_, hasSweep := sweepPingCand[threadKey]
	sweepPingMu.Unlock()

	switch {
	case hasSummary:
		claim.Choice = PingSummary
		if hasSweep {
			sweepSummaryOwnedPing.Add(1)
		}
	case hasSweep:
		if _, running := inFlight.peek(threadKey); running {
			sweepSummaryInFlight.Add(1)
			claim.Choice = PingPlain
			break
		}
		claim.Choice = PingSweep
	default:
		claim.Choice = PingNone
	}
	if claim.Choice == PingPlain || claim.Choice == PingNone {
		claim.Release()
	}
	return claim
}

// ColdApplyPhase is the shared answer to "may a held result be applied to this request?" (#404). The
// summary reserve graduates on it (cacheAwareApplyPhase), and the sweep's cold apply must use the same
// test: a cold or untracked cache, where the rewrite is already paid for. PR C calls this.
func ColdApplyPhase(p components.CachePhase) bool { return cacheAwareApplyPhase(p) }
