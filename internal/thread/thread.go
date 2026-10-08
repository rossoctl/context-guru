// Package thread tells apart the THREADS of one session: the main agent, each subagent, each
// fork. Every thread has its own prompt and its own provider cache entry, so the two places that
// reason about one cache entry — the dashboard's miss attribution and the idle keep-alive — must
// key on session + thread, not on the session alone (issue #423).
//
// A thread id is found in this order:
//
//  1. The agent's own header. Claude Code sends `x-claude-code-agent-id` on every subagent
//     request and omits it on the main thread (seen on the wire with 2.1.294). The proxy reads
//     the header and passes it in as the hint.
//  2. Prefix continuity, for every request without that header and for every agent and provider
//     that never sends it. A request continues thread T when T's last message list is a prefix
//     of the new request's message list — the same rule the provider cache applies, so requests
//     are grouped exactly as the cache groups them.
//  3. The session itself (thread id ""), when neither gives an answer. That is the behaviour
//     before this package existed, so a wrong guess can never be worse than no guess.
//
// The first thread of a session without a header also gets the id "". A single-thread session — every Codex or
// Bob Shell session, and every Claude Code session that never starts a subagent — therefore keys
// exactly as it did before.
package thread

import (
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// Primary is the id of a session's first thread, and of every request whose thread cannot be
// told. Key(session, Primary) is the session itself.
const Primary = ""

// Key is the map key the recorder and the keeper use for one thread: the session itself for the
// primary thread, so single-thread traffic keys exactly as it did before threads existed.
func Key(session, thread string) string {
	if thread == Primary {
		return session
	}
	return session + "\x00" + thread
}

// Source says which rule produced a thread id.
type Source string

const (
	SourceHeader  Source = "header"  // the agent named the thread
	SourcePrefix  Source = "prefix"  // matched, or started, by prefix continuity
	SourceSession Source = "session" // last resort: no usable message list
)

// Result is one request's thread.
type Result struct {
	ID     string
	Source Source
	// Disagree is set when the agent's header named a KNOWN thread but prefix continuity matched
	// a different one. The header wins; the caller logs this so a drift between the two paths is
	// visible rather than silent.
	Disagree bool
}

const (
	// maxThreadsPerSession bounds one session's thread list. A Claude Code session makes many
	// small auxiliary requests, each with its own first message, so without a bound the list
	// would grow for the life of the session. The oldest thread goes first: a thread silent long
	// enough to be the oldest of 64 has lost its provider cache anyway.
	maxThreadsPerSession = 64
	// maxSessions and staleAfter bound the session map the same way dash.Recorder bounds its own
	// (by age, well past every provider cache lifetime this repo knows of).
	maxSessions = 20000
	staleAfter  = 2 * time.Hour
	// maxHeaderID refuses a header value too long to be an agent id.
	maxHeaderID = 128
)

type threadState struct {
	id string
	// n and sum describe the thread's LAST request: its message count and the cumulative
	// fingerprint after its last message. A new request continues the thread when its own
	// fingerprint after message n equals sum.
	n    int
	sum  uint64
	last time.Time
	// compacting marks a thread whose last request was the agent's own compaction request. The
	// agent's next request starts from a summary, so it matches no thread by prefix; it is still
	// the same thread, and it inherits this id.
	compacting bool
}

type sessionState struct {
	threads []*threadState
	last    time.Time
}

// Tracker holds each live session's threads. Safe for concurrent use. The zero value is not
// usable; call New.
type Tracker struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
	now      func() time.Time
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{sessions: map[string]*sessionState{}, now: time.Now}
}

// ValidHeaderID reports whether an agent-id header value is usable. Empty, over-long, or
// containing anything outside [A-Za-z0-9_.:-] is not: such a value falls back to prefix
// continuity rather than becoming a key.
func ValidHeaderID(v string) bool {
	if v == "" || len(v) > maxHeaderID {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// Resolve finds the thread of one request.
//
// scope is the tenant + session the thread belongs to. agentID is the agent's own thread header
// ("" when absent; the caller validates it with ValidHeaderID). body is the request AS THE AGENT
// SENT IT — before any compaction, which would otherwise break the very prefix this matches on.
// agentCompaction marks the agent's own compaction request (see the compacting field).
//
// A nil tracker, and an empty scope, give the session itself.
func (t *Tracker) Resolve(scope, agentID string, body []byte, agentCompaction bool) Result {
	if t == nil || scope == "" {
		return Result{ID: Primary, Source: SourceSession}
	}
	sums, ok := Fingerprint(body)
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[scope]
	if s == nil {
		if len(t.sessions) >= maxSessions {
			t.pruneLocked(now)
		}
		s = &sessionState{}
		t.sessions[scope] = s
	}
	s.last = now

	var match *threadState
	if ok {
		match = s.longestPrefix(sums)
	}

	var res Result
	switch {
	case agentID != "":
		res = Result{ID: "a:" + agentID, Source: SourceHeader}
		if known := s.find(res.ID); known != nil && match != nil && match != known {
			res.Disagree = true
		}
	case !ok:
		// No message list to match on (a server-held Responses history, an unparseable body).
		// Today's behaviour: the session itself.
		return Result{ID: Primary, Source: SourceSession}
	case match != nil:
		res = Result{ID: match.id, Source: SourcePrefix}
	default:
		res = Result{ID: s.newID(sums), Source: SourcePrefix}
	}
	if !ok {
		// A header thread with no message list: keep its id, record nothing to match on.
		return res
	}
	th := s.find(res.ID)
	if th == nil {
		th = &threadState{id: res.ID}
		s.add(th)
	}
	th.n, th.sum, th.last = len(sums), sums[len(sums)-1], now
	th.compacting = agentCompaction && res.Source == SourcePrefix
	return res
}

// newID names a thread that matched nothing. The first thread of a session is the primary; the
// successor of a compacting thread inherits its id; anything else is named after its own
// fingerprint, which differs from every other thread's (a fork has its parent's prefix but not
// its parent's full message list).
func (s *sessionState) newID(sums []uint64) string {
	if !s.hasPrefixThread() {
		return Primary
	}
	for _, th := range s.threads {
		if th.compacting {
			th.compacting = false
			return th.id
		}
	}
	// The same fingerprint as a thread that has since moved on is a resent request from that
	// thread's past; the caller then joins it, which is right, since the prefix is the same.
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], sums[len(sums)-1])
	return "p:" + hex.EncodeToString(b[:6])
}

