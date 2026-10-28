package counterfactual

import "time"

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
	if s.PrevPrint != nil {
		fp := *s.PrevPrint
		s.PrevPrint = &fp
	}
	s.Version = 0
	ts.m[ThreadKey(session, child)] = &Tracker{state: s}
}
