package dash

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// Lifecycle signals (issue #424, #427): the per-request facts that explain WHY a thread
// went idle, so a keep-alive budget can be set from the cause instead of guessed. Opt-in
// (Options.LifecycleSignals) and stored in a side table, request_lifecycle, so the hot
// requests table is untouched and a deployment that leaves the flag off writes nothing.
//
// Privacy: counts, sizes, hashes, and tool NAMES only. A name is client-supplied text, so it
// goes through metaEnum like every other client enum; tool inputs and tool_result bodies are
// never read beyond their byte length.

// LifecycleReq is what one request body says about the wait it ended. Built from the
// pristine body on the request path, with no session knowledge.
type LifecycleReq struct {
	ToolsHash, SystemHash uint64
	Messages              int
	// PrevToolNames/PrevToolCount: the tool_use blocks of the assistant message the agent
	// is answering, i.e. the PREVIOUS response's calls. Names are metaEnum'd, comma-joined,
	// at most maxLifecycleNames of them; PrevToolCount is the true count.
	PrevToolNames string
	PrevToolCount int
	// ResultCount/ResultBytes/ResultError describe the tool_results answering them: how
	// many, their summed content length in bytes, and whether any is flagged is_error.
	ResultCount, ResultBytes int
	ResultError              bool
}

const maxLifecycleNames = 8

// ScanLifecycle reads a request body once. Fail open: a body it cannot parse yields the
// zero value, and the row simply carries empty facts.
func ScanLifecycle(body []byte) LifecycleReq {
	var q LifecycleReq
	q.ToolsHash = fnvOf(gjson.GetBytes(body, "tools").Raw)
	q.SystemHash = systemHash(gjson.GetBytes(body, "system"))
	var asst gjson.Result
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		q.Messages++
		role := m.Get("role").String()
		switch role {
		case "assistant":
			asst = m
			q.ResultCount, q.ResultBytes, q.ResultError = 0, 0, false
		case "tool": // OpenAI: one message per result
			q.ResultCount++
			q.ResultBytes += len(m.Get("content").Raw)
		default: // Anthropic: results ride in a user message's content blocks
			m.Get("content").ForEach(func(_, b gjson.Result) bool {
				if b.IsObject() && b.Get("type").String() == "tool_result" {
					q.ResultCount++
					q.ResultBytes += len(b.Get("content").Raw)
					if b.Get("is_error").Bool() {
						q.ResultError = true
					}
				}
				return true
			})
		}
		return true
	})
	if asst.Exists() {
		var names []string
		add := func(n string) {
			q.PrevToolCount++
			if len(names) < maxLifecycleNames {
				names = append(names, metaEnum(n))
			}
		}
		asst.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.IsObject() && b.Get("type").String() == "tool_use" {
				add(b.Get("name").String())
			}
			return true
		})
		asst.Get("tool_calls").ForEach(func(_, c gjson.Result) bool {
			add(c.Get("function.name").String())
			return true
		})
		q.PrevToolNames = strings.Join(names, ",")
	}
	return q
}

