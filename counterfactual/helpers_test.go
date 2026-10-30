package counterfactual

import (
	"strings"
	"testing"
	"time"
)

// blk is one position of a test body: an ID that makes its content distinct, and its size. The
// size is in bytes, and the tests bill one token per byte, so the token ratio is exactly 1.
type blk struct {
	id string
	n  int
}

func b(id string, n int) blk { return blk{id, n} }

func content(bs []blk) [][]byte {
	out := make([][]byte, len(bs))
	for i, x := range bs {
		out[i] = []byte(x.id + strings.Repeat(".", x.n-len(x.id)))
	}
	return out
}

func mk(tools, system, msgs []blk) Body {
	return NewBody(content(tools), content(system), content(msgs))
}

// msgs returns n messages "m0".. of size each.
func msgs(n, size int) []blk {
	out := make([]blk, n)
	for i := range out {
		out[i] = b("m"+itoa(i), size)
	}
	return out
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

func with(bs []blk, extra ...blk) []blk { return append(append([]blk(nil), bs...), extra...) }

func pos(s Section, i int) Position { return Position{Section: s, Index: i} }
func sys(i int) Position            { return pos(SectionSystem, i) }
func msg(i int) Position            { return pos(SectionMessages, i) }
func tool(i int) Position           { return pos(SectionTools, i) }
func bp5(p Position) Breakpoint     { return Breakpoint{Pos: p, TTL: "5m"} }
func bps5(ps ...Position) []Breakpoint {
	out := make([]Breakpoint, len(ps))
	for i, p := range ps {
		out[i] = bp5(p)
	}
	return out
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

var exact5m = EntryFacts{TTL: 5 * time.Minute, Lifetime: LifetimeExact}

// req is one request of a test scenario.
type req struct {
	at        time.Time
	client    Body
	clientBPs []Breakpoint
	ops       []Operation
	fwd       Body
	fwdBPs    []Breakpoint
	facts     EntryFacts
	usage     Usage
}

func (q req) begin(ts *Trackers) *Request {
	r := ts.BeginRequest(RequestMeta{RequestID: "r", Session: "s1", At: q.at}, q.client, q.clientBPs)
	for _, op := range q.ops {
		r.Add(op)
	}
	fwd, fbp := q.fwd, q.fwdBPs
	if fwd.Pos == nil {
		fwd = q.client // no operator changed the body
	}
	if fbp == nil {
		fbp = q.clientBPs
	}
	r.EndRequest(Thread{ID: "main"}, fwd, fbp, q.facts)
	return r
}

func (q req) run(ts *Trackers) Result { return q.begin(ts).Compute(q.usage) }

func rewrite(id, op, change string, at Position, before, after int) Operation {
	return Operation{ID: id, Kind: KindRewrite, Operator: op, ChangeID: change, At: at,
		SizeBefore: Size{Bytes: before}, SizeAfter: Size{Bytes: after}}
}

func findOp(t *testing.T, res Result, id string) PricedOp {
	t.Helper()
	for _, p := range res.Ops {
		if p.OpID == id {
			return p
		}
	}
	t.Fatalf("no priced op %q in %+v", id, res.Ops)
	return PricedOp{}
}

func noChecks(t *testing.T, res Result) {
	t.Helper()
	if len(res.Checks) != 0 {
		t.Fatalf("Checks = %+v, want none", res.Checks)
	}
}

func hasUnpriced(res Result, reason string) bool {
	for _, u := range res.Unpriced {
		if u.Reason == reason {
			return true
		}
	}
	return false
}

func samePos(a, b *Position) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
