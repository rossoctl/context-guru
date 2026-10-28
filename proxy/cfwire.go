package proxy

import (
	"fmt"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/internal/thread"
)

// The values of Options.Savings.
const (
	SavingsLegacy         = "legacy"
	SavingsShadow         = "shadow"
	SavingsCounterfactual = "counterfactual"
)

// ParseSavings normalizes "" to legacy and refuses any value that is not one of the three modes.
func ParseSavings(v string) (string, error) {
	switch v {
	case "", SavingsLegacy:
		return SavingsLegacy, nil
	case SavingsShadow, SavingsCounterfactual:
		return v, nil
	}
	return "", fmt.Errorf("savings: unknown mode %q (want legacy, shadow or counterfactual)", v)
}

// cfThreadID maps the primary thread to "main", the library's name for it.
func cfThreadID(id string) string {
	if id == thread.Primary {
		return "main"
	}
	return id
}

// cfBegin opens the counterfactual record with the body the agent sent. Nil-safe.
func (c *capture) cfBegin(meta cf.RequestMeta, client []byte) {
	if c == nil || c.savings == nil {
		return
	}
	c.cfReq = c.savings.begin(meta, client)
}

// cfEnd records the thread, the forwarded body and the operations. A thread that continues
// another by content starts from a copy of the parent's state. Nil-safe; a no-op when cfBegin
// did not open a record.
func (c *capture) cfEnd(th thread.Result, forwarded []byte, ops []cf.Operation) {
	if c == nil || c.savings == nil || c.cfReq == nil {
		return
	}
	t := cf.Thread{ID: cfThreadID(th.ID)}
	if parent, ok := th.InheritsFrom(); ok {
		t.Parent = cfThreadID(parent)
		c.savings.trackers.Fork(c.cfReq.Meta.Session, cf.Thread{ID: t.Parent}, t)
	}
	// PingRead is left nil: see the LIMITATION note on entryFacts.
	c.savings.end(c.cfReq, t, forwarded, ops, entryFacts(forwarded, c.pings, c.cfReq.Meta.Session, t))
}

// entryFacts builds the facts the library needs about the entry this request wrote. A keep-alive
// ping for the thread since its last request is consumed from pings (nil = none) and supplies
// LastPing and PingWindow.
//
// LIMITATION: PingRead stays nil. The library takes its ping-hot branch, and so produces a Rescue
// effect, only with PingRead, the extent the ping read in forwarded coordinates, and the
// keeper's report does not carry it. Inventing one would misprice the rescue, so with it nil the
// library uses LastPing for the gap only.
func entryFacts(forwarded []byte, pings *PingStore, session string, t cf.Thread) cf.EntryFacts {
	facts := cf.EntryFacts{TTL: 5 * time.Minute, Lifetime: cf.LifetimeUnknown}
	for _, bp := range cfBreakpoints(forwarded) {
		if bp.TTL == "1h" {
			facts.TTL = time.Hour
			break
		}
	}
	if pf, ok := pings.take(cf.ThreadKey(session, t)); ok {
		facts.LastPing, facts.PingWindow = pf.LastPing, pf.PingWindow
	}
	return facts
}
