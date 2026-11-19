package proxy

import (
	"context"
	"sync/atomic"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/keepalive"
)

// The early wait-cause scan (#1225, #1227).
//
// The keeper plans a turn's pings at response time, from the open-task counts of the request BEFORE
// (the dashboard writer updates them a moment later). A turn that starts background work then reads as
// "a person": the person ping count, the 80k prefix floor, the time windows.
//
// To plan from this turn's own body, the handler starts ONE goroutine when the request goes upstream. It
// runs waitcause.ScanBody on the pristine body: a pure function, no shared state written. At response
// end the keeper takes the result with a NON-blocking check: ready, the counts join the plan; not ready,
// nothing waits and the plan is today's. The dashboard writer reuses the same result for its in-order
// tracker update when it is ready, else scans as before. The writer stays the only writer of the tracker.
//
// Bounds, none of them a timer: the work bound in ScanBody (bytes, messages), a recover, and a context
// that ends with the request, polled once per message. The goroutine outlives the request by at most the
// message in progress.

// earlyScan is one request's scan. The zero value is not used; see startEarlyScan.
type earlyScan struct {
	done   chan struct{}
	cancel context.CancelFunc
	// res is written once by the scan goroutine before done is closed, and read only after done is seen
	// closed. nil: the scan could not say (the work bound, a panic, cancellation, server-held history).
	res *waitcause.Scan
}

// scanStats counts what happened to the early scan, process-wide. Metadata only.
type scanStats struct {
	used, notReady, unknown atomic.Uint64 // the keeper's plan: counts used / scan not ready / scan said unknown
	writerEarly, writerOwn  atomic.Uint64 // the writer's tracker update: reused the result / scanned itself
}

// startEarlyScan starts the scan goroutine for body and returns at once. ctx should end with the request.
// gate, when non-nil, is called first on the goroutine (a test hook that holds the scan).
func startEarlyScan(parent context.Context, provider bschemas.ModelProvider, body []byte, gate func(context.Context)) *earlyScan {
	ctx, cancel := context.WithCancel(parent)
	s := &earlyScan{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		defer func() { _ = recover() }() // fail open: res stays nil
		if gate != nil {
			gate(ctx)
		}
		s.res = waitcause.ScanBody(func() bool { return ctx.Err() != nil }, provider, body)
	}()
	return s
}

// result returns the scan's result if it is ready and says something, else nil. It never blocks.
func (s *earlyScan) result() *waitcause.Scan {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return s.res
	default:
		return nil
	}
}

// ready reports whether the scan has finished, with or without a result. It never blocks.
func (s *earlyScan) ready() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// stop ends the scan's work in progress. A result already produced stays readable. Nil-safe, idempotent.
func (s *earlyScan) stop() {
	if s != nil {
		s.cancel()
	}
}

// openTasks is the keeper's non-blocking take of the scan at response end, counted in st. nil: the scan
// was not started, is not ready, or said unknown, and the plan is today's.
func (s *earlyScan) openTasks(st *scanStats) *keepalive.TaskCounts {
	if s == nil {
		return nil
	}
	if !s.ready() {
		st.notReady.Add(1)
		return nil
	}
	r := s.result()
	if r == nil {
		st.unknown.Add(1)
		return nil
	}
	st.used.Add(1)
	return &keepalive.TaskCounts{Background: r.OpenBackground, Teammate: r.OpenTeammate}
}
