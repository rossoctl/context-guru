package counterfactual

import (
	"testing"
	"time"
)

// Regression tests for the findings of the first review.

// §4.5, §4.6: the order decided on the first request must hold on every later request, so the
// reducer that ran before the summary keeps its credit.
func TestAReducerBeforeASummaryKeepsItsCreditOnLaterRequests(t *testing.T) {
	_, ts := summaryOverMask(t, 1100)
	sysB := []blk{b("sys", 1000)}
	client := []blk{b("m0", 1000), b("m1", 2000), b("m2", 1000), b("m3", 500)}
	res := req{at: t0.Add(2 * time.Minute), client: mk(nil, sysB, client), clientBPs: bps5(sys(0), msg(3)),
		fwd: mk(nil, sysB, []blk{b("S", 50), b("m2", 1000), b("m3", 500)}), fwdBPs: bps5(sys(0), msg(2)), facts: exact5m,
		usage: Usage{CacheRead: 2050, CacheWrite: 500}}.run(ts)
	noChecks(t, res)
	findOp(t, res, "active:cg:mask")
	if st := stateOf(ts, "main"); len(st.Active) != 2 {
		t.Fatalf("Active = %+v, want mask and the summary", st.Active)
	}
}

// §4.2 rule 2, §6.5 check 3: a spend shares the ChangeID of its change and does not replace it.
func TestASpendWithTheChangeIDOfAnActiveRewriteKeepsTheRewrite(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	rs[1].run(ts)
	q := rs[2]
	q.ops = []Operation{{ID: "s", Kind: KindSpend, ChangeID: "cg:cmdfilter:c40e", Call: &ModelCall{Model: "m"}}}
	res := q.run(ts)
	noChecks(t, res)
	findOp(t, res, "active:cg:cmdfilter:c40e")
}

// A malformed report does not replace the good active operation with its ChangeID.
func TestAMalformedReportKeepsTheActiveOperation(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	rs[1].run(ts)
	q := rs[2]
	q.ops = []Operation{{ID: "y", Kind: KindRewrite, ChangeID: "cg:cmdfilter:c40e", SizeBefore: Size{Bytes: -1}}}
	res := q.run(ts)
	noChecks(t, res)
	findOp(t, res, "active:cg:cmdfilter:c40e")
}

// §4.6: a setting stays active until the setting changes again.
func TestASettingStaysActive(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	hi, lo := "high", "low"
	q := rs[0]
	q.ops = []Operation{{ID: "e", Kind: KindSetting, Operator: "router", ChangeID: "rt:e",
		Settings: map[string]SettingChange{"effort": {From: &hi, To: &lo}}}}
	q.run(ts)
	r1 := rs[1]
	r1.ops = nil
	r1.fwd = Body{}
	r1.usage = Usage{CacheRead: 30000, CacheWrite: 8000}
	r1.run(ts)
	st := stateOf(ts, "main")
	if len(st.Active) != 1 || st.Active[0].ChangeID != "rt:e" {
		t.Fatalf("Active = %+v, want the setting", st.Active)
	}
}

// R12, §4.3: a DryRun splice did not change the forwarded body, so it moves no other position.
func TestADryRunSpliceMovesNoPosition(t *testing.T) {
	sysB := []blk{b("sys", 1000)}
	client := msgs(5, 1000)
	client[4] = b("m4", 2000)
	sum := rewrite("op1", "context-guru.summarize", "cg:sum", msg(0), 3000, 100)
	sum.MessageSplice = &MessageSplice{At: 0, Removed: 3, Inserted: 1}
	sum.DryRun = true
	cmd := rewrite("op2", "context-guru.cmdfilter", "cg:cmd", msg(4), 2000, 200)
	fwd := with(client[:4], b("f4", 200))
	ts := NewTrackers()
	res := req{at: t0, client: mk(nil, sysB, client), clientBPs: bps5(sys(0), msg(4)), ops: []Operation{sum, cmd},
		fwd: mk(nil, sysB, fwd), facts: exact5m, usage: Usage{CacheWrite: 5200}}.run(ts)
	noChecks(t, res)
	for _, op := range stateOf(ts, "main").Active {
		if op.ChangeID == "cg:cmd" && op.At != msg(4) {
			t.Fatalf("cmdfilter stored at %+v, want messages[4]", op.At)
		}
	}
}

