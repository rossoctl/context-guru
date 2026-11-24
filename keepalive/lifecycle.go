package keepalive

import (
	"strings"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/thread"
)

// Client lifecycle signals for the keep-alive.
//
// A client that runs Claude Code hooks can tell a host things the host cannot see in the
// request stream: that the user left (SessionEnd), that the conversation was compacted so its
// old cached prefix is dead (SessionStart source=compact), that the user is back (resume), and
// so on. This file is the keeper's half of that mechanism (main #527's proxy/clientsignal.go):
// MarkEnded / EndedAfter record and query an end mark, and RetireSessionStartedBefore (which
// RetireSession is now a special case of) drops the entries a signal makes stale. The decision
// of WHICH host events to call these for, and the CONTEXT_GURU_CLIENT_SIGNALS switch, stay with
// the host route — this package only does what it is told.
//
// # Failure safety
//
// Every signal only ever REMOVES spend: it retires a held entry or stops future pings. None
// extends a hold or arms a ping. A lost, late or forged call therefore costs at most what the
// no-signal policy would also have spent, and a missing call is the no-signal policy. A mark is
// also scoped to its own tenant (bucketed first by tenant in k.ended), so one tenant can never
// silence another's keep-alive.

const (
	// lifecycleTTL is how long a SessionEnd mark is remembered: past it the mark is dead
	// weight. Port of main's proxy/clientsignal.go lifecycleTTL (#527).
	lifecycleTTL = 12 * time.Hour
	// maxEndedPerTenant bounds one tenant's end marks. The cap is per tenant, not global, so
	// one tenant's churn cannot evict another tenant's marks. Port of main's
	// proxy/clientsignal.go maxEndedPerTenant (#527).
	maxEndedPerTenant = 2000
)

// MarkEnded records that session ended at `at`. Record then refuses any entry of that session
// (matched on Key.Session or Record.Client) whose request started before `at`.
func (k *Keeper) MarkEnded(tenant, session string, at time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.markEndedLocked(tenant, session, at)
}

// markEndedLocked records an end mark under the tenant's own bucket. At the cap it first drops
// marks older than lifecycleTTL, then the oldest, and only ever from this tenant's bucket. Port
// of main's keeper.markEndedLocked (proxy/clientsignal.go, #527). k.mu must be held.
func (k *Keeper) markEndedLocked(tenant, session string, now time.Time) {
	if k.ended == nil {
		k.ended = map[string]map[string]time.Time{}
	}
	b := k.ended[tenant]
	if b == nil {
		b = map[string]time.Time{}
		k.ended[tenant] = b
	}
	if _, ok := b[session]; !ok && len(b) >= maxEndedPerTenant {
		oldK, oldT := "", now
		for kk, t := range b {
			if now.Sub(t) > lifecycleTTL {
				delete(b, kk)
			} else if oldK == "" || t.Before(oldT) {
				oldK, oldT = kk, t
			}
		}
		if len(b) >= maxEndedPerTenant {
			delete(b, oldK)
		}
	}
	b[session] = now
}

// EndedAfter reports whether session (or a client alias) was marked ended at or after
// startedAt, i.e. the request that started at startedAt was in flight when the user left. Port
// of main's keeper.endedAfter (proxy/clientsignal.go, #527).
func (k *Keeper) EndedAfter(tenant, session string, startedAt time.Time) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.endedAfterLocked(tenant, session, startedAt)
}

// endedAfterLocked is EndedAfter's body, for Record which already holds k.mu. k.mu must be held.
func (k *Keeper) endedAfterLocked(tenant, session string, startedAt time.Time) bool {
	t, ok := k.ended[tenant][session]
	return ok && !startedAt.After(t)
}

// RetireSessionStartedBefore retires the session's entries (matched on Key.Session or
// Record.Client). A zero cutoff retires every thread, as RetireSession. A non-zero cutoff
// retires only the MAIN thread's entries ("" thread) that started before it — only the main
// agent compacts (its compaction call stays on the main thread), so a subagent's or teammate's
// held cache keeps its keep-alive. Returns how many entries were retired. Port of main's
// keeper.retireSessionStartedBefore (proxy/keepalive.go, #527).
func (k *Keeper) RetireSessionStartedBefore(tenant, session string, cutoff time.Time) int {
	if k == nil {
		return 0
	}
	base := Key{Tenant: tenant, Session: session}.id()
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for key, e := range k.live {
		own := key == base || strings.HasPrefix(key, base+"\x00") ||
			(e.client != "" && e.tenant == tenant && e.client == session)
		if !own || (!cutoff.IsZero() && (e.thread != "" || !e.startedAt.Before(cutoff))) {
			continue
		}
		k.bytes -= int64(len(e.body))
		k.releaseLocked(e)
		e.clear()
		delete(k.live, key)
		offload.ClearKeepAliveCandidate(thread.Key(e.session, e.thread))
		n++
	}
	if cutoff.IsZero() { // a full retire, not the lifecycle sweep: the reasons go with the entries
		k.dropReasonsLocked(tenant, session)
	}
	return n
}

