package thinkcount

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
)

func sse(events ...string) []byte {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return []byte(b.String())
}

// A stream with two thinking blocks, each signature sent in two signature_delta events, in the spaced
// JSON a gateway writes. The usage is in message_delta.
func twoBlockStream(a, b string) []byte {
	return sse(
		`{"type": "message_start", "message": {"usage": {"input_tokens": 9, "output_tokens": 1}}}`,
		`{"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": "", "signature": ""}}`,
		`{"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "hm"}}`,
		`{"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "`+a[:len(a)/2]+`"}}`,
		`{"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "`+a[len(a)/2:]+`"}}`,
		`{"type": "content_block_stop", "index": 0}`,
		`{"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}}`,
		`{"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": "signature_delta is a word"}}`,
		`{"type": "content_block_start", "index": 2, "content_block": {"type": "redacted_thinking", "data": "`+b+`"}}`,
		`{"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 400, "output_tokens_details": {"thinking_tokens": 300}}}`,
		`{"type": "message_stop"}`)
}

func TestCollectorReadsAStreamInAnyChunks(t *testing.T) {
	a, b := strings.Repeat("A", 2000), strings.Repeat("B", 1000)
	stream := twoBlockStream(a, b)
	for _, chunk := range []int{1, 7, 100, len(stream)} {
		var c Collector
		for i := 0; i < len(stream); i += chunk {
			c.Write(stream[i:min(i+chunk, len(stream))])
		}
		r := c.Result()
		if len(r.Opaques) != 2 || r.Opaques[0] != a || r.Opaques[1] != b || r.Tokens != 300 || !r.HasTokens {
			t.Fatalf("chunk %d: %d opaques, tokens %d %v", chunk, len(r.Opaques), r.Tokens, r.HasTokens)
		}
	}
}

func TestFromBodyReadsJSONAndStreams(t *testing.T) {
	a := strings.Repeat("S", 812)
	js := []byte(`{"content":[{"type":"thinking","thinking":"","signature":"` + a + `"},{"type":"text","text":"x"}],` +
		`"usage":{"input_tokens":22,"output_tokens":65,"output_tokens_details":{"thinking_tokens":62}}}`)
	if r := FromBody("application/json", js); len(r.Opaques) != 1 || r.Opaques[0] != a || r.Tokens != 62 || !r.HasTokens {
		t.Fatalf("JSON: %+v", r)
	}
	if r := FromBody("text/event-stream; charset=utf-8", twoBlockStream("xy", "z")); r.Tokens != 300 || len(r.Opaques) != 2 {
		t.Fatalf("stream: %+v", r)
	}
	noTokens := []byte(`{"content":[{"type":"thinking","signature":"abc"}],"usage":{"output_tokens":3}}`)
	if r := FromJSON(noTokens); r.Countable() {
		t.Fatalf("a usage with no thinking_tokens must give no count: %+v", r)
	}
	if r := FromJSON([]byte(`{"content":[{"type":"text","text":"hi"}]}`)); r.Countable() || len(r.Opaques) != 0 {
		t.Fatalf("no thinking: %+v", r)
	}
}

// A line longer than the limit breaks the stream: no count, rather than a count of part of a block.
func TestCollectorGivesUpPastItsLimits(t *testing.T) {
	var c Collector
	c.Write([]byte("data: " + strings.Repeat("x", maxLine+1)))
	c.Write(twoBlockStream("ab", "c"))
	if r := c.Result(); r.Countable() || len(r.Opaques) != 0 {
		t.Fatalf("a broken stream must give nothing: %+v", r)
	}
}

func TestTableRecordsAndLooksUpByKey(t *testing.T) {
	tb := NewTable(0)
	a, b := strings.Repeat("A", 300), strings.Repeat("B", 100)
	tb.Record(Response{Opaques: []string{a, b}, Tokens: 400, HasTokens: true})
	if n, ok := tb.Lookup(accountant.OpaqueKey(a)); !ok || n != 300 {
		t.Fatalf("a: %d %v, want 300", n, ok)
	}
	if n, ok := tb.Lookup(accountant.OpaqueKey(b)); !ok || n != 100 {
		t.Fatalf("b: %d %v, want 100", n, ok)
	}
	if _, ok := tb.Lookup(accountant.OpaqueKey("other")); ok {
		t.Fatalf("an unknown block must have no count")
	}
	tb.Record(Response{Opaques: []string{"c"}, Tokens: 5}) // no thinking_tokens reported
	if tb.Len() != 2 {
		t.Fatalf("a response with no count was recorded: len %d", tb.Len())
	}
	var nilTable *Table
	if _, ok := nilTable.Lookup(1); ok || nilTable.Len() != 0 {
		t.Fatalf("a nil table must know nothing")
	}
	nilTable.Record(Response{Opaques: []string{"x"}, Tokens: 1, HasTokens: true})
}

