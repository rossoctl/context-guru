// Package thinkcount keeps the token count of each thinking block a model made, so the accountant can
// price a block at its own count when the client sends it back (accountant design D16, issue #1187).
//
// A response reports one figure for its reasoning: usage.output_tokens_details.thinking_tokens
// (Anthropic). A host reads that figure and the signature of each new thinking block from the
// response (FromBody, or a Collector on a stream), and records them in a Table. On the next request
// the host builds its bodies with accountant.NewBodyCounted(..., table.Lookup).
//
// Data dependence: metadata only. The table holds a 64-bit hash of each signature and an integer. A
// signature is read only to hash it and take its length; neither the signature nor any text is kept.
package thinkcount

import (
	"encoding/json"
	"strconv"
	"sync"

	"github.com/rossoctl/accountant"
)

// DefaultMax is the number of blocks a Table keeps. A Claude Code turn makes about one block, so
// this is some thousands of turns across all sessions. An entry costs about 30 bytes in memory and in
// the state file.
const DefaultMax = 20000

// Response is what one response says about its thinking blocks: the opaque string of each new block
// (a thinking signature, or the data of a redacted_thinking block), in order, and the response's
// thinking tokens. HasTokens is false when the usage did not report the figure.
type Response struct {
	Opaques   []string
	Tokens    int
	HasTokens bool
}

// maxRecord is the most blocks one response may record. A real response makes a few; more is a client
// or an upstream that could push every other entry out of a shared table, so its counts are not kept.
const maxRecord = 64

// Countable says whether the response gives a count to record. A count of 0 is not one: a block with a
// signature holds some thinking, so 0 means the figure is missing, not that the block is free.
func (r Response) Countable() bool {
	return r.HasTokens && r.Tokens > 0 && len(r.Opaques) > 0 && len(r.Opaques) <= maxRecord
}

// Table is a bounded map from accountant.OpaqueKey to a token count. It is safe for concurrent use.
// When it is full, the oldest entry goes first.
type Table struct {
	mu    sync.Mutex
	max   int
	m     map[uint64]int32
	order []uint64 // a ring of the keys in insertion order
	head  int      // the oldest key, once the ring is full
}

// NewTable makes a Table that keeps at most max blocks; max <= 0 means DefaultMax.
func NewTable(max int) *Table {
	if max <= 0 {
		max = DefaultMax
	}
	return &Table{max: max, m: map[uint64]int32{}}
}

// Lookup is the accountant.OpaqueCount of the table. Nil-safe: a nil Table knows no count.
func (t *Table) Lookup(key uint64) (int, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.m[key]
	return int(n), ok
}

// Record stores the counts of one response: its thinking tokens split between its blocks by length
// (accountant.SplitOpaqueTokens). It does nothing for a response with no count. Nil-safe.
func (t *Table) Record(r Response) {
	if t == nil || !r.Countable() {
		return
	}
	parts := accountant.SplitOpaqueTokens(r.Opaques, r.Tokens)
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, s := range r.Opaques {
		if s != "" {
			t.put(accountant.OpaqueKey(s), parts[i])
		}
	}
}

// put stores one count. The caller holds mu.
func (t *Table) put(key uint64, tokens int) {
	if tokens > 1<<31-1 {
		tokens = 1<<31 - 1
	}
	if _, ok := t.m[key]; ok { // the same block again: keep its place, take the new count
		t.m[key] = int32(tokens)
		return
	}
	if len(t.order) < t.max {
		t.order = append(t.order, key)
	} else {
		delete(t.m, t.order[t.head])
		t.order[t.head] = key
		t.head = (t.head + 1) % t.max
	}
	t.m[key] = int32(tokens)
}

// Len is the number of blocks the table holds. Nil-safe.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// MarshalJSON writes the entries oldest first, as [key, tokens] pairs. A key is a decimal string, so
// a JavaScript reader does not round it.
func (t *Table) MarshalJSON() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([][2]json.RawMessage, 0, len(t.order))
	for i := range t.order {
		k := t.order[(t.head+i)%len(t.order)]
		out = append(out, [2]json.RawMessage{json.RawMessage(strconv.Quote(strconv.FormatUint(k, 10))),
			json.RawMessage(strconv.Itoa(int(t.m[k])))})
	}
	return json.Marshal(out)
}

// UnmarshalJSON adds the entries MarshalJSON wrote, oldest first, so the newest survive when the table
// is smaller than the file. A malformed entry fails the whole read and leaves the table unchanged. A
// negative count is skipped.
func (t *Table) UnmarshalJSON(b []byte) error {
	var raw [][2]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	type entry struct {
		key    uint64
		tokens int
	}
	in := make([]entry, 0, len(raw))
	for _, p := range raw {
		var ks string
		var e entry
		if err := json.Unmarshal(p[0], &ks); err != nil {
			return err
		}
		if err := json.Unmarshal(p[1], &e.tokens); err != nil {
			return err
		}
		k, err := strconv.ParseUint(ks, 10, 64)
		if err != nil {
			return err
		}
		e.key = k
		in = append(in, e)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[uint64]int32{}
	}
	if t.max <= 0 {
		t.max = DefaultMax
	}
	for _, e := range in {
		if e.tokens >= 0 {
			t.put(e.key, e.tokens)
		}
	}
	return nil
}
