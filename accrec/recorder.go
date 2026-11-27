package accrec

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/thinkcount"
)

// Usage is what one response billed, in tokens.
type Usage struct {
	FreshInput, CacheRead, CacheWrite, CacheWrite1h int64
}

// Accountant is the usage in the library's type. It is the one conversion: every host site that builds an
// accountant.Usage from a billed usage calls it.
func (u Usage) Accountant() accountant.Usage {
	return accountant.Usage{Input: int(u.FreshInput), CacheRead: int(u.CacheRead),
		CacheWrite: int(u.CacheWrite), CacheWrite1h: int(u.CacheWrite1h)}
}

// Recorder runs the accountant calls around one request. It holds the one Trackers of a host: a
// thread's state lives there between requests. A host builds one with NewRecorder and calls
// Begin, End and Price for each request. It keeps files, logs and metrics to itself (see
// SnapshotState and RestoreState for the state it keeps across a restart).
type Recorder struct {
	// Trackers holds each thread's state. Exported so a host test can break it on purpose.
	Trackers *accountant.Trackers
	// Siblings holds each thread's last forwarded body, for EntryFacts.SharedWarm.
	Siblings *SiblingStore
	// Tiers is the tier each thread's last write was billed at, for BilledTTL (issue #1078).
	Tiers *TierStore
	// Thinking holds the token count of each thinking block a response made, by its signature's key, so a block
	// the client sends back is priced at its own count (issue #1187, accountant design D16). Saved with the state.
	Thinking *thinkcount.Table
	// Clients holds the hash of each open request's client body, by request, from Begin to End.
	Clients sync.Map // *accountant.Request -> clientHash
	// Forwarded holds the forwarded body of each ended request, by request, from End to Price or Drop (#1533): the
	// request ratio is built from it, before Compute, for a cost the host reports as an operation.
	Forwarded sync.Map // *accountant.Request -> accountant.Body
	// OnEnd, when set, is called with each record after EndRequest. Tests only.
	OnEnd func(*accountant.Request, EndRecord)

	// rescueFactsMissing counts priced requests that followed keep-alive pings but found no ping facts.
	rescueFactsMissing atomic.Int64
}

// NewRecorder returns a Recorder with empty state.
func NewRecorder() *Recorder {
	return &Recorder{Trackers: accountant.NewTrackers(), Siblings: NewSiblingStore(), Tiers: NewTierStore(),
		Thinking: thinkcount.NewTable(0)}
}

// RecordThinking stores the thinking counts of one billed response. Nil-safe.
func (s *Recorder) RecordThinking(r thinkcount.Response) {
	if s != nil {
		s.Thinking.Record(r)
	}
}

// NoteRescueFactsMissing counts one priced request that followed pings and found no ping facts. Nil-safe.
func (s *Recorder) NoteRescueFactsMissing() {
	if s != nil {
		s.rescueFactsMissing.Add(1)
	}
}

// RescueFactsMissing is the count NoteRescueFactsMissing keeps. Nil-safe.
func (s *Recorder) RescueFactsMissing() int64 {
	if s == nil {
		return 0
	}
	return s.rescueFactsMissing.Load()
}

// clientHash is what Begin built from the client body, kept until End so a request that was forwarded unchanged does
// not hash the same bytes again. Hashing a 600 KB body costs 3 to 4 ms.
type clientHash struct {
	meta accountant.RequestMeta // the library keeps its record private, so End reads the meta from here
	raw  []byte
	body accountant.Body
	bps  []accountant.Breakpoint
}

// EndRecord is what End gave EndRequest. The library keeps its record in unexported fields, so a test
// that checks the bodies the library saw reads them here.
type EndRecord struct {
	Client, Forwarded accountant.Body
	Breakpoints       []accountant.Breakpoint
}

// Drop forgets the client hash of a request that will never end: a panic between Begin and End makes the host
// send the original body, and End is the only other place that removes the entry. Nil-safe.
func (s *Recorder) Drop(req *accountant.Request) {
	if s != nil && req != nil {
		s.Clients.Delete(req)
		s.Forwarded.Delete(req)
	}
}

// Begin opens the request record with the client body, before any component ran. The Trackers bound
// their own memory (accountant design §6.1, Memory), so there is nothing to sweep here.
func (s *Recorder) Begin(meta accountant.RequestMeta, client []byte) *accountant.Request {
	body, bps := accwire.For(client).Parse(client, s.Thinking.Lookup)
	req := s.Trackers.BeginRequest(meta, body, bps)
	s.Clients.Store(req, clientHash{meta: meta, raw: client, body: body, bps: bps})
	return req
}

