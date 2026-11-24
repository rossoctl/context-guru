package proxy

import (
	"sync"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/thinkcount"
)

// savingsTracker is the proxy's host of the accrec.Recorder. The Recorder holds the accountant logic and the
// state of every thread. This type adds what only the proxy has: the state file (accountant_state.go) and the
// proxy's Usage type. The fields below are views of the Recorder's own pieces, so the host code reads them by name.
type savingsTracker struct {
	*accrec.Recorder
	thinking *thinkcount.Table
	clients  *sync.Map
	// onEnd, when set, is called with each record after EndRequest. Tests only.
	onEnd func(*accountant.Request, endRecord)
	// written is the last state file's bytes, for save to skip an unchanged write.
	written lastSaved
}

// endRecord is what end gave EndRequest (accrec.EndRecord).
type endRecord = accrec.EndRecord

func newSavingsTracker() *savingsTracker {
	r := accrec.NewRecorder()
	s := &savingsTracker{Recorder: r, thinking: r.Thinking, clients: &r.Clients}
	r.OnEnd = func(req *accountant.Request, rec endRecord) {
		if s.onEnd != nil {
			s.onEnd(req, rec)
		}
	}
	return s
}

// recordThinking stores the thinking counts of one billed response. Nil-safe.
func (s *savingsTracker) recordThinking(r thinkcount.Response) {
	if s != nil {
		s.Recorder.RecordThinking(r)
	}
}

// noteRescueFactsMissing counts one priced request that followed pings and found no ping facts. Nil-safe.
func (s *savingsTracker) noteRescueFactsMissing() {
	if s != nil {
		s.Recorder.NoteRescueFactsMissing()
	}
}

// RescueFactsMissing is the count noteRescueFactsMissing keeps. Nil-safe.
func (s *savingsTracker) RescueFactsMissing() int64 {
	if s == nil {
		return 0
	}
	return s.Recorder.RescueFactsMissing()
}

// drop forgets the client hash of a request that will never end. Nil-safe.
func (s *savingsTracker) drop(req *accountant.Request) {
	if s != nil {
		s.Recorder.Drop(req)
	}
}

func (s *savingsTracker) begin(meta accountant.RequestMeta, client []byte) *accountant.Request {
	return s.Recorder.Begin(meta, client)
}

func (s *savingsTracker) end(req *accountant.Request, th accountant.Thread, forwarded []byte, ops []accountant.Operation, facts accountant.EntryFacts) endRecord {
	return s.Recorder.End(req, th, forwarded, ops, facts)
}

// usage converts the proxy's Usage to the Recorder's.
func (u Usage) accrec() accrec.Usage {
	return accrec.Usage{FreshInput: u.FreshInput, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CacheWrite1h: u.CacheWrite1h}
}

func (s *savingsTracker) computeSafe(req *accountant.Request, u Usage) (accountant.Result, bool) {
	return s.Recorder.ComputeSafe(req, u.accrec())
}

func (s *savingsTracker) compute(req *accountant.Request, u Usage) accountant.Result {
	return s.Recorder.Compute(req, u.accrec())
}
