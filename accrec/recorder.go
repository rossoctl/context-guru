package accrec

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/thinkcount"
)

// Usage is what one response billed, in tokens.
type Usage struct {
	FreshInput, CacheRead, CacheWrite, CacheWrite1h int64
}

func (u Usage) lib() accountant.Usage {
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
	return req.Compute(u.lib())
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
