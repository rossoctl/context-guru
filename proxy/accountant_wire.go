package proxy

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
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
	c.accThread = t.ID
	if parent, ok := th.InheritsFrom(); ok {
		t.Parent = accThreadID(parent)
	}
	// The envelope changes report no operation of their own: envelopeOps finds them from the two
	// bodies. Appended after the pipeline's ops, because the tools and system positions do not move
	// with a message splice.
	ops = append(append([]accountant.Operation(nil), ops...), envelopeOps(c.accClient, forwarded)...)
	facts := entryFacts(bschemas.ModelProvider(c.provider), c.accReq.Meta.Model, forwarded, c.pings, c.accReq.Meta.Session, t)
	c.accPingWindow = facts.PingWindow
	c.savings.end(c.accReq, t, forwarded, ops, facts)
}

// entryFacts builds the facts the library needs about the entry this request wrote. A keep-alive
// ping for the thread since its last request is read from pings (nil = none; the capture drops it
// once the request is priced) and supplies
// LastPing, PingWindow and the ping's usage. PingRead stays nil: the keeper replays the previous
// request's forwarded body, so the library derives the extent the ping read from its usage
// (accountant design §5.2).
//
// The Lifetime kind comes from apply.CacheLifetime, the same rule the cold gate uses: an exact expiry
// for a provider with explicit breakpoints, a minimum for OpenAI's 30-minute entries, else unknown.
// An unknown lifetime cannot prove an expiry, so the library would never rescue after a gap past
// the TTL (design §5.2). The TTL is 1h only when EVERY breakpoint of the forwarded body asks for it,
// as the keeper decides: a 1h head over a 5m tail leaves a tail that expires in five minutes. The
// keeper also needs the billed tier, which is known only after the response; the facts are written
// before it, so a request that asks for 1h and is billed 5m keeps the 1h here. The library then
// treats a gap of 5 to 60 minutes as hot, which can overstate a rescue (an edge case: a gateway that
// downgrades the tier).
func entryFacts(provider bschemas.ModelProvider, model string, forwarded []byte, pings *PingStore,
	session string, t accountant.Thread) accountant.EntryFacts {
	facts := accountant.EntryFacts{TTL: 5 * time.Minute, Lifetime: accountant.LifetimeUnknown}
	// The kind comes from the cold gate's rule. The duration is taken again with the keeper's tier
	// (every breakpoint asks 1h): apply.CacheLifetime lengthens it for a single 1h ask.
	switch ttl, kind := apply.CacheLifetime(provider, model, forwarded); kind {
	case apply.CacheLifetimeExact:
		facts.TTL, facts.Lifetime = modelinfo.CacheLifetime(string(provider), model, apply.AllBreakpointsAskExtendedTTL(forwarded)),
			accountant.LifetimeExact
	case apply.CacheLifetimeMinimum:
		facts.TTL, facts.Lifetime = ttl, accountant.LifetimeMinimum
	}
	if pf, ok := pings.peek(accPingKey(session, t.ID)); ok {
		pf.Apply(&facts)
	}
	return facts
}

// accPingKey is the key the ping store keeps one thread's ping facts under: the session and the
// thread id as the accountant sees it ("main" for the primary thread).
func accPingKey(session, threadID string) string { return session + "\x00" + threadID }