func fnvOf(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// systemHash digests the system prompt WITHOUT Claude Code's per-request billing header
// block, which carries a changing nonce and would otherwise give every request its own thread.
func systemHash(sys gjson.Result) uint64 {
	if !sys.IsArray() {
		return fnvOf(sys.Raw)
	}
	h := fnv.New64a()
	sys.ForEach(func(_, b gjson.Result) bool {
		if !strings.HasPrefix(b.Get("text").String(), "x-anthropic-billing-header") {
			h.Write([]byte(b.Raw))
		}
		return true
	})
	return h.Sum64()
}

// LifecycleRow is one request_lifecycle row. Gaps are -1 when unknown ("missing is not zero").
type LifecycleRow struct {
	ThreadID, ParentThreadID string
	ThreadSeq                int // 1 = first request seen on this thread
	PrevStopReason           string
	PrevToolNames            string
	PrevToolCount            int
	ResultCount, ResultBytes int
	ResultError              bool
	GapMs, SincePrevEndMs    int64
	GrantedTTL               string // 5m | 1h | mixed | "" (no cache write to read a grant from)
	PingN                    int    // >0 on a keep-alive ping row: its ordinal in the idle span
}

type threadState struct {
	id, key            string
	gen, lastMsgs, seq int
	lastTS, lastEnd    int64
	lastStop           string
}

type sessionState struct {
	firstKey, firstThread string
	lastThread            map[string]string // model -> latest thread, for ping rows
}

// lifecycle tracks threads in memory. State is lost on restart (the first request after
// one reads gap -1), the same amnesia ObserveSplit documents.
type lifecycle struct {
	mu       sync.Mutex
	threads  map[string]*threadState
	sessions map[string]*sessionState
}

func newLifecycle() *lifecycle {
	return &lifecycle{threads: map[string]*threadState{}, sessions: map[string]*sessionState{}}
}

// LifecycleOn reports whether lifecycle signals are enabled.
func (r *Recorder) LifecycleOn() bool { return r != nil && r.life != nil }

// ObserveLifecycle assigns the request to a thread and returns its row. ts is the request
// start, endTS its response end, both epoch ms; stop is THIS response's stop reason,
// remembered as the next request's prev_stop_reason; granted5m/granted1h are the cache
// write tokens by tier.
//
// A thread is (tenant, session, model, tools, system); a message count that falls below the
// thread's last one starts a new generation of it (a fork or a compaction re-sent a shorter
// transcript, which is a different cache entry). ponytail: two forks with identical tools and
// system interleaving in one session share a key and will flip generations; an agent-id header
// (#427) would separate them.
func (r *Recorder) ObserveLifecycle(tenant, session, model string, ts, endTS int64,
	q LifecycleReq, stop string, write5m, write1h int64) *LifecycleRow {
	if !r.LifecycleOn() {
		return nil
	}
	l := r.life
	key := tenant + "\x00" + session + "\x00" + model + "\x00" +
		strconv.FormatUint(q.ToolsHash, 16) + "\x00" + strconv.FormatUint(q.SystemHash, 16)
	sk := tenant + "\x00" + session
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.threads) > 20000 {
		for k, t := range l.threads {
			if ts-t.lastEnd > staleSession {
				delete(l.threads, k)
			}
		}
	}
	if len(l.sessions) > 20000 {
		l.sessions = map[string]*sessionState{}
	}
	t := l.threads[key]
	if t == nil {
		t = &threadState{key: key}
		l.threads[key] = t
	}
	row := &LifecycleRow{GapMs: -1, SincePrevEndMs: -1}
	if t.seq > 0 {
		if q.Messages < t.lastMsgs {
			t.gen++
			t.seq, t.lastStop, t.lastTS, t.lastEnd = 0, "", 0, 0
		}
	}
	if t.seq > 0 {
		row.PrevStopReason = metaEnum(t.lastStop)
		if d := ts - t.lastTS; d >= 0 {
			row.GapMs = d
		}
		if t.lastEnd > 0 && ts >= t.lastEnd {
			row.SincePrevEndMs = ts - t.lastEnd
		}
	}
	sum := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(t.gen)))
	t.id = hex.EncodeToString(sum[:6])
	t.seq++
	t.lastMsgs, t.lastTS, t.lastEnd, t.lastStop = q.Messages, ts, endTS, stop
	row.ThreadID, row.ThreadSeq = t.id, t.seq

	s := l.sessions[sk]
	if s == nil {
		s = &sessionState{firstKey: key, firstThread: t.id, lastThread: map[string]string{}}
		l.sessions[sk] = s
	}
	// Subagent hint: a different tools/system digest than the session's first thread.
	if tail(key) != tail(s.firstKey) {
		row.ParentThreadID = s.firstThread
	}
	s.lastThread[model] = t.id

	row.PrevToolNames, row.PrevToolCount = q.PrevToolNames, q.PrevToolCount
	row.ResultCount, row.ResultBytes, row.ResultError = q.ResultCount, q.ResultBytes, q.ResultError
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

// tail returns the tools+system part of a key.
func tail(key string) string {
	parts := strings.SplitN(key, "\x00", 4)
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

// PingLifecycle links a keep-alive ping row to the session's latest thread on that model.
// The keeper pings per session on this base (per thread comes with #426), so for a session
// with live subagents the link is the thread that spoke last, which is the entry the ping
// most plausibly refreshed.
func (r *Recorder) PingLifecycle(tenant, session, model string, pingN int) *LifecycleRow {
	if !r.LifecycleOn() {
		return nil
	}
	r.life.mu.Lock()
	defer r.life.mu.Unlock()
	row := &LifecycleRow{GapMs: -1, SincePrevEndMs: -1, PingN: pingN}
	if s := r.life.sessions[tenant+"\x00"+session]; s != nil {
		row.ThreadID = s.lastThread[model]
	}
	return row
}

// Lifecycles returns every stored lifecycle row, oldest request first, keyed by request id
// order. It is the read side for analysis scripts and tests; nothing in the UI uses it yet.
func (d *DB) Lifecycles() ([]LifecycleRow, error) {
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT thread_id, parent_thread_id, thread_seq,
		prev_stop_reason, prev_tool_names, prev_tool_count, result_count, result_bytes, result_error,
		gap_ms, since_prev_end_ms, granted_ttl, ping_n FROM request_lifecycle ORDER BY request_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LifecycleRow
	for rows.Next() {
		var l LifecycleRow
		var rerr int
		if err := rows.Scan(&l.ThreadID, &l.ParentThreadID, &l.ThreadSeq, &l.PrevStopReason,
			&l.PrevToolNames, &l.PrevToolCount, &l.ResultCount, &l.ResultBytes, &rerr,
			&l.GapMs, &l.SincePrevEndMs, &l.GrantedTTL, &l.PingN); err != nil {
			return nil, err
		}
		l.ResultError = rerr != 0
		out = append(out, l)
	}
	return out, rows.Err()
}
