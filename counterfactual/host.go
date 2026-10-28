package counterfactual

import (
	"slices"
	"time"
)

// BillingFrom wraps a billing mode the host read from the request's credential.
func BillingFrom(mode BillingMode) Billing { return Billing{Mode: mode} }

// Ping is a keep-alive ping in the library's own terms, so the library does not import a host
// type. The host maps its report onto this.
type Ping struct {
	Session, ThreadID, Model, Host string
	Usage                          CallUsage
	At                             time.Time // when the ping started
	Window                         time.Time // the start of the idle gap the ping served
}

// PingFacts are what the thread's next real request needs in EntryFacts (cortex-wiring W4.6).
type PingFacts struct {
	LastPing   time.Time
	PingWindow string
}

// PingOps turns a ping into a RequestOps with one spend, and returns the facts for the next
// request. The thread "" is the primary thread and becomes "main".
func PingOps(p Ping, b Billing) (RequestOps, PingFacts) {
	id := p.ThreadID
	if id == "" {
		id = "main"
	}
	change := "ka:" + id + ":" + p.Window.UTC().Format(time.RFC3339)
	return RequestOps{
			Version: 1,
			Session: p.Session,
			Thread:  Thread{ID: id},
			Model:   p.Model,
			At:      p.At,
			Billing: b,
			Ops: []Operation{{
				Kind:     KindSpend,
				Operator: "context-guru.keepalive",
				ChangeID: change,
				Call:     &ModelCall{Host: p.Host, Model: p.Model, Usage: p.Usage},
			}},
		},
		PingFacts{LastPing: p.At, PingWindow: change}
}

// IsActive reports whether the thread still applies the change again on each request. A host
// need not check it before reporting: the library replaces an active change by a reported one with
// the same ChangeID. Tests use it to observe thread state.
func (ts *Trackers) IsActive(session string, t Thread, changeID string) bool {
	ts.mu.Lock()
	tr, ok := ts.m[ThreadKey(session, t)]
	ts.mu.Unlock()
	if !ok {
		return false
	}
	for _, op := range tr.snapshot().Active {
		if op.ChangeID == changeID {
			return true
		}
	}
	return false
}

// Fork installs a copy of the parent thread's state as the child's initial state, when the child
// has none and the parent has some. A child with state, or a parent without, is left alone.
func (ts *Trackers) Fork(session string, parent, child Thread) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	p, ok := ts.m[ThreadKey(session, parent)]
	if !ok {
		return
	}
	if _, has := ts.m[ThreadKey(session, child)]; has {
		return
	}
	s := p.snapshot()
	// The Fingerprint's Body slices stay shared with the parent's. That is safe because no one
	// mutates them in place: nextState builds a new Fingerprint for every request.
	if s.PrevPrint != nil {
		fp := *s.PrevPrint
		s.PrevPrint = &fp
	}
	s.Version = 0
	ts.m[ThreadKey(session, child)] = &Tracker{state: s, created: time.Now()}
}

// Len is the number of Trackers held.
func (ts *Trackers) Len() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.m)
}

// EvictIdle removes every Tracker whose thread was last seen before before: its previous request
// (ThreadState.PrevPrint.At), or its creation when it has none. It returns how many it removed.
//
// A Request in flight is not affected: it holds its own snapshot (EndRequest), and Compute looks the
// Tracker up again, so after an eviction it creates a new, empty one, calculates against that
// (Result.Recomputed, phase unknown) and stores the next state there. Nothing correct is lost when
// before is older than the cache TTL: past it the library can only find the phase unknown.
func (ts *Trackers) EvictIdle(before time.Time) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	for k, t := range ts.m {
		if t.lastSeen().Before(before) {
			delete(ts.m, k)
			n++
		}
	}
	return n
}

// EvictOldest removes the Trackers seen longest ago until at most keep remain, and returns how many
// it removed. It is the hard bound for a process that sees more threads inside the idle window than
// it can hold.
func (ts *Trackers) EvictOldest(keep int) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	over := len(ts.m) - max(keep, 0)
	if over <= 0 {
		return 0
	}
	type aged struct {
		key string
		at  time.Time
	}
	all := make([]aged, 0, len(ts.m))
	for k, t := range ts.m {
		all = append(all, aged{k, t.lastSeen()})
	}
	slices.SortFunc(all, func(a, b aged) int { return a.at.Compare(b.at) })
	for _, a := range all[:over] {
		delete(ts.m, a.key)
	}
	return over
}
