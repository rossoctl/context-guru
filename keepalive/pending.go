package keepalive

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/waitcause"
)

// Pending records (#1227).
//
// Record plans a turn from the open-task counts of the request before, so a turn that starts background
// work reads as a wait for a person. A person wait has the 80k prefix floor and the time windows, and a
// thread under that floor, or outside the windows, is not held at all. The dashboard writer reads the
// turn in order a moment later and calls RaiseWaitPlan with "background". To give that call something to
// promote, Record keeps such a thread as a pending record for one flush window instead of releasing it.
//
// A pending record is a kaEntry that is not in k.live: no schedule, not counted, not pinged. It holds
// what an entry holds, under the same rules: the keeper-owned masked body copy, the credential the host
// held for it (released exactly once), a timer that retires it. It lives PendingTTL (5 s by default), and
// at most MaxPending of them, MaxPendingBytes of body in all, exist at once. The next request of the
// thread (Arrive), a retire, a session end and Stop drop it. Nothing is written anywhere.

// pendLocked tries to keep e as a pending record instead of refusing it. e was built for the plan Record
// chose (a person wait); pendLocked re-plans it for background work and keeps it only if it would pass
// every gate then. It reports whether it kept e (the caller then returns without refusing). k.mu is held.
func (k *Keeper) pendLocked(r Record, e *kaEntry, pol Policy, planned bool, wait string, off bool) bool {
	id := r.Key.id()
	if !planned || wait != waitcause.WaitingHuman || e.manualOnly || pol.ManualHold || r.Provider == bschemas.OpenAI ||
		e.ttl1h || IsSubagentThread(e.thread) || len(e.body) == 0 {
		return false
	}
	if len(k.pending) >= MaxPending || k.pendBytes+int64(len(e.body)) > MaxPendingBytes {
		return false
	}
	// The policy a background wait would get: the wide one (no windows) when the host gave it.
	cp, tag := pol, e.tag
	cp.MaxPings = e.planBase // pol carries the person plan's count; the base is what WaitPings starts from
	if w := r.WidePolicy; w != nil && usableWide(*w) {
		cp, tag = *w, r.WideTag
	}
	if !usableWide(cp) {
		return false
	}
	base := cp.MaxPings
	cp.MaxPings = cp.WaitPings(waitcause.WaitingBackground, false)
	probe := *e
	probe.pol, probe.wait, probe.planBase, probe.planned, probe.tag = cp, waitcause.WaitingBackground, base, true, tag
	if !probe.pingable() {
		return false
	}
	*e = probe
	e.pendReason = ReasonBelowLimits
	if off {
		e.pendReason = ReasonNotOptedIn
	}
	if r.Hold != nil {
		e.ref = r.Hold()
		e.held = true
	}
	ttl := k.o.PendingTTL
	if ttl <= 0 {
		ttl = DefaultPendingTTL
	}
	e.armedAt = k.now()
	k.dropPendingLocked(id, false)
	k.pending[id] = e
	k.pendBytes += int64(len(e.body))
	e.timer = time.AfterFunc(ttl, func() { k.expirePending(id, e) })
	return true
}

// expirePending is the timer of a pending record: no report came, so it is refused as Record would
// have refused it. A record that was promoted or dropped meanwhile is not the one in the map: no-op.
func (k *Keeper) expirePending(id string, e *kaEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending[id] == e {
		k.dropPendingLocked(id, true)
	}
}

// dropPendingLocked releases the pending record of id: body zeroed, credential released, timer stopped.
// refuse: record the refusal Record would have made (reason, and the skipped count for a held-back
// automatic thread). k.mu must be held.
func (k *Keeper) dropPendingLocked(id string, refuse bool) {
	e, ok := k.pending[id]
	if !ok {
		return
	}
	delete(k.pending, id)
	k.pendBytes -= int64(len(e.body))
	k.releaseLocked(e)
	e.clear()
	if refuse {
		k.setReasonLocked(e.key, e.pendReason)
		if e.pendReason == ReasonBelowLimits {
			k.skipped.Add(1)
		}
	}
}

// promoteLocked turns the pending record of id into a live entry planned for cause, when cause is
// background work. The retention deadline counts from the record instant, as for an entry made at
// response time. It returns the new count and true when it promoted. k.mu must be held.
func (k *Keeper) promoteLocked(id, cause string) (int, bool) {
	e, ok := k.pending[id]
	if !ok || cause != waitcause.WaitingBackground || e.timer == nil || !e.timer.Stop() {
		return 0, false
	}
	delete(k.pending, id)
	k.pendBytes -= int64(len(e.body))
	if !k.now().Before(e.armedAt.Add(e.hold())) {
		k.releaseLocked(e)
		e.clear()
		return 0, false
	}
	e.timer = nil
	k.clearReasonLocked(e.key)
	k.live[id] = e
	k.bytes += int64(len(e.body))
	k.armHoldLocked(id, e, e.armedAt)
	k.evictLocked()
	return e.pol.MaxPings, true
}
