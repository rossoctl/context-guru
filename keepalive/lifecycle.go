package keepalive

import (
	"strings"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/thread"
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
	return n
}
