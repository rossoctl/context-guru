package proxy

import (
	"log/slog"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/thread"
)

// accBegin notes the body the agent sent. It does no work: parsing and hashing a large body cost a request
// milliseconds before it went upstream (issue #1059), so the record is opened by accEnd, off the request
// path. Nil-safe.
func (c *capture) accBegin(meta accountant.RequestMeta, client []byte) {
	defer c.timed("accBegin")()
	if c == nil || c.savings == nil {
		return
	}
	c.accMeta, c.accClient, c.accOpen = meta, client, true
}

// accJob is the accountant's work for one request, running on its own goroutine. w is a private capture that
// holds only what the work reads and writes, so the request goroutine and the job share no field. w is read only
// after done is closed.
type accJob struct {
	done chan struct{}
	w    *capture
}

// accEnd hands the thread, the forwarded body and the operations to the accountant and returns at once. The
// parsing, hashing and bookkeeping run on another goroutine; finish waits for them (accWait) before it prices the
// request, so a request's record is complete before the request is priced. The order of two requests of one thread
// is usual, not guaranteed: finish runs after the response is flushed, so a client can send the next turn first.
// The library recomputes a figure when another request of the thread intervened (Result.Recomputed). Nil-safe; a no-op when accBegin did not note a request.
func (c *capture) accEnd(th thread.Result, forwarded []byte, ops []accountant.Operation) {
	defer c.timed("accEnd")()
	if c == nil || c.savings == nil || !c.accOpen || c.accJob != nil {
		return
	}
	w := &capture{savings: c.savings, provider: c.provider, pings: c.pings, accClient: c.accClient, accPreDefer: c.accPreDefer, accMeta: c.accMeta,
		route: c.route, endpoint: c.endpoint, readSets: c.readSets}
	job := &accJob{done: make(chan struct{}), w: w}
	meta, client := c.accMeta, c.accClient
	ops = append([]accountant.Operation(nil), ops...)
	// Publish the job last: finish waits for a published job, so nothing that can fail may sit between this line
	// and the go statement.
	c.accJob = job
	go func() {
		defer close(job.done)
		w.accRun(meta, client, th, forwarded, ops)
	}()
}

// accWait joins the accountant's work and takes its results into c. Idempotent. Nil-safe.
func (c *capture) accWait() {
	defer c.timed("accWait")()
	if c == nil || c.accJob == nil {
		return
	}
	job := c.accJob
	<-job.done
	c.accJob = nil
	c.accReq, c.accThread, c.accPingWindow, c.accFacts = job.w.accReq, job.w.accThread, job.w.accPingWindow, job.w.accFacts
	if job.w.accNoResult != "" {
		c.accNoResult = job.w.accNoResult
	}
}

// accRun opens the record and ends it. It runs off the request path, on the job's private capture.
//
// The savings record is a side figure. A panic while hashing a hostile body must cost that figure only: the
// request is already on its way, and a panic on this goroutine would end the process. The record is dropped, and
// the event books no saving (issue #918).
func (c *capture) accRun(meta accountant.RequestMeta, client []byte, th thread.Result, forwarded []byte, ops []accountant.Operation) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("accountant: the record panicked; the request is recorded with no saving", "panic", p)
			c.accDrop()
		}
	}()
	c.accReq = c.savings.begin(meta, client)
	c.accEndNow(th, forwarded, ops)
}

// accEndNow records the thread, the forwarded body and the operations (accrec.Recorder.Record holds the rules).
func (c *capture) accEndNow(th thread.Result, forwarded []byte, ops []accountant.Operation) {
	if c.accReq == nil {
		return
	}
	// Set before the record runs, so a panic inside it still leaves the thread named, as it always did.
	c.accThread = accrec.ThreadID(th.ID)
	parent, hasParent := th.InheritsFrom()
	out := c.savings.Record(c.accReq, accrec.RecordIn{
		Meta: c.accMeta, Client: c.accClient, PreDefer: c.accPreDefer, Forwarded: forwarded, Ops: ops,
		ThreadID: th.ID, Parent: parent, HasParent: hasParent,
		Provider: bschemas.ModelProvider(c.provider), Endpoint: c.endpoint, Route: c.route, ReadSets: c.readSets, Pings: c.pings,
	})
	c.accThread, c.accPingWindow, c.accFacts = out.Thread, out.Facts.PingWindow, out.Facts
	if out.NoResult != "" {
		c.accNoResult = out.NoResult
	}
}

// accDeferralUnplaced names why a request books no figure when its deferral cannot be placed. The log line says
// acc_fallback deferral_unplaced (noResultWhy). The stored fallback of the event is no_result.
const accDeferralUnplaced = accrec.DeferralUnplaced

// accBilled is the body the accountant records. A request deferral did not change returns the forwarded body. A
// deferral whose stub cannot be placed (a stub that would not be smaller than its definition) also returns the
// forwarded body, and sets accNoResult so the event books no figure and the sum check does not turn it into a
// wrong one. A tool the client deferred itself is left as it is (ownDeferred).
func (c *capture) accBilled(forwarded []byte) []byte {
	b, noResult := accrec.BilledForwarded(forwarded, c.accPreDefer)
	if noResult != "" {
		c.accNoResult = noResult
	}
	return b
}

// envelopeOpsFor is accrec.EnvelopeOpsFor for this request's client body.
func (c *capture) envelopeOpsFor(billed []byte) []accountant.Operation {
	return accrec.EnvelopeOpsFor(c.accClient, billed, c.accPreDefer)
}

// entryFacts is accrec.EntryFacts with the proxy's ping store.
func entryFacts(provider bschemas.ModelProvider, model string, forwarded []byte, pings *PingStore,
	session string, t accountant.Thread) accountant.EntryFacts {
	return accrec.EntryFacts(provider, model, forwarded, pings, session, t) // a nil store has no facts (Peek is nil-safe)
}

// accBilledTTL applies accrec.BilledTTL, which holds the rule: it sets the entry TTL to the tier the response billed, when a gateway downgraded the tier
// the request asked for (1h). It runs after the usage arrives and before Compute, which reads the facts'
// TTL for the verdict and stores it as the entry's TTL. Without it the library treats a gap of 5 to 60
// minutes as hot and can overstate a rescue by up to 55 minutes of idle time.
//
// The rule is keepalive.BilledOneHour's, which ResolveTier uses: a write bills its tier on the tokens it
// wrote; a turn that wrote nothing bills none and keeps the thread's last billed tier (issue #1078). A
// thread with no earlier write has none to keep, so the asked tier stands. Only an exact lifetime has a TTL
// that can be lowered: a minimum (OpenAI) has no tier. SharedWarm was judged with the asked TTL at accEnd
// and is not redone; it matters only when another thread of the session touched the entry. Nil-safe.
func (c *capture) accBilledTTL(u Usage) {
	defer c.timed("accBilledTTL")()
	if c == nil || c.accReq == nil {
		return
	}
	c.accFacts = c.savings.LowerTTL(c.accReq, c.accMeta.Session, c.accThread, c.accFacts, u.CacheWrite, u.CacheWrite1h)
}

// accDeferSpend reports the search-turn costs of a deferred request to the accountant as spend operations on the
// tooldefer operator (#1441), so the one figure for deferral holds them. It runs before Compute. Nil-safe.
func (c *capture) accDeferSpend() {
	d := c.deferFacts
	if c.accReq == nil || d == nil || !d.spendOK {
		return
	}
	for _, op := range accrec.DeferSpendOps(d.model, d.spend) {
		c.accReq.Add(op)
	}
}
