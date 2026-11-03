package proxy

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
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
	c.savings.end(c.accReq, t, forwarded, ops,
		entryFacts(bschemas.ModelProvider(c.provider), c.accReq.Meta.Model, forwarded, c.pings, c.accReq.Meta.Session, t))
}

// entryFacts builds the facts the library needs about the entry this request wrote. A keep-alive
// ping for the thread since its last request is consumed from pings (nil = none) and supplies
// LastPing, PingWindow and the ping's usage. PingRead stays nil: the keeper replays the previous
// request's forwarded body, so the library derives the extent the ping read from its usage
// (accountant design §5.2).
//
// TTL and Lifetime come from apply.CacheLifetime, the same rule the cold gate uses: an exact expiry
// for a provider with explicit breakpoints, a minimum for OpenAI's 30-minute entries, else unknown.
// An unknown lifetime cannot prove an expiry, so the library would never rescue after a gap past
// the TTL (design §5.2). A 1h breakpoint in the forwarded body still lengthens a TTL that is short.
func entryFacts(provider bschemas.ModelProvider, model string, forwarded []byte, pings *PingStore,
	session string, t accountant.Thread) accountant.EntryFacts {
	facts := accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
	switch ttl, kind := apply.CacheLifetime(provider, model, forwarded); kind {
	case apply.CacheLifetimeExact:
		facts.TTL, facts.Lifetime = ttl, accountant.LifetimeExact
	case apply.CacheLifetimeMinimum:
		facts.TTL, facts.Lifetime = ttl, accountant.LifetimeMinimum
	}
	for _, bp := range accBreakpoints(forwarded) {
		if bp.TTL == "1h" && facts.TTL < time.Hour {
			facts.TTL = time.Hour
			break
		}
	}
	if pf, ok := pings.take(accPingKey(session, t.ID)); ok {
		pf.Apply(&facts)
	}
	return facts
}

// accPingKey is the key the ping store keeps one thread's ping facts under: the session and the
// thread id as the accountant sees it ("main" for the primary thread).
func accPingKey(session, threadID string) string { return session + "\x00" + threadID }