// hasPrefixThread reports whether the session has a thread not named by a header. Header threads
// do not count: after a restart a subagent may speak first, and the main thread, which carries no
// header, must still get the primary id.
func (s *sessionState) hasPrefixThread() bool {
	for _, th := range s.threads {
		if !strings.HasPrefix(th.id, "a:") {
			return true
		}
	}
	return false
}

// longestPrefix returns the thread whose last message list is the longest prefix of sums, the
// most recent on a tie. Longest, because a fork shares its parent's earlier prefix: the parent's
// own next request matches the parent's full list, the fork's matches only the shorter one.
func (s *sessionState) longestPrefix(sums []uint64) *threadState {
	var best *threadState
	for _, th := range s.threads {
		if th.n == 0 || th.n > len(sums) || sums[th.n-1] != th.sum {
			continue
		}
		if best == nil || th.n > best.n || (th.n == best.n && th.last.After(best.last)) {
			best = th
		}
	}
	return best
}

func (s *sessionState) find(id string) *threadState {
	for _, th := range s.threads {
		if th.id == id {
			return th
		}
	}
	return nil
}

func (s *sessionState) add(th *threadState) {
	if len(s.threads) >= maxThreadsPerSession {
		oldest := 0
		for i, o := range s.threads {
			if o.last.Before(s.threads[oldest].last) {
				oldest = i
			}
		}
		s.threads = append(s.threads[:oldest], s.threads[oldest+1:]...)
	}
	s.threads = append(s.threads, th)
}

// pruneLocked drops sessions silent longer than staleAfter, and empties the map if that frees
// nothing. Caller holds t.mu.
func (t *Tracker) pruneLocked(now time.Time) {
	for k, s := range t.sessions {
		if now.Sub(s.last) > staleAfter {
			delete(t.sessions, k)
		}
	}
	if len(t.sessions) >= maxSessions {
		t.sessions = map[string]*sessionState{}
	}
}

// Fingerprint returns the cumulative fingerprint of a request's message list: element i is the
// hash of messages[0..i]. ok is false when the body carries no message list to match on — no
// `messages` or `input` array, or a Responses request that continues server-held history
// (`previous_response_id`), whose `input` is only the newest turn.
//
// The fingerprint covers what decides whether the agent CONTINUED a conversation, and leaves out
// what the agent rewrites in place without starting a new one:
//
//   - `cache_control` markers: an agent moves its breakpoint to the newest message every turn.
//   - thinking and reasoning blocks: their signatures and encrypted payloads carry no thread
//     identity, and a client may drop them from earlier turns.
//   - the CONTENT of a tool result, kept by its call id only: Claude Code clears old tool results
//     ("microcompact") without starting a new thread.
//
// The system prompt and the tools are left out on purpose. A thread whose system prompt changes
// is still the same thread — that change is a prefix change, and it must be labelled as one.
func Fingerprint(body []byte) ([]uint64, bool) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() || root.Get("previous_response_id").String() != "" {
		return nil, false
	}
	items := root.Get("messages")
	if !items.IsArray() {
		items = root.Get("input")
	}
	h := fnv.New64a()
	switch {
	case items.Type == gjson.String:
		// Responses accepts a bare string as the whole input: one user message.
		canon(h, items)
		return []uint64{h.Sum64()}, true
	case !items.IsArray():
		return nil, false
	}
	var sums []uint64
	items.ForEach(func(_, m gjson.Result) bool {
		h.Write([]byte{0x1e}) // record separator, so [ab][c] and [a][bc] differ
		canon(h, m)
		sums = append(sums, h.Sum64())
		return true
	})
	return sums, len(sums) > 0
}

// canon writes v's thread-identifying shape to h. See Fingerprint for what it leaves out.
func canon(h hash.Hash64, v gjson.Result) {
	switch {
	case v.IsObject():
		switch v.Get("type").String() {
		case "thinking", "redacted_thinking", "reasoning":
			h.Write([]byte("~"))
			return
		case "tool_result":
			h.Write([]byte("tool_result:"))
			h.Write([]byte(v.Get("tool_use_id").String()))
			return
		case "function_call_output": // Responses
			h.Write([]byte("function_call_output:"))
			h.Write([]byte(v.Get("call_id").String()))
			return
		}
		if v.Get("role").String() == "tool" { // OpenAI chat
			h.Write([]byte("tool:"))
			h.Write([]byte(v.Get("tool_call_id").String()))
			return
		}
		h.Write([]byte("{"))
		v.ForEach(func(k, val gjson.Result) bool {
			if k.Str == "cache_control" {
				return true
			}
			h.Write([]byte(k.Str))
			h.Write([]byte(":"))
			canon(h, val)
			h.Write([]byte(","))
			return true
		})
		h.Write([]byte("}"))
	case v.IsArray():
		h.Write([]byte("["))
		v.ForEach(func(_, val gjson.Result) bool {
			canon(h, val)
			h.Write([]byte(","))
			return true
		})
		h.Write([]byte("]"))
	default:
		h.Write([]byte(v.Raw))
	}
}
