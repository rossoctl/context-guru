package lifecycle

import (
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
)

// Fin is the input of Finish: the facts of one finished request and the steps only the host can do.
type Fin struct {
	// Store is the tenant's store. Shadow is the store the observe-mode off-path run reads. It may be nil.
	Store, Shadow store.Store
	Mode          components.Mode
	// Session and ThreadID are Trace.Session and the thread of the request. Every write below
	// uses this pair, or state is written under one key and read under another.
	Session, ThreadID string
	// UsageOK says the response reported usage. Billed is fresh + cache read + cache write input.
	UsageOK bool
	Billed  int64
	// Forwarded says the upstream answered with a 2xx status. CacheRead is the cache-read tokens of the response.
	Forwarded bool
	CacheRead int
	// ObserveSession derives the session id the shadow pipeline uses, for an observe-mode request
	// (where Session is empty). nil or "" skips the shadow record. ObserveThread is the thread
	// resolver the off-path run uses; nil keeps ThreadID.
	ObserveSession func() string
	ObserveThread  func(session string) thread.Result

	// Touch hands the finished request to the keep-alive. It runs first.
	Touch func()
	// Ledger books the request in the tool-deferral ledger.
	Ledger func()
}

// Finish runs the steps after the response, in their order:
//
//  1. Touch (keep-alive), then Ledger (tool deferral);
//  2. release a cache-aware summary the turn deferred because its cache was cold;
//  3. run the warm sweep's question, if the trigger registered one;
//  4. only when usage is OK: keep the billed input for the next turn, under the session and the
//     thread, and, in observe mode, under the shadow store too.
//
// A response without usage leaves the previous turn's figure in place: a zero would read as
// "unknown" and shut the fraction gate.
func Finish(f Fin) {
	if f.Touch != nil {
		f.Touch()
	}
	if f.Ledger != nil {
		f.Ledger()
	}
	// Harmless, and nearly free, on a session that never deferred anything. A failed forward
	// resolves the deferral too: see offload.ResolveDeferredCacheAwareSummary.
	offload.ResolveDeferredCacheAwareSummary(f.Session, f.ThreadID)
	offload.ResolveWarmQuestion(f.Session, f.ThreadID, f.Forwarded, f.CacheRead)
	if !f.UsageOK {
		return
	}
	apply.RecordBilledInput(f.Store, f.Session, f.Billed)
	apply.RecordBilledInputThread(f.Store, f.Session, f.ThreadID, f.Billed)
	// Observe mode needs its own record. The enforced path runs no pipeline there, so Session is
	// empty and the write above does nothing. The off-path run reads the shadow store, which
	// nothing else writes the billed input into: without this the fraction gate reads "unknown"
	// on every turn and the projection is a permanent zero.
	if f.Mode == components.ModeObserve && f.Shadow != nil && f.ObserveSession != nil {
		if sid := f.ObserveSession(); sid != "" {
			apply.RecordBilledInput(f.Shadow, sid, f.Billed)
			obsThread := f.ThreadID
			if f.ObserveThread != nil {
				obsThread = f.ObserveThread(sid).ID
			}
			apply.RecordBilledInputThread(f.Shadow, sid, obsThread, f.Billed)
		}
	}
}
