package proxy

import (
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/thread"
)

// accThreadID maps the primary thread to "main", the library's name for it.
func accThreadID(id string) string {
	if id == thread.Primary {
		return "main"
	}
	return id
}

// accBegin opens the accountant record with the body the agent sent. Nil-safe.
func (c *capture) accBegin(meta accountant.RequestMeta, client []byte) {
	if c == nil || c.savings == nil {
		return
	}
	c.accReq = c.savings.begin(meta, client)
	c.accClient = client
}

// accEnd records the thread, the forwarded body and the operations. A thread that continues
// another by content names it as its parent, but starts with no state of its own: accountant carries
// Thread.Parent and does not read it (design §4.1). Nil-safe; a no-op when accBegin did not open a
// record.
func (c *capture) accEnd(th thread.Result, forwarded []byte, ops []accountant.Operation) {
	if c == nil || c.savings == nil || c.accReq == nil {
		return
	}
	t := accountant.Thread{ID: accThreadID(th.ID)}
	if parent, ok := th.InheritsFrom(); ok {
		t.Parent = accThreadID(parent)
	}
	// The envelope changes report no operation of their own: envelopeOps finds them from the two
	// bodies. Appended after the pipeline's ops, because the tools and system positions do not move
	// with a message splice.
	ops = append(append([]accountant.Operation(nil), ops...), envelopeOps(c.accClient, forwarded)...)
	// PingRead is left nil: see the LIMITATION note on entryFacts.
	c.savings.end(c.accReq, t, forwarded, ops, entryFacts(forwarded, c.pings, c.accReq.Meta.Session, t))
}

// entryFacts builds the facts the library needs about the entry this request wrote. A keep-alive
// ping for the thread since its last request is consumed from pings (nil = none) and supplies
// LastPing and PingWindow.
//
// LIMITATION: PingRead stays nil. The library takes its ping-hot branch, and so produces a Rescue
// effect, only with PingRead, the extent the ping read in forwarded coordinates, and the
// keeper's report does not carry it. Inventing one would misprice the rescue, so with it nil the
// library uses LastPing for the gap only.
func entryFacts(forwarded []byte, pings *PingStore, session string, t accountant.Thread) accountant.EntryFacts {
	facts := accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
	for _, bp := range accBreakpoints(forwarded) {
		if bp.TTL == "1h" {
			facts.TTL = time.Hour
			break
		}
	}
	if pf, ok := pings.take(accPingKey(session, t.ID)); ok {
		facts.LastPing, facts.PingWindow = pf.LastPing, pf.PingWindow
	}
	return facts
}

// accPingKey is the key the ping store keeps one thread's ping facts under: the session and the
// thread id as the accountant sees it ("main" for the primary thread).
func accPingKey(session, threadID string) string { return session + "\x00" + threadID }