// End records the operations, then the thread and the forwarded body. Every op is added, replays
// included: the library replaces an active change by the reported op with the same ChangeID
// (accountant internal/engine/compute.go, items()), so a change is priced once and unique_change never fires.
// Skipping a replay here would lose the op whenever Compute ends the change on this request.
func (s *Recorder) End(req *accountant.Request, th accountant.Thread, forwarded []byte, ops []accountant.Operation, facts accountant.EntryFacts) EndRecord {
	for _, op := range ops {
		req.Add(op)
	}
	var fwd accountant.Body
	var bps []accountant.Breakpoint
	var client accountant.Body
	var meta accountant.RequestMeta
	if v, ok := s.Clients.LoadAndDelete(req); ok {
		client, meta = v.(clientHash).body, v.(clientHash).meta
		if bytes.Equal(v.(clientHash).raw, forwarded) {
			fwd, bps = client, v.(clientHash).bps
		} else {
			fwd, bps = accwire.For(forwarded).Parse(forwarded, s.Thinking.Lookup)
		}
	} else {
		fwd, bps = accwire.For(forwarded).Parse(forwarded, s.Thinking.Lookup)
	}
	facts.SharedWarm = s.Siblings.SharedWarm(meta.Session, th.ID, meta.At, facts.TTL, fwd, bps)
	req.EndRequest(th, fwd, bps, facts)
	s.Forwarded.Store(req, fwd)
	rec := EndRecord{Client: client, Forwarded: fwd, Breakpoints: bps}
	if s.OnEnd != nil {
		s.OnEnd(req, rec)
	}
	return rec
}

// ThreadOf is the library thread for a thread id and, when the thread continues another by content, its parent.
// A continuing thread starts with no state of its own: accountant carries Thread.Parent and does not read it
// (design §4.1).
func ThreadOf(id string, parent string, hasParent bool) accountant.Thread {
	t := accountant.Thread{ID: ThreadID(id)}
	if hasParent {
		t.Parent = ThreadID(parent)
	}
	return t
}

// Compute prices the request. It stores the new ThreadState (so a shadow run still advances it).
func (s *Recorder) Compute(req *accountant.Request, u Usage) accountant.Result {
	return req.Compute(u.Accountant())
}

// ComputeSafe is Compute for the request path. The library asks the host to wrap Compute, and the rule is
// to fail open: a panic here would unwind through the host's finish and lose the log line and the dashboard row
// of a request that was already forwarded. It reports ok=false instead, the request is recorded with no saving
// (PriceMode falls back with no_result), and the panic is logged. Tests call Compute, so a panic still fails them.
func (s *Recorder) ComputeSafe(req *accountant.Request, u Usage) (res accountant.Result, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accountant: Compute panicked; the request is recorded with no saving", "panic", r)
			res, ok = accountant.Result{}, false
		}
	}()
	return s.Compute(req, u), true
}

// PriceIn is what Price needs of one finished request.
type PriceIn struct {
	Status, Rounds int
	// NoResult is the reason an earlier step gave for booking no figure (a deferral that could not be placed). "" if none.
	NoResult string
	Usage    Usage
	// BeforeCompute, when set, runs once, just before the library prices the request and only when it will. The
	// host uses it to set the entry's billed tier on its facts (BilledTTL), which Compute reads.
	BeforeCompute func()
	// AfterCompute, when set, runs once with the result, right after the library priced the request and before the
	// rescue counter and the ping-facts drop. It does not run when the request books no figure. The host books the
	// result here (event, metrics), so those side effects keep their order.
	AfterCompute func(accountant.Result)
	// Session and Thread are the ping store's key. KaPings is how many pings went out in the gap this request ended.
	// PingWindow is the window of the ping facts the request read when it opened, "" if it read none.
	Session, Thread string
	KaPings         int
	PingWindow      string
	Pings           *PingStore
}

// PriceOut is what Price decided. NoResult is PriceIn.NoResult, or the reason the request books no figure.
type PriceOut struct {
	Result   accountant.Result
	Priced   bool // Result is set
	NoResult string
}

// Price prices one finished request, or says why it books no figure. The rules for which request books none are
// ShouldCompute's (a rejected status, a deferral that could not be placed, several upstream rounds) and a panic in
// the library (ComputePanicked). It also counts a request that followed pings and found no ping facts, and
// spends the ping facts it read: they belong to the next priced request only.
func (s *Recorder) Price(req *accountant.Request, in PriceIn) PriceOut {
	defer s.Forwarded.Delete(req) // the body served BeforeCompute (RatioFor); nothing reads it after the request is priced
	out := PriceOut{NoResult: in.NoResult}
	compute, why := ShouldCompute(in.Status, in.Rounds, in.NoResult)
	if !compute {
		out.NoResult = why
		return out
	}
	if in.BeforeCompute != nil {
		in.BeforeCompute()
	}
	if res, ok := s.ComputeSafe(req, in.Usage); ok {
		out.Result, out.Priced = res, true
		if in.AfterCompute != nil {
			in.AfterCompute(res)
		}
	} else {
		out.NoResult = ComputePanicked
	}
	// Pings went out in the gap this request ended, yet no ping facts were found for its thread:
	// the credit for them is lost. Count it, so a host that does not wire the ping side shows.
	if in.KaPings > 0 && in.PingWindow == "" {
		s.NoteRescueFactsMissing()
	}
	// The ping facts were read for this request; now that it is priced they are spent.
	in.Pings.Drop(PingKey(in.Session, in.Thread), in.PingWindow)
	return out
}

