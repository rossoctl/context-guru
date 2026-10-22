package keepalive

import "time"

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
//     would answer "cold" anyway. A 1-hour entry is capped to the same 300 s on purpose: the
//     longer a credential is held, the more the caller has to trust.
//   - A new request on the thread replaces the entry and restarts the 300 s. A press that READ the
//     cache refreshed the provider's entry, so it restarts it too.
//
// The existing bounds apply unchanged: MaxBodyBytes per body, MaxSessions entries, MaxBytes in all.

// ManualHoldMax is how long a manual-only hold lives, from the start of the request that made it.
const ManualHoldMax = 300 * time.Second

// Why a thread cannot be pinged, for the button to say. Pingable is the other half of the answer.
const (
	ReasonNotOptedIn  = "not opted in" // no automatic keep-alive and the request did not ask to be held
	ReasonTooLarge    = "too large"    // the body is over MaxBodyBytes
	ReasonExpired     = "expired"      // it was held, and the hold ran out
	ReasonOverBudget  = "over budget"  // one ping would cost more than the per-ping ceiling
	ReasonUnsupported = "unsupported"  // a shape that cannot be pinged cheaply (thinking enabled, an unsupported route)
)

// PingState says whether a thread can be pinged right now, and if not, why.
type PingState struct {
	Pingable bool
	Reason   string
}

// PingStateMap is one session's threads ("" = main). A thread with no entry was never held.
type PingStateMap map[string]PingState

// For is the thread's state; a thread nothing is known about is "not opted in".
func (m PingStateMap) For(thread string) PingState {
	if s, ok := m[thread]; ok {
		return s
	}
	return PingState{Reason: ReasonNotOptedIn}
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
	return e.manualOnly && !now.Before(e.startedAt.Add(ManualHoldMax))
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
		case e.stopped:
			out[e.thread] = PingState{Reason: ReasonNotOptedIn}
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
