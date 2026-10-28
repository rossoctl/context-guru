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
	facts := cf.EntryFacts{TTL: 5 * time.Minute, Lifetime: cf.LifetimeUnknown}
	for _, bp := range cfBreakpoints(forwarded) {
		if bp.TTL == "1h" {
			facts.TTL = time.Hour
			break
		}
	}
	c.savings.end(c.cfReq, t, forwarded, ops, facts)
}
