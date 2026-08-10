package modes

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Pool is the bounded off-path worker pool for async and observe mode: one queue,
// a fixed number of drain goroutines, owned by the host (the proxy) rather than
// spawned per request.
//
// The shape is headroom's BackgroundCompressor, ported and extended:
//
//   - dedup by key, with the pending slot claimed BEFORE the job becomes
//     observable in the queue, so dedup is atomic against a concurrent enqueue of
//     the same key;
//   - a bounded queue that DROPS rather than blocks — the request has already been
//     forwarded, so a drop costs savings, never correctness, and the request path
//     must never wait on this;
//   - no request-coupled deadline: jobs run under the pool's own context, not the
//     inbound request's, which is cancelled the moment the response is written;
//   - fail-open on every path, including a panicking job;
//   - the FULL counter tuple exposed, dropped and stale_discarded included.
//     headroom's dashboard shows only `queued`, which hides exactly the counter that
//     says "we silently gave up savings".
type Pool struct {
	q       chan job
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool

	mu             sync.Mutex
	pending        map[string]struct{}
	processed      int64
	dropped        int64
	errors         int64
	staleDiscarded int64
}

type job struct {
	key string
	run func(context.Context)
}

// Stats is the async queue's counter tuple, surfaced whole in /stats.
type Stats struct {
	Queued         int64 `json:"queued"`
	Pending        int64 `json:"pending"`
	Processed      int64 `json:"processed"`
	Dropped        int64 `json:"dropped"`
	Errors         int64 `json:"errors"`
	StaleDiscarded int64 `json:"stale_discarded"`
}

// Defaults for the pool's two knobs. One worker is deliberate: the expensive part of
// a compaction is an LLM call, and one in flight per process keeps the cheap-model
// spend and the gateway's rate limit predictable while still removing the latency from
// the request path.
const (
	DefaultMaxQueue = 256
	DefaultWorkers  = 1
)

// NewPool builds and starts a pool. maxQueue/workers <= 0 take the defaults.
// Call Stop to shut it down; a stopped pool drops every later enqueue.
func NewPool(maxQueue, workers int) *Pool {
	if maxQueue <= 0 {
		maxQueue = DefaultMaxQueue
	}
	if workers <= 0 {
		workers = DefaultWorkers
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		q:       make(chan job, maxQueue),
		ctx:     ctx,
		cancel:  cancel,
		started: true,
		pending: map[string]struct{}{},
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.drain()
	}
	return p
}

// Enqueue queues run under key, returning false if it was dropped — because the key
// is already queued or in flight (dedup / coalesced supersession), the queue is full,
// or the pool is stopped. Never blocks.
func (p *Pool) Enqueue(key string, run func(context.Context)) bool {
	if p == nil || run == nil {
		return false
	}
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return false
	}
	if _, dup := p.pending[key]; dup {
		p.mu.Unlock()
		return false // already queued or running — the newer turn will enqueue the next generation
	}
	// Claim the slot BEFORE the job is observable in the queue, so a concurrent
	// Enqueue of the same key cannot slip past the dedup check.
	p.pending[key] = struct{}{}
	p.mu.Unlock()

	select {
	case p.q <- job{key: key, run: run}:
		return true
	default:
		p.mu.Lock()
		delete(p.pending, key)
		p.dropped++
		p.mu.Unlock()
		slog.Warn("context-guru: async queue full, dropping compaction job (request already forwarded)",
			"key", key, "max_queue", cap(p.q))
		return false
	}
}

func (p *Pool) drain() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case j, ok := <-p.q:
			if !ok {
				return
			}
			p.runOne(j)
		}
	}
}

func (p *Pool) runOne(j job) {
	defer func() {
		p.mu.Lock()
		delete(p.pending, j.key)
		if r := recover(); r != nil {
			p.errors++
			p.mu.Unlock()
			slog.Error("context-guru: recovered from panic in async compaction job", "key", j.key, "panic", r)
			return
		}
		p.processed++
		p.mu.Unlock()
	}()
	j.run(p.ctx)
}

// RecordStale notes that a completed job's result was thrown away because a newer
// generation had already landed. Counted separately from `dropped` (never ran) and
// `errors` (ran and failed): a rising stale count means turns arrive faster than
// compaction finishes, which is a tuning signal, not a fault.
func (p *Pool) RecordStale() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.staleDiscarded++
	p.mu.Unlock()
}

// Stats returns the counter tuple.
func (p *Pool) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Queued:         int64(len(p.q)),
		Pending:        int64(len(p.pending)),
		Processed:      p.processed,
		Dropped:        p.dropped,
		Errors:         p.errors,
		StaleDiscarded: p.staleDiscarded,
	}
}

// stopGrace bounds how long Stop waits for an in-flight job. Cancelling the context asks
// the job to stop, but a compaction sitting in an HTTP call to the cheap model only
// notices when that call returns, and its client timeout is minutes. Since the job's
// result is pure savings that nobody is waiting for, a shutdown must not inherit that
// timeout — it gives up and lets the goroutine die with the process.
const stopGrace = 2 * time.Second

// Stop cancels the pool's context and waits briefly for its workers to exit. Queued jobs
// are abandoned — they were pure savings, and the requests they belonged to went out long
// ago. Returns false if a worker was still running at the grace deadline (its goroutine
// is left to exit on its own; nothing depends on its result). Idempotent.
func (p *Pool) Stop() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return true
	}
	p.started = false
	p.mu.Unlock()
	p.cancel()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(stopGrace):
		slog.Warn("context-guru: async worker still busy at shutdown; abandoning its result",
			"grace", stopGrace)
		return false
	}
}