// §4.3, §4.5: a client edit at or before an active change ends it.
func TestAClientEditBeforeAnActiveChangeEndsIt(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	rs[1].run(ts)
	short := msgs(12, 100) // a client compaction: every message is new
	res := req{at: t0.Add(2 * time.Minute), client: mk(nil, ex2Sys, short), clientBPs: bps5(sys(0), msg(11)), facts: exact5m,
		usage: Usage{CacheRead: 3000, CacheWrite: 1200}}.run(ts)
	for _, p := range res.Ops {
		if p.ChangeID == "cg:cmdfilter:c40e" {
			t.Fatalf("the change ended with the client edit and must not be priced: %+v", p)
		}
	}
	noChecks(t, res)
}

// §4.2 setting table, §5.2 row 3: system[0] in a body with no system starts at the first message,
// so the tools before it are still read.
func TestASettingPositionInAnEmptySectionStartsAtTheNextSection(t *testing.T) {
	tools := []blk{b("t", 5000)}
	m := msgs(4, 8000)
	ts := NewTrackers()
	req{at: t0, client: mk(tools, nil, m), clientBPs: bps5(tool(0), msg(3)), facts: exact5m, usage: Usage{CacheWrite: 37000}}.run(ts)
	on := "on"
	op := Operation{ID: "op1", Kind: KindSetting, Operator: "router", ChangeID: "rt:ws",
		Settings: map[string]SettingChange{"web_search": {To: &on, At: ptr(sys(0))}}}
	res := req{at: t0.Add(time.Minute), client: mk(tools, nil, with(m, b("m4", 1000))), clientBPs: bps5(tool(0), msg(4)),
		ops: []Operation{op}, facts: exact5m, usage: Usage{CacheRead: 5000, CacheWrite: 33000}}.run(ts)
	if br := effect(t, res, EffectBreak); !samePos(br.From, ptr(tool(0))) || br.Saved != (TierTokens{CacheRead: 32000}) {
		t.Fatalf("break %+v, want 32000 from tools[0]", br)
	}
}

// §5.2 row 4: the read the phase predicts is prevExtent snapped down to the last forwarded
// breakpoint at or before it. A previous extent past the last breakpoint is not a lost entry.
func TestRow4PredictsTheReadAtTheBreakpoint(t *testing.T) {
	sysB := []blk{b("sys", 3000)}
	m := msgs(4, 1000)
	ts := NewTrackers()
	// The previous request cached through messages[3], but the next request only has a breakpoint
	// at system: the provider can read 3,000 at most.
	req{at: t0, client: mk(nil, sysB, m), clientBPs: bps5(sys(0), msg(3)), facts: exact5m, usage: Usage{CacheWrite: 7000}}.run(ts)
	res := req{at: t0.Add(time.Minute), client: mk(nil, sysB, with(m, b("m4", 1000))), clientBPs: bps5(sys(0)), facts: exact5m,
		usage: Usage{CacheRead: 3000, Input: 5000}}.run(ts)
	noEffects(t, res)
	if !samePos(res.Boundary, ptr(sys(0))) {
		t.Fatalf("boundary %v, want system[0]: no breakpoint of this request reaches messages[3]", res.Boundary)
	}
}

// §5.3: the tokens come from the request's own ratio. Here the provider bills one token for every
// two bytes, so 7,000 bytes saved are 3,500 tokens, and the extents are found in tokens.
func TestTheBytesRatioConvertsSizesAndExtents(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	for i := range rs {
		u := &rs[i].usage
		u.Input, u.CacheRead, u.CacheWrite = u.Input/2, u.CacheRead/2, u.CacheWrite/2
	}
	rs[0].run(ts)
	res := rs[1].run(ts)
	if !samePos(res.Boundary, ptr(msg(8))) {
		t.Fatalf("boundary %v, want messages[8]", res.Boundary)
	}
	if p := findOp(t, res, "op1"); p.Saved != (TierTokens{CacheWrite5m: 3500}) {
		t.Fatalf("op1 %+v, want 3500 written", p.Saved)
	}
	if p := findOp(t, rs[2].run(ts), "active:cg:cmdfilter:c40e"); p.Saved != (TierTokens{CacheRead: 3500}) {
		t.Fatalf("applied again %+v, want 3500 read", p.Saved)
	}
}
