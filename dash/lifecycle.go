package dash

import (
	"sync"
)

// Lifecycle signals (issue #424, #427): the few per-request facts that no other column holds,
// so a keep-alive budget can be set from the cause of an idle gap instead of guessed. Opt-in
// (Options.LifecycleSignals), stored in the side table request_lifecycle so the requests table
// is untouched and a deployment that leaves the flag off writes nothing.
//
// Deliberately NOT recorded here, because main already has them: the thread id and its gap
// since the previous request (requests.thread_id, #426), and the previous response's stop
// reason and tool_use names (requests.waiting_for / wait_tool, #453). A keep-alive ping's
// thread link is its requests.thread_id.
//
// Privacy: counts, byte lengths and a tier label only. No names, no tool input, no result text.

// LifecycleRow is one request_lifecycle row. SincePrevEndMs is -1 when unknown ("missing is
// not zero"): the first request of a thread, or the first one after a proxy restart.
type LifecycleRow struct {
	// ResultCount/ResultBytes/ResultError describe the tool_results the request answers: how
	// many, their summed text length in bytes, and whether any is flagged is_error.
	ResultCount, ResultBytes int
	ResultError              bool
	// SincePrevEndMs is the time from the previous response's END on this thread to this
	// request's START. Both are read from the PROXY clock (the end is stamped when the proxy
	// finishes relaying the response), not the provider's, so a long stream's end is when the
	// last byte reached the proxy.
	SincePrevEndMs int64
	GrantedTTL     string // 5m | 1h | mixed | "" (no cache write to read a grant from)
	PingN          int    // >0 on a keep-alive ping row: its ordinal in the idle span
}

// LifecycleReq is what a request body says about the wait it ended; built off the writer
// goroutine's closure (proxy/lifecycle.go), with no thread knowledge.
type LifecycleReq struct {
	ResultCount, ResultBytes int
	ResultError              bool
}

const lifecycleSweepMs = 60_000

// lifecycle remembers when each thread's last response ended. State is lost on restart (the
// first request after one reads -1), the same amnesia ObserveSplit documents.
type lifecycle struct {
	mu      sync.Mutex
	end     map[string]int64 // tenant\0session\0thread -> epoch ms of the last response end
	sweptAt int64
}

func newLifecycle() *lifecycle { return &lifecycle{end: map[string]int64{}} }

// LifecycleOn reports whether lifecycle signals are enabled.
func (r *Recorder) LifecycleOn() bool { return r != nil && r.life != nil }

// ObserveLifecycle builds the row for one request. ts is its start and endTS its response
// end, epoch ms on the proxy clock; write5m/write1h are the cache-write tokens by tier.
// Stale threads (silent longer than staleSession) are dropped by age, at most once a minute,
// so the cost is amortised and a busy fleet never loses a live thread's state.
func (r *Recorder) ObserveLifecycle(tenant, session, thread string, ts, endTS int64,
	q LifecycleReq, write5m, write1h int64) *LifecycleRow {
	if !r.LifecycleOn() {
		return nil
	}
	l := r.life
	key := tenant + "\x00" + session + "\x00" + thread
	l.mu.Lock()
	defer l.mu.Unlock()
	if endTS-l.sweptAt > lifecycleSweepMs {
		for k, e := range l.end {
			if endTS-e > staleSession {
				delete(l.end, k)
			}
		}
		l.sweptAt = endTS
	}
	row := &LifecycleRow{
		ResultCount: q.ResultCount, ResultBytes: q.ResultBytes, ResultError: q.ResultError,
		SincePrevEndMs: -1,
	}
	if prev, ok := l.end[key]; ok && ts >= prev {
		row.SincePrevEndMs = ts - prev
	}
	l.end[key] = endTS
	switch {
	case write5m > 0 && write1h > 0:
		row.GrantedTTL = "mixed"
	case write1h > 0:
		row.GrantedTTL = "1h"
	case write5m > 0:
		row.GrantedTTL = "5m"
	}
	return row
}

// PingLifecycle is the row of a keep-alive ping: only its ordinal. Its thread is the
// requests.thread_id of the ping's own row.
func (r *Recorder) PingLifecycle(pingN int) *LifecycleRow {
	if !r.LifecycleOn() {
		return nil
	}
	return &LifecycleRow{SincePrevEndMs: -1, PingN: pingN}
}

// Lifecycles returns the stored lifecycle rows of one tenant, oldest request first. It is the
// read side for analysis and tests; nothing in the UI uses it, and it takes the tenant so an
// endpoint built on it cannot leak across tenants.
func (d *DB) Lifecycles(tenant string) ([]LifecycleRow, error) {
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT l.result_count, l.result_bytes,
		l.result_error, l.since_prev_end_ms, l.granted_ttl, l.ping_n
		FROM request_lifecycle l JOIN requests r ON r.id = l.request_id
		WHERE r.tenant_id = ? ORDER BY l.request_id`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LifecycleRow
	for rows.Next() {
		var l LifecycleRow
		var rerr int
		if err := rows.Scan(&l.ResultCount, &l.ResultBytes, &rerr, &l.SincePrevEndMs,
			&l.GrantedTTL, &l.PingN); err != nil {
			return nil, err
		}
		l.ResultError = rerr != 0
		out = append(out, l)
	}
	return out, rows.Err()
}