// The body the accountant builds with the table prices the block at its count.
func TestTableFeedsNewBodyCounted(t *testing.T) {
	sig := strings.Repeat("S", 3000)
	tb := NewTable(0)
	tb.Record(Response{Opaques: []string{sig}, Tokens: 1000, HasTokens: true})
	msg := []byte(`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"` + sig + `"}]}`)
	p := accountant.NewBodyCounted(nil, nil, [][]byte{msg}, tb.Lookup).Pos[0]
	if p.OpaqueCounted != 3000 || p.OpaqueTokens != 1000 {
		t.Fatalf("position %+v, want 3000 counted bytes at 1000 tokens", p)
	}
}

func TestTableDropsTheOldestWhenFull(t *testing.T) {
	tb := NewTable(2)
	for _, s := range []string{"a", "b", "c"} {
		tb.Record(Response{Opaques: []string{s}, Tokens: 1, HasTokens: true})
	}
	if _, ok := tb.Lookup(accountant.OpaqueKey("a")); ok || tb.Len() != 2 {
		t.Fatalf("the oldest must go first: len %d", tb.Len())
	}
	tb.Record(Response{Opaques: []string{"b"}, Tokens: 9, HasTokens: true}) // known: new count, same place
	if n, _ := tb.Lookup(accountant.OpaqueKey("b")); n != 9 || tb.Len() != 2 {
		t.Fatalf("b = %d, len %d", n, tb.Len())
	}
}

func TestTableSurvivesJSON(t *testing.T) {
	tb := NewTable(0)
	for i, s := range []string{"a", "b", "c"} {
		tb.Record(Response{Opaques: []string{s}, Tokens: i + 1, HasTokens: true})
	}
	data, err := json.Marshal(tb)
	if err != nil {
		t.Fatal(err)
	}
	small := NewTable(2) // smaller than the file: the newest survive
	if err := json.Unmarshal(data, small); err != nil {
		t.Fatal(err)
	}
	if _, ok := small.Lookup(accountant.OpaqueKey("a")); ok {
		t.Fatalf("the oldest entry must not survive a smaller table")
	}
	if n, ok := small.Lookup(accountant.OpaqueKey("c")); !ok || n != 3 {
		t.Fatalf("c = %d %v, want 3", n, ok)
	}
	if err := json.Unmarshal([]byte(`[["x",1]]`), NewTable(0)); err == nil {
		t.Fatalf("a malformed key must fail")
	}
}

func TestCollectorReadsJSONInChunks(t *testing.T) {
	js := []byte(`{"content":[{"type":"thinking","signature":"` + strings.Repeat("S", 900) + `"}],` +
		`"usage":{"output_tokens_details":{"thinking_tokens":62}}}`)
	c := NewCollector("application/json")
	for i := 0; i < len(js); i += 50 {
		c.Write(js[i:min(i+50, len(js))])
	}
	if r := c.Result(); len(r.Opaques) != 1 || r.Tokens != 62 {
		t.Fatalf("JSON in chunks: %+v", r)
	}
	big := NewCollector("application/json")
	big.Write(make([]byte, maxJSON+1))
	if r := big.Result(); r.Countable() {
		t.Fatalf("a body past the limit must give nothing")
	}
}

// A count of 0 is a missing figure, and a response with more blocks than a real one makes is not kept.
func TestCountableRefusesZeroAndTooManyBlocks(t *testing.T) {
	if (Response{Opaques: []string{"a"}, Tokens: 0, HasTokens: true}).Countable() {
		t.Fatalf("a count of 0 must not be recorded")
	}
	many := make([]string, maxRecord+1)
	for i := range many {
		many[i] = strings.Repeat("x", i+1)
	}
	if (Response{Opaques: many, Tokens: 1000, HasTokens: true}).Countable() {
		t.Fatalf("%d blocks must not be recorded", len(many))
	}
	if !(Response{Opaques: many[:maxRecord], Tokens: 1000, HasTokens: true}).Countable() {
		t.Fatalf("%d blocks must be recorded", maxRecord)
	}
}

// A stream with CRLF framing is read like one with LF (#1499).
func TestCollectorReadsCRLFFraming(t *testing.T) {
	stream := strings.ReplaceAll(string(twoBlockStream("AAA", "BBB")), "\n", "\r\n")
	var c Collector
	c.Write([]byte(stream))
	if r := c.Result(); len(r.Opaques) != 2 || r.Tokens != 300 {
		t.Fatalf("%d opaques, tokens %d", len(r.Opaques), r.Tokens)
	}
}
