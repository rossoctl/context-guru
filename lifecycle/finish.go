package lifecycle

import (
	"log/slog"

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
	// RatioTokens and RatioBytes are the request ratio of this turn: the provider's billed tokens of the forwarded
	// text and its bytes (accountant.Ratio.Text), 0 when the host has none. Kept for the thread's next turn (#1533).
	RatioTokens, RatioBytes int
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
	// Counters runs after the warm sweep's question and before the billed-input writes. The host
	// books its SSE and usage counters here.
	Counters func()
}

// Finish runs the steps after the response, in their order:
//
//  1. Touch (keep-alive), then Ledger (tool deferral);
//  2. release a cache-aware summary the turn deferred because its cache was cold;
//  3. run the warm sweep's question, if the trigger registered one;
//  4. Counters (the host's SSE and usage counters);
//  5. only when usage is OK: keep the billed input for the next turn, under the session and the
//     thread, and, in observe mode, under the shadow store too.
//
// A response without usage leaves the previous turn's figure in place: a zero would read as
// "unknown" and shut the fraction gate.
func Finish(f Fin) {
	// Fail open: one recover per step, so a panic in one step costs that step only and the steps after it still run.
	if f.Touch != nil {
		failOpen("touch", f.Touch)
	}
	if f.Ledger != nil {
		failOpen("ledger", f.Ledger)
	}
	// Harmless, and nearly free, on a session that never deferred anything. A failed forward
	// resolves the deferral too: see offload.ResolveDeferredCacheAwareSummary.
	failOpen("deferred summary", func() { offload.ResolveDeferredCacheAwareSummary(f.Session, f.ThreadID) })
	failOpen("warm question", func() { offload.ResolveWarmQuestion(f.Session, f.ThreadID, f.Forwarded, f.CacheRead) })
	if f.Counters != nil {
		failOpen("counters", f.Counters)
	}
	if !f.UsageOK {
		return
	}
	failOpen("billed input", func() {
		apply.RecordBilledInput(f.Store, f.Session, f.Billed)
		apply.RecordBilledInputThread(f.Store, f.Session, f.ThreadID, f.Billed)
		apply.RecordRatioThread(f.Store, f.Session, f.ThreadID, f.RatioTokens, f.RatioBytes)
	})
	// Observe mode needs its own record. The enforced path runs no pipeline there, so Session is
	// empty and the write above does nothing. The off-path run reads the shadow store, which
	// nothing else writes the billed input into: without this the fraction gate reads "unknown"
	// on every turn and the projection is a permanent zero.
	if f.Mode == components.ModeObserve && f.Shadow != nil && f.ObserveSession != nil {
		failOpen("shadow billed input", func() {
			if sid := f.ObserveSession(); sid != "" {
				apply.RecordBilledInput(f.Shadow, sid, f.Billed)
				obsThread := f.ThreadID
				if f.ObserveThread != nil {
					obsThread = f.ObserveThread(sid).ID
				}
				apply.RecordBilledInputThread(f.Shadow, sid, obsThread, f.Billed)
			}
		})
	}
}

// failOpen runs one step of Finish. A panic is logged and drops that step only (issue #1510).
func failOpen(step string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("lifecycle: a step after the response panicked; it is skipped and the next step runs", "step", step, "panic", p)
		}
	}()
	fn()
}