// CompactRaceMargin bounds how late a SessionStart compact event may arrive after the request that
// follows the compaction started. Measured hook-to-proxy delivery is 0.2-0.8 s; the old prefix's
// last request (the compaction call itself) started tens of seconds earlier. Known limitation: a
// ping also moves startedAt, so an old-prefix entry pinged within the margin is not retired by the
// event and is pinged at most once more before its next idle check stops it.
const CompactRaceMargin = 2 * time.Second

// signalEvents is the closed set of accepted lifecycle events; anything else is refused, so a client
// cannot smuggle a free-text field through the signal route.
var signalEvents = map[string]bool{"SessionEnd": true, "SessionStart": true, "Notification": true}

// signalKinds is the closed set of accepted reason and notification types.
var signalKinds = map[string]bool{"": true, "clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "other": true,
	"startup": true, "compact": true, "fork": true, "permission_prompt": true, "elicitation_dialog": true, "idle_prompt": true}

// NormalizeSignal checks a client lifecycle event against the closed sets and returns the kind to
// use. A SessionEnd reason a newer client adds still means the session ended, so it reads as
// "other" instead of being refused and silently losing the feature. ok is false for an event or a
// kind outside the sets.
func NormalizeSignal(event, kind string) (string, bool) {
	if event == "SessionEnd" && !signalKinds[kind] {
		kind = "other"
	}
	return kind, signalEvents[event] && signalKinds[kind]
}

// SignalResult is what ApplyClientSignal did.
type SignalResult struct {
	// Applied is true when the event is one the keeper acts on (SessionEnd, or SessionStart with
	// kind "compact"). It stays true when nothing was live to retire.
	Applied bool
	// Ended is true when the session was marked ended (SessionEnd only).
	Ended bool
	// Retired is how many live entries the retirement removed. It can be 0 when Applied is true.
	Retired int
	// Cutoff is zero for SessionEnd, which retires every thread of the session. For a compact it is
	// the arrival time less CompactRaceMargin: only the main thread's entries whose last request
	// started before it are retired.
	Cutoff time.Time
}

// ApplyClientSignal applies one client lifecycle event to the session and says what it did.
//
//   - SessionEnd, any reason (even clear or resume): retires every entry of the id the event names,
//     because that id will not send its next request from this process, then marks it ended. If it
//     does (a resumed session), the request starts after the mark and is tracked normally (EndedAfter).
//   - SessionStart with kind "compact": compaction replaced the conversation, so every entry held for
//     the session caches the old prefix, which no later request will send. The session itself goes
//     on, so there is no end mark. The hook sends in the background, so the event can arrive after
//     the post-compaction request started: only entries whose last request started CompactRaceMargin
//     before the arrival are the old prefix.
//   - Everything else is accepted and never acted on. Notification: offline, the pings sent on
//     tool-wait rows that outlast 60 s (the nearest proxy for "blocked on a human") net positive, so
//     a rule that stops them would lose money.
//
// afterRetire runs after the retirement and before the end mark, so a host can tell a campaign the
// session ended (nil is allowed).
func (k *Keeper) ApplyClientSignal(tenantID, scoped, event, kind string, now time.Time,
	afterRetire func(SignalResult)) SignalResult {
	if k == nil {
		return SignalResult{}
	}
	var res SignalResult
	switch {
	case event == "SessionEnd":
		before := k.Stats().Live
		k.RetireSession(tenantID, scoped)
		res = SignalResult{Applied: true, Ended: true, Retired: before - k.Stats().Live}
		if afterRetire != nil {
			afterRetire(res)
		}
		k.MarkEnded(tenantID, scoped, now)
	case event == "SessionStart" && kind == "compact":
		before := k.Stats().Live
		res.Cutoff = now.Add(-CompactRaceMargin)
		k.RetireSessionStartedBefore(tenantID, scoped, res.Cutoff)
		res.Applied, res.Retired = true, before-k.Stats().Live
		if afterRetire != nil {
			afterRetire(res)
		}
	}
	return res
}
