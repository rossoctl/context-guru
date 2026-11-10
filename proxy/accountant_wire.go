package proxy

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
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
	// The savings record is a side figure. A panic while hashing a hostile body must cost that figure only, and
	// must not reach the pre-forward recover, which would throw away the pipeline's rewrite of the request
	// (issue #918). The record is dropped, and the event books no saving.
	defer func() {
		if p := recover(); p != nil {
			slog.Error("accountant: BeginRequest panicked; the request is recorded with no saving", "panic", p)
			c.accDrop()
		}
	}()
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
	//
	// The library gets the prompt the provider BILLS (accountant R56, billedBody), not the bytes sent: a tool
	// deferred with defer_loading is a stub in it. The sent bytes still decide the cache facts below.
	billed := c.accBilled(forwarded)
	ops = append(append([]accountant.Operation(nil), ops...), c.envelopeOpsFor(billed)...)
	facts := entryFacts(bschemas.ModelProvider(c.provider), c.accReq.Meta.Model, forwarded, c.pings, c.accReq.Meta.Session, t)
	c.accPingWindow = facts.PingWindow
	c.savings.end(c.accReq, t, billed, ops, facts)
}

// accDeferralUnplaced names why a request books no figure when its deferral cannot be placed. The log line says
// acc_fallback deferral_unplaced (noResultWhy). The stored fallback of the event is no_result.
const accDeferralUnplaced = "deferral_unplaced"

// accBilled is the body the accountant records. A request deferral did not change returns the forwarded body. A
// deferral whose stub cannot be placed (a stub that would not be smaller than its definition) also returns the
// forwarded body, and sets accNoResult so the event books no figure and the sum check does not turn it into a
// wrong one. A tool the client deferred itself is left as it is (ownDeferred).
func (c *capture) accBilled(forwarded []byte) []byte {
	if c.accPreDefer == nil {
		return forwarded
	}
	own := ownDeferred(forwarded, c.accPreDefer)
	if len(own) == 0 {
		return forwarded
	}
	b, ok := billedBody(forwarded, own)
	if !ok {
		c.accNoResult = accDeferralUnplaced
		return forwarded
	}
	return b
}

// envelopeOpsFor is envelopeOps for the body the accountant records, with tool deferral's edits attributed to the
// tooldefer operator so its row does not land on toolschema (#908). Deferral's edits are two: the definitions the
// proxy deferred, each now a stub (R56), and the search tool it adds, an ordinary added declaration.
//
// Each keeps the ChangeID envelopeOps gave it, and only the operator changes. The ChangeID is what the library
// replaces by, so a thread whose state was written before this change (per-tool cg:toolschema:decl-rewritten and
// decl-added operations) has them replaced on its next request instead of kept beside a second copy, and a deferred
// set that grows or shrinks mid-thread re-reports each surviving tool under its own stable ID. One merged operation
// would carry a hash of the whole set, so any change of the set would orphan it.
func (c *capture) envelopeOpsFor(billed []byte) []accountant.Operation {
	ops := envelopeOps(c.accClient, billed)
	if c.accPreDefer == nil {
		return ops
	}
	// Match by NAME: a rewritten declaration is reported at its CLIENT index, and a declaration removed before it
	// makes the billed index smaller.
	own := ownDeferred(billed, c.accPreDefer)
	atBilled := map[string]int{}
	for i, t := range gjson.GetBytes(billed, "tools").Array() {
		atBilled[t.Get("name").String()] = i
	}
	client := gjson.GetBytes(c.accClient, "tools").Array()
	out := make([]accountant.Operation, 0, len(ops))
	for _, o := range ops {
		switch {
		case o.Operator == "context-guru.toolschema" && o.At.Section == accountant.SectionTools &&
			strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-rewritten:") && o.At.Index < len(client) &&
			own[client[o.At.Index].Get("name").String()] && isStub(billed, atBilled[client[o.At.Index].Get("name").String()]):
			o.Operator = "context-guru.tooldefer"
		case o.Operator == "context-guru.toolschema" && strings.HasPrefix(o.ChangeID, "cg:toolschema:decl-added:") &&
			gjson.GetBytes(billed, "tools."+strconv.Itoa(o.At.Index)+".name").String() == tooldefer.SearchName:
			o.Operator = "context-guru.tooldefer"
		}
		out = append(out, o)
	}
	return out
}

// isStub says whether the tool at i of the billed body is a deferred stub: a bare name and the defer_loading mark.
func isStub(billed []byte, i int) bool {
	t := gjson.GetBytes(billed, "tools."+strconv.Itoa(i))
	return t.Get("defer_loading").Bool() && t.Get("name").Exists()
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
// keeper also needs the billed tier, which is known only after the response. These facts are written
// before it, so they hold the ASKED tier. accBilledTTL corrects it once the usage arrives, before the
// library prices the request (context-guru#919).
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

// accBilledTTL lowers the entry TTL from the tier the request asked for (1h) to the tier the response
// billed, when a gateway downgraded it. It runs after the usage arrives and before Compute, which reads
// the facts' TTL for the verdict and stores it as the entry's TTL. Without it the library treats a gap of
// 5 to 60 minutes as hot and can overstate a rescue by up to 55 minutes of idle time.
//
// The rule is keepalive.ResolveTier's: one hour needs every write billed at the one-hour tier. A request
// that wrote nothing bills no tier, so the asked tier stands. Only an exact lifetime has a TTL that can be
// lowered: a minimum (OpenAI) has no tier. SharedWarm was judged with the asked TTL at accEnd and is not
// redone; it matters only when another thread of the session touched the entry. Nil-safe.
func (c *capture) accBilledTTL(u Usage) {
	if c == nil || c.accReq == nil {
		return
	}
	f := &c.accReq.Facts
	if f.Lifetime != accountant.LifetimeExact || f.TTL <= 5*time.Minute {
		return
	}
	if u.CacheWrite > 0 && u.CacheWrite1h < u.CacheWrite {
		f.TTL = 5 * time.Minute
		// The library judges each part of the entry by the TTL of the breakpoint that wrote it, so the
		// breakpoints must say 5m too. A copy: the slice may be the one the client hash cache holds.
		bps := append([]accountant.Breakpoint(nil), c.accReq.Breakpoints...)
		for i := range bps {
			bps[i].TTL = "5m"
		}
		c.accReq.Breakpoints = bps
	}
}