// RecordIn is what Record needs of one request, after the pipeline ran.
type RecordIn struct {
	Meta accountant.RequestMeta
	// Client is the body the agent sent. PreDefer is the body tool deferral read, nil on a request it did not change.
	Client, PreDefer []byte
	// Forwarded is the body sent upstream. Ops are the pipeline's operations; Record appends the envelope ones.
	Forwarded []byte
	Ops       []accountant.Operation
	// ThreadID is the thread's id. Parent is the thread it continues by content, when HasParent.
	ThreadID, Parent string
	HasParent        bool
	Provider         bschemas.ModelProvider
	Endpoint, Route  string
	// ReadSets tells whether a read sets the entry's TTL on this route (the keeper's readSetsTTL). nil: never.
	ReadSets func(provider bschemas.ModelProvider, endpoint, route, model string, pinged bool) bool
	Pings    *PingStore
}

// RecordOut is what Record decided. NoResult is set only when the request books no figure (a deferral that
// could not be placed).
type RecordOut struct {
	Thread   string
	Facts    accountant.EntryFacts
	NoResult string
}

// Record records the thread, the forwarded body and the operations of an open request. A thread that continues
// another by content names it as its parent, but starts with no state of its own: accountant carries Thread.Parent
// and does not read it (design §4.1).
func (s *Recorder) Record(req *accountant.Request, in RecordIn) RecordOut {
	t := ThreadOf(in.ThreadID, in.Parent, in.HasParent)
	out := RecordOut{Thread: t.ID}
	// The envelope changes report no operation of their own: EnvelopeOpsFor finds them from the two
	// bodies. Appended after the pipeline's ops, because the tools and system positions do not move
	// with a message splice.
	//
	// The library gets the prompt the provider BILLS (accountant R56, billedBody), not the bytes sent: a tool
	// deferred with defer_loading is a stub in it. The sent bytes still decide the cache facts below.
	billed, noResult := BilledForwarded(in.Forwarded, in.PreDefer)
	out.NoResult = noResult
	ops := append(append([]accountant.Operation(nil), in.Ops...), EnvelopeOpsFor(in.Client, billed, in.PreDefer)...)
	facts := EntryFacts(in.Provider, in.Meta.Model, in.Forwarded, in.Pings, in.Meta.Session, t)
	// A read sets the entry's TTL on a route the keeper PROVED does so (the 1-hour-on-wait probe), and on the
	// return that decides that proof: facts.PingTTL is 1 hour when a 1-hour ping served the gap. The keeper
	// records the proof after this request is priced, so without the second case the proof return would book
	// no rescue. Nowhere else: Provider.ReadSetsTTL is off by default, as the library asks for a provider not
	// measured. The rescue is itself evidence (it credits only tokens the return READ), see Keeper.ReadSetsTTL.
	if in.ReadSets != nil && in.ReadSets(in.Provider, in.Endpoint, in.Route, in.Meta.Model, facts.PingTTL >= OneHourTTL) {
		facts.Provider.ReadSetsTTL = true
	}
	out.Facts = facts
	s.End(req, t, billed, ops, facts)
	return out
}

// LowerTTL sets the entry TTL to the tier the response billed, when a gateway downgraded the tier the request asked
// for (1h), and returns the facts. It applies BilledTTL, which holds the rule. It runs after the usage arrives and
// before Compute, which reads the facts' TTL for the verdict and stores it as the entry's TTL. The library lowers the
// facts' TTL and the TTL of every 1h breakpoint together, on a copy. Nil-safe: a nil request changes nothing.
func (s *Recorder) LowerTTL(req *accountant.Request, session, thread string, f accountant.EntryFacts, cacheWrite, cacheWrite1h int64) accountant.EntryFacts {
	if req == nil {
		return f
	}
	if BilledTTL(s.Tiers, PingKey(session, thread), f, cacheWrite, cacheWrite1h) {
		f.TTL = BaseTTL
		req.LowerTTL(BaseTTL)
	}
	return f
}

// RatioFor is the request ratio of an ended request, before Compute: the provider's billed tokens of the forwarded
// text (accountant.NewRatio), the same ratio Compute builds. A host uses it to convert the bytes of a cost it
// reports as an operation, so the operation and the figure share one ratio (#1533). nil when the request has no
// forwarded body here, the usage bills nothing, or the ratio is out of range (accountant check 9): the host keeps its fallback. Nil-safe.
func (s *Recorder) RatioFor(req *accountant.Request, u Usage) *accountant.Ratio {
	if s == nil || req == nil {
		return nil
	}
	v, ok := s.Forwarded.Load(req)
	if !ok {
		return nil
	}
	a := u.Accountant()
	billed := a.Input + a.CacheRead + a.CacheWrite
	if billed <= 0 {
		return nil
	}
	r := accountant.NewRatio(billed, v.(accountant.Body))
	if !r.InRange() {
		return nil // Compute would refuse this ratio (check 9) and set no Result.Ratio: a host keeps its fallback
	}
	return &r
}
