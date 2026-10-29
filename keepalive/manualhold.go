package keepalive

import (
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
)

// The manual-only hold. The Ping button (manual.go) needs the thread's body and the caller's
// credential, and the keeper keeps those only while automatic keep-alive is on for the account.
// With it off, 60% of the idle gaps measured on CG2 had nothing held, so most presses were
// refused. Holding a body and a credential for everyone is not the answer: the caller has not
// agreed to it, and memory is what the OOM was about (#639).
//
// So the host passes the caller's opt-in per request (Policy.ManualHold; the proxy reads the
// x-context-guru-ping header, which the plugin adds to Claude Code's requests). A request that
// asks is held like any other, masked and with the credential held by the host, with three
// differences:
//
//   - NOTHING IS EVER PINGED for it. The entry has no schedule, no plan and no pending ping, so
//     it cannot spend money and cannot keep an idle proxy awake.
//   - It lives ManualHoldMax from the request's START, then it is released and zeroized. That is
//     the provider's own lifetime for a 5-minute entry, so after it the cache is cold and a press
//     would answer "cold" anyway. A thread billed at the 1-hour tier lives ManualHoldMax1h
//     (59 minutes) instead: capped to 300 s the button could never work on it. The longer a
//     credential is held, the more the caller has to trust, so this is still only for a request
//     that opted in, and still never extended by a press.
//   - A new request on the thread replaces the entry and restarts the 300 s. A press that READ the
//     cache refreshed the provider's entry, so it restarts it too.
//
// The existing bounds apply unchanged: MaxBodyBytes per body, MaxSessions entries, MaxBytes in all.

// ManualHoldMax is how long a manual-only hold lives, from the start of the last REAL request on the
// thread. Nothing else extends it: not a press (a press every 30 s would otherwise hold a caller's
// credential for as long as they keep pressing), not a ping. So the most a credential is held for the
// button is ManualHoldMax after the last real request.
const ManualHoldMax = 300 * time.Second

// The caps on manual-only holds PER TENANT. The global caps (MaxSessions, MaxBytes) are shared, and a
// header any client can send must not let one tenant fill them with many session ids and starve
// everyone else's keep-alive. Past either cap, that tenant's OLDEST manual-only hold goes first. 16
// threads covers a busy session and its sub-agents; 32 MiB is a few large contexts.
const (
	ManualHoldMaxPerTenant      = 16
	ManualHoldMaxBytesPerTenant = 32 << 20
)

// Why a thread cannot be pinged, for the button to say. Pingable is the other half of the answer.
const (
	ReasonNotOptedIn  = "not opted in" // no automatic keep-alive and the request did not ask to be held
	ReasonTooLarge    = "too large"    // the body is over MaxBodyBytes
	ReasonExpired     = "expired"      // it was held, and the hold ran out
	ReasonOverBudget  = "over budget"  // one ping would cost more than the per-ping ceiling
	ReasonUnsupported = "unsupported"  // a shape that cannot be pinged cheaply (thinking enabled, an unsupported route)
	// The reasons on an account whose automatic keep-alive is ON (issue #662). None of these is
	// "not opted in": that stays for a request that carried no opt-in and met no automatic policy.
	ReasonPingsUsed   = "pings used"   // the planned pings ran out and the keeper let go of the thread
	ReasonStopped     = "stopped"      // the keeper stopped the thread after a miss, a refused ping or a failure
	ReasonBelowLimits = "below limits" // the automatic gates refused it: first request, under the minimum prefix, or the predictor
	ReasonNoEntry     = "no entry"     // nothing is known about the thread: a restart, or no request since
	ReasonUnavailable = "unavailable"  // the route is failing in a row, or the provider refused this credential
)

// PingState says whether a thread can be pinged right now, and if not, why.
type PingState struct {
	Pingable bool
	Reason   string
}

// PingStateMap is one session's threads ("" = main). A thread with no entry was never held.
type PingStateMap map[string]PingState

// For is the thread's state; a thread nothing is known about has "no entry".
func (m PingStateMap) For(thread string) PingState {
	if s, ok := m[thread]; ok {
		return s
	}
	return PingState{Reason: ReasonNoEntry}
}

// refusal remembers why a thread was not held, until the next request on it.
type refusal struct {
	key    Key
	reason string
	at     time.Time
}

// maxReasons bounds the refusal memory. It holds a short string per thread; when it is full the
// oldest half-hour of entries goes, and if that is not enough it starts over.
const maxReasons = MaxTurnKeys

func (k *Keeper) setReasonLocked(key Key, reason string) {
	if k.reasons == nil {
		k.reasons = map[string]refusal{}
	}
	if len(k.reasons) >= maxReasons {
		cut := k.now().Add(-30 * time.Minute)
		for key, r := range k.reasons {
			if r.at.Before(cut) {
				delete(k.reasons, key)
			}
		}
		if len(k.reasons) >= maxReasons {
			k.reasons = map[string]refusal{}
		}
	}
	k.reasons[key.id()] = refusal{key: key, reason: reason, at: k.now()}
}

func (k *Keeper) clearReasonLocked(key Key) { delete(k.reasons, key.id()) }

