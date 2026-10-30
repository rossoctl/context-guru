package counterfactual

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Examples 1 and 2 of examples.md: one cmdfilter change, reported once, priced on a hot request
// where it is new, a hot request where it is cached, and a cold request.

var (
	ex2Sys   = []blk{b("sys", 3000)}
	ex2Msgs  = msgs(9, 3000) // m0..m8: 27,000; with system 30,000
	ex2Op    = rewrite("op1", "context-guru.cmdfilter", "cg:cmdfilter:c40e", msg(9), 8000, 1000)
	ex2Turn1 = with(ex2Msgs, b("m9", 8000))
	ex2Fwd1  = with(ex2Msgs, b("f9", 1000))
)

func ex2Requests() []req {
	return []req{
		{at: t0, client: mk(nil, ex2Sys, ex2Msgs), clientBPs: bps5(sys(0), msg(8)), facts: exact5m,
			usage: Usage{CacheWrite: 30000}},
		{at: t0.Add(10 * time.Second), client: mk(nil, ex2Sys, ex2Turn1), clientBPs: bps5(sys(0), msg(9)),
			ops: []Operation{ex2Op}, fwd: mk(nil, ex2Sys, ex2Fwd1), facts: exact5m,
			usage: Usage{CacheRead: 30000, CacheWrite: 1000}},
		{at: t0.Add(70 * time.Second), client: mk(nil, ex2Sys, with(ex2Turn1, b("m10", 500))), clientBPs: bps5(sys(0), msg(10)),
			fwd: mk(nil, ex2Sys, with(ex2Fwd1, b("m10", 500))), facts: exact5m,
			usage: Usage{CacheRead: 31000, CacheWrite: 500}},
		{at: t0.Add(21 * time.Minute), client: mk(nil, ex2Sys, with(ex2Turn1, b("m10", 500), b("m11", 500))), clientBPs: bps5(sys(0), msg(11)),
			fwd: mk(nil, ex2Sys, with(ex2Fwd1, b("m10", 500), b("m11", 500))), facts: exact5m,
			usage: Usage{CacheWrite: 32000}},
	}
}

func TestFirstRequestOfAThreadIsUnknown(t *testing.T) {
	ts := NewTrackers()
	res := ex2Requests()[0].run(ts)
	if res.Phase != PhaseUnknown || res.Boundary != nil || !hasUnpriced(res, ReasonPenaltyUnknown) {
		t.Fatalf("first request: phase %q boundary %v unpriced %+v", res.Phase, res.Boundary, res.Unpriced)
	}
	if got := ts.tracker(ThreadKey("s1", Thread{ID: "main"})).snapshot().Version; got != 1 {
		t.Fatalf("Compute did not replace the ThreadState: Version = %d", got)
	}
}

func TestANewChangeAfterBStarIsAWrite(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	res := rs[1].run(ts)
	if res.Phase != PhaseHot || !samePos(res.Boundary, ptr(msg(8))) {
		t.Fatalf("phase %q boundary %v, want hot at messages[8]", res.Phase, res.Boundary)
	}
	op := findOp(t, res, "op1")
	if op.Saved != (TierTokens{CacheWrite5m: 7000}) || op.Caused != (TierTokens{}) {
		t.Fatalf("op1 saved %+v caused %+v, want 7000 written", op.Saved, op.Caused)
	}
	if op.Operator != "context-guru.cmdfilter" || op.ChangeID != "cg:cmdfilter:c40e" {
		t.Fatalf("op1 lost its operator or ChangeID: %+v", op)
	}
	noChecks(t, res)
}

func TestAnActiveChangeIsAppliedAgainAndReadWhenHot(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	rs[1].run(ts)
	res := rs[2].run(ts)
	op := findOp(t, res, "active:cg:cmdfilter:c40e")
	if op.Saved != (TierTokens{CacheRead: 7000}) || op.Operator != "context-guru.cmdfilter" {
		t.Fatalf("applied again: %+v, want 7000 read under cmdfilter", op)
	}
	if len(res.CacheEffects) != 0 {
		t.Fatalf("a turn that only appends has no break: %+v", res.CacheEffects)
	}
	noChecks(t, res)
}

func TestAColdRequestWritesAgainAndRecordsTheExpiry(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	for _, q := range rs[:3] {
		q.run(ts)
	}
	res := rs[3].run(ts)
	if res.Phase != PhaseCold || res.Boundary != nil {
		t.Fatalf("phase %q boundary %v, want cold and no boundary", res.Phase, res.Boundary)
	}
	if op := findOp(t, res, "active:cg:cmdfilter:c40e"); op.Saved != (TierTokens{CacheWrite5m: 7000}) {
		t.Fatalf("cold: %+v, want 7000 written", op.Saved)
	}
	if len(res.CacheEffects) != 1 || res.CacheEffects[0].Kind != EffectBreak || res.CacheEffects[0].ChangeID != "" ||
		res.CacheEffects[0].Saved != (TierTokens{}) {
		t.Fatalf("expiry: %+v, want one empty break with no cause", res.CacheEffects)
	}
	noChecks(t, res)
}

func ptr(p Position) *Position { return &p }