// manualExpired reports whether a manual-only entry is past its deadline.
func (e *kaEntry) manualExpired(now time.Time) bool {
	return e.manualOnly && !now.Before(e.startedAt.Add(e.holdFor))
}

// expireManual is the entry's timer: the guarantee that a quiet process still releases the hold on
// time, with no sweep and no request. It checks the real deadline, because a press moves it.
func (k *Keeper) expireManual(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.live[key]
	if !ok || !e.manualExpired(k.now()) {
		return
	}
	k.setReasonLocked(e.key, ReasonExpired)
	k.retireLocked(key)
}

// PingStates says, per thread of one session, whether a manual ping can work now and, when not,
// why. all drops the tenant filter, as Plans does. It is PingNow's own "held" test, so a client
// that hides the button on Pingable=false never meets ErrNotHeld. nil means the keeper cannot say.
func (k *Keeper) PingStates(tenant string, all bool, session string) PingStateMap {
	if k == nil || session == "" {
		return nil
	}
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	out := PingStateMap{} // non-nil even when empty: "nothing is held" is an answer
	for _, e := range k.live {
		if e.session != session || (!all && e.tenant != tenant) {
			continue
		}
		switch {
		// A stopped entry still holds the body and key until its hold ends, and it plans no ping, so a
		// press can work and is the only thing left to keep the cache warm (#771).
		case e.manualExpired(now):
			out[e.thread] = PingState{Reason: ReasonExpired}
		default:
			out[e.thread] = PingState{Pingable: true}
		}
	}
	for _, r := range k.reasons {
		if r.key.Session != session || (!all && r.key.Tenant != tenant) {
			continue
		}
		if _, held := out[r.key.Thread]; !held {
			out[r.key.Thread] = PingState{Reason: r.reason}
		}
	}
	return out
}

// entryCeiling is the per-ping ceiling this entry answers to: its policy's, or the one a manual-only
// entry was admitted under.
func (e *kaEntry) entryCeiling() float64 {
	if e.manualOnly {
		return e.ceiling
	}
	return e.pol.Ceiling()
}

// autoReleaseReason is why the keeper is letting go of an AUTOMATIC entry on its own: stopped by a
// miss or a refusal, its planned pings spent, or no longer passing the gates. Called with k.mu held.
func (e *kaEntry) autoReleaseReason() string {
	switch {
	case e.stopped:
		return ReasonStopped
	case e.pol.MaxPings > 0 && e.pings >= e.pol.MaxPings:
		return ReasonPingsUsed
	default:
		return ReasonBelowLimits
	}
}

// expireAuto is the hard-deadline timer of an automatic entry: it records why before it lets go.
func (k *Keeper) expireAuto(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.live[key]; ok {
		k.setReasonLocked(e.key, e.autoReleaseReason())
	}
	k.retireLocked(key)
}

// evictManualLocked enforces the per-tenant manual-only caps after e was added: while the tenant holds
// too many entries or bytes in manual-only holds, its oldest one (by request start) is released.
func (k *Keeper) evictManualLocked(tenant string) {
	for {
		var n int
		var bytes int64
		var oldest *kaEntry
		var oldestKey string
		for key, e := range k.live {
			if !e.manualOnly || e.tenant != tenant {
				continue
			}
			n++
			bytes += int64(len(e.body))
			if oldest == nil || e.startedAt.Before(oldest.startedAt) {
				oldest, oldestKey = e, key
			}
		}
		if oldest == nil || (n <= ManualHoldMaxPerTenant && bytes <= ManualHoldMaxBytesPerTenant) {
			return
		}
		k.setReasonLocked(oldest.key, ReasonNoEntry)
		k.bytes -= int64(len(oldest.body))
		k.releaseLocked(oldest)
		oldest.clear()
		delete(k.live, oldestKey)
		offload.ClearKeepAliveCandidate(thread.Key(oldest.session, oldest.thread))
		k.skipped.Add(1)
	}
}

// dropReasonsLocked forgets the refusal reasons of one tenant (session == "": all of them) or of one session.
func (k *Keeper) dropReasonsLocked(tenant, session string) {
	for id, r := range k.reasons {
		if r.key.Tenant == tenant && (session == "" || r.key.Session == session) {
			delete(k.reasons, id)
		}
	}
}

// holdMax is the manual-only hold's length for a five-minute thread: ManualHoldMax unless a test
// shortened it.
func (k *Keeper) holdMax() time.Duration {
	if k.manualHoldMax > 0 {
		return k.manualHoldMax
	}
	return ManualHoldMax
}

// ManualHoldMax1h is the manual-only hold of a thread billed at the one-hour tier: the same
// 59 minutes the schedule pings at. At 300 s the button would never work on a one-hour thread: its
// cache is still warm then and a press is pointless, and by the time a press matters (near the
// hour) the hold would be long gone. It is still measured from the last REAL request, and a press
// does not extend it.
const ManualHoldMax1h = modelinfo.AnthropicDefaultKeepAliveIdle1h

// holdFor is the manual-only hold's length for a thread of the given tier. A test that shortened the
// hold shortens both.
func (k *Keeper) holdFor(ttl1h bool) time.Duration {
	if ttl1h && k.manualHoldMax == 0 {
		return ManualHoldMax1h
	}
	return k.holdMax()
}