// §6.2: after the last breakpoint, and with no breakpoint, the tokens are input.
func TestTokensAfterTheLastBreakpointAreInput(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	q := rs[1]
	q.clientBPs = bps5(sys(0), msg(8)) // L is before message 9
	if p := findOp(t, q.run(ts), "op1"); p.Saved != (TierTokens{Input: 7000}) {
		t.Fatalf("after L: %+v, want 7000 input", p.Saved)
	}
}

func TestTokensWithNoBreakpointAreInput(t *testing.T) {
	op := rewrite("op1", "tool-prune", "tp", tool(0), 1000, 0)
	res := req{at: t0, client: mk([]blk{b("t0", 1000)}, nil, msgs(1, 1000)), fwd: mk(nil, nil, msgs(1, 1000)),
		ops: []Operation{op}, usage: Usage{Input: 1000}}.run(NewTrackers())
	if p := findOp(t, res, "op1"); p.Saved != (TierTokens{Input: 1000}) {
		t.Fatalf("no breakpoint: %+v, want 1000 input", p.Saved)
	}
}

// §6.2: the TTL of a write is the TTL of the first breakpoint at or after the position.
func TestAWriteTakesTheTTLOfTheNextBreakpoint(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	q := rs[1]
	q.clientBPs = []Breakpoint{bp5(sys(0)), {Pos: msg(9), TTL: "1h"}}
	if p := findOp(t, q.run(ts), "op1"); p.Saved != (TierTokens{CacheWrite1h: 7000}) {
		t.Fatalf("1h: %+v, want 7000 written at 1h", p.Saved)
	}
}

// R33: a rewrite that adds tokens (a restore, an injected tool) is a cost, in forwarded space.
func TestARewriteThatAddsTokensIsACost(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	q := rs[1]
	q.ops = []Operation{rewrite("op1", "context-guru.restore", "cg:restore", msg(9), 8000, 9000)}
	q.fwd = mk(nil, ex2Sys, with(ex2Msgs, b("r9", 9000)))
	q.usage = Usage{CacheRead: 30000, CacheWrite: 9000}
	res := q.run(ts)
	if p := findOp(t, res, "op1"); p.Caused != (TierTokens{CacheWrite5m: 1000}) || p.Saved != (TierTokens{}) {
		t.Fatalf("restore: %+v, want 1000 written as Caused", p)
	}
	noChecks(t, res)
}

// A restore is found by size, not by hash: the hashes are cumulative, so an earlier change that is
// still applied makes the restored item's hash differ from the client's.
func TestARestoreIsFoundWhileAnEarlierChangeStaysApplied(t *testing.T) {
	ts := NewTrackers()
	client := with(msgs(9, 3000), b("m9", 3000))
	opA := rewrite("opA", "context-guru.mask", "cg:mask:a", msg(2), 3000, 500)
	opB := rewrite("opB", "context-guru.coref", "cg:coref:b", msg(5), 3000, 500)
	shrunk := func(restoreB bool) []blk {
		out := append([]blk(nil), client...)
		out[2] = b("fa", 500)
		if !restoreB {
			out[5] = b("fb", 500)
		}
		return out
	}
	reqs := []req{
		{at: t0, client: mk(nil, ex2Sys, client), clientBPs: bps5(sys(0), msg(9)), facts: exact5m,
			usage: Usage{CacheWrite: 33000}},
		{at: t0.Add(10 * time.Second), client: mk(nil, ex2Sys, client), clientBPs: bps5(sys(0), msg(9)),
			ops: []Operation{opA, opB}, fwd: mk(nil, ex2Sys, shrunk(false)), facts: exact5m,
			usage: Usage{CacheWrite: 28000}},
		{at: t0.Add(20 * time.Second), client: mk(nil, ex2Sys, client), clientBPs: bps5(sys(0), msg(9)),
			fwd: mk(nil, ex2Sys, shrunk(true)), facts: exact5m, usage: Usage{CacheRead: 28000, CacheWrite: 2500}},
	}
	ids := func() []string {
		var out []string
		for _, op := range ts.tracker(ThreadKey("s1", Thread{ID: "main"})).snapshot().Active {
			out = append(out, op.ID)
		}
		return out
	}
	reqs[0].run(ts)
	reqs[1].run(ts)
	if got := ids(); len(got) != 2 {
		t.Fatalf("after the report: active %v, want opA and opB", got)
	}
	reqs[2].run(ts)
	if got := ids(); len(got) != 1 || got[0] != "opA" {
		t.Fatalf("after opB was restored: active %v, want only opA", got)
	}
}

// A failed check is logged at warning level by Compute, once for each failure, and never by Preview.
func TestAFailedCheckIsLoggedAsAWarning(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	q := rs[1]
	q.fwd = mk(nil, ex2Sys, with(ex2Msgs, b("f9", 900))) // 100 bytes smaller than the operation reports
	r := q.begin(ts)

	if res := r.Preview(q.usage); len(res.Checks) == 0 || buf.Len() != 0 {
		t.Fatalf("Preview: %d checks, log %q, want checks and no log", len(res.Checks), buf.String())
	}
	res := r.Compute(q.usage)
	if len(res.Checks) == 0 {
		t.Fatal("Compute: no check failed, the scenario is wrong")
	}
	if got := strings.Count(buf.String(), "level=WARN msg=\"counterfactual check failed\""); got != len(res.Checks) {
		t.Fatalf("%d warnings for %d failed checks:\n%s", got, len(res.Checks), buf.String())
	}
}
