package counterfactual

import (
	"testing"
	"time"
)

// The tests bill one token per byte: each usage adds up to the forwarded body's bytes, so the
// request ratio (§5.3) is exactly 1 and the expected figures are those of examples.md.

func effect(t *testing.T, res Result, kind CacheEffectKind) CacheEffect {
	t.Helper()
	var found []CacheEffect
	for _, e := range res.CacheEffects {
		if e.Kind == kind {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s effect, got %+v", kind, res.CacheEffects)
	}
	return found[0]
}

func noEffects(t *testing.T, res Result) {
	t.Helper()
	if len(res.CacheEffects) != 0 {
		t.Fatalf("CacheEffects = %+v, want none", res.CacheEffects)
	}
}

// Example 3: tool-prune removes three declarations that are not next to each other.
func TestSpansInTheToolsAreReadWhenHot(t *testing.T) {
	tools := []blk{b("t0", 1000), b("t1", 1000), b("t2", 3500), b("t3", 5000), b("t4", 2000), b("t5", 2500), b("t6", 5000)}
	fwdTools := []blk{b("t0", 1000), b("t1", 1000), b("t3", 5000), b("t6", 5000)}
	sysB := []blk{b("sys", 3000)}
	op := Operation{ID: "op1", Kind: KindRewrite, Operator: "tool-prune", ChangeID: "tp:decl-set-7f3a", At: tool(2),
		Spans: []Span{
			{Section: SectionTools, From: 2, To: 2, SizeBefore: Size{Bytes: 3500}},
			{Section: SectionTools, From: 4, To: 4, SizeBefore: Size{Bytes: 2000}},
			{Section: SectionTools, From: 5, To: 5, SizeBefore: Size{Bytes: 2500}}},
		SizeBefore: Size{Bytes: 8000}}
	m0 := msgs(1, 1000)
	ts := NewTrackers()
	req{at: t0, client: mk(tools, sysB, m0), clientBPs: bps5(sys(0), msg(0)), ops: []Operation{op},
		fwd: mk(fwdTools, sysB, m0), facts: exact5m, usage: Usage{CacheWrite: 16000}}.run(ts)
	m1 := msgs(2, 1000)
	res := req{at: t0.Add(time.Minute), client: mk(tools, sysB, m1), clientBPs: bps5(sys(0), msg(1)),
		fwd: mk(fwdTools, sysB, m1), facts: exact5m, usage: Usage{CacheRead: 16000, CacheWrite: 1000}}.run(ts)
	if p := findOp(t, res, "active:tp:decl-set-7f3a"); p.Saved != (TierTokens{CacheRead: 8000}) || !p.Estimated {
		t.Fatalf("hot: %+v, want 8000 read, estimated", p)
	}
	noEffects(t, res)
	noChecks(t, res)
}

func TestTheSpansCheckComparesTheSums(t *testing.T) {
	op := Operation{ID: "op1", Kind: KindRewrite, Operator: "tool-prune", ChangeID: "tp", At: tool(0),
		Spans:      []Span{{Section: SectionTools, From: 0, To: 0, SizeBefore: Size{Bytes: 100}}},
		SizeBefore: Size{Bytes: 999}}
	res := req{at: t0, client: mk([]blk{b("t0", 100)}, nil, msgs(1, 100)), fwd: mk(nil, nil, msgs(1, 100)),
		ops: []Operation{op}, usage: Usage{Input: 100}}.run(NewTrackers())
	if len(res.Checks) != 1 || res.Checks[0].Check != "spans" {
		t.Fatalf("Checks = %+v, want one spans failure", res.Checks)
	}
}

// Example 4: a summary with a splice. On request A it breaks the cache after system, on B it is read.
func TestASummaryBreaksTheCacheOnceThenIsRead(t *testing.T) {
	tools, sysB := []blk{b("t", 4000)}, []blk{b("sys", 4000)}
	client := []blk{b("m0", 4000), b("m1", 3000), b("m2", 3000), b("m3", 1500), b("m4", 1500)}
	fwd := []blk{b("S", 500), b("m3", 1500), b("m4", 1500)}
	spend := Operation{ID: "op1", Kind: KindSpend, Operator: "context-guru.summarize", ChangeID: "cg:summarize:5d21",
		Call: &ModelCall{Model: "summarizer-model", Usage: CallUsage{Usage: Usage{Input: 10000}, Output: 500, CostUSD: 0.0125}}}
	sum := rewrite("op2", "context-guru.summarize", "cg:summarize:5d21", msg(0), 10000, 500)
	sum.MessageSplice = &MessageSplice{At: 0, Removed: 3, Inserted: 1}

	ts := NewTrackers()
	req{at: t0, client: mk(tools, sysB, client), clientBPs: bps5(sys(0), msg(4)), facts: exact5m,
		usage: Usage{CacheWrite: 21000}}.run(ts)
	a := req{at: t0.Add(time.Minute), client: mk(tools, sysB, client), clientBPs: bps5(sys(0), msg(4)),
		ops: []Operation{spend, sum}, fwd: mk(tools, sysB, fwd), fwdBPs: bps5(sys(0), msg(2)), facts: exact5m,
		usage: Usage{CacheRead: 8000, CacheWrite: 3500}}.run(ts)

	if p := findOp(t, a, "op2"); p.Saved != (TierTokens{CacheRead: 9500}) {
		t.Fatalf("A: summary saved %+v, want 9500 read", p.Saved)
	}
	if p := findOp(t, a, "op1"); p.Call == nil || p.Call.Usage.CostUSD != 0.0125 || p.Saved != (TierTokens{}) {
		t.Fatalf("A: the spend must pass its Call through unpriced: %+v", p)
	}
	br := effect(t, a, EffectBreak)
	if br.ChangeID != "cg:summarize:5d21" || !samePos(br.From, ptr(sys(0))) || !samePos(br.To, ptr(msg(2))) ||
		br.Saved != (TierTokens{CacheRead: 3500}) || br.Caused != (TierTokens{CacheWrite5m: 3500}) {
		t.Fatalf("A: break %+v, want 3500 read->write from system[0] to messages[2] (forwarded)", br)
	}
	noChecks(t, a)

	clientB, fwdB := with(client, b("m5", 500)), with(fwd, b("m5", 500))
	bReq := req{at: t0.Add(2 * time.Minute), client: mk(tools, sysB, clientB), clientBPs: bps5(sys(0), msg(5)),
		fwd: mk(tools, sysB, fwdB), fwdBPs: bps5(sys(0), msg(3)), facts: exact5m,
		usage: Usage{CacheRead: 11500, CacheWrite: 500}}
	resB := bReq.run(ts)
	if p := findOp(t, resB, "active:cg:summarize:5d21"); p.Saved != (TierTokens{CacheRead: 9500}) {
		t.Fatalf("B: summary saved %+v, want 9500 read", p.Saved)
	}
	if !samePos(resB.Boundary, ptr(msg(4))) {
		t.Fatalf("B: boundary %v, want messages[4] in client space", resB.Boundary)
	}
	noEffects(t, resB)
	noChecks(t, resB)
}

// Example 7: coref changes message 5, inside the cached prefix. The last breakpoint before it is the
// end of system, so everything after system is written again.
func ex7(clientBPs []Breakpoint, usage Usage) Result {
	tools, sysB := []blk{b("t", 12300)}, []blk{b("sys", 3000)}
	m := []blk{b("m0", 3000), b("m1", 2000), b("m2", 2000), b("m3", 2060), b("m4", 2000), b("m5", 20000),
		b("m6", 1500), b("m7", 1500), b("m8", 1000), b("m9", 1000), b("m10", 1400), b("m11", 1500)}
	prevBPs := append([]Breakpoint{}, clientBPs...)
	prevBPs[len(prevBPs)-1] = bp5(msg(11))
	ts := NewTrackers()
	req{at: t0, client: mk(tools, sysB, m), clientBPs: prevBPs, facts: exact5m, usage: Usage{CacheWrite: 56260 - 2000}}.run(ts)
	fwd := with(m[:5], b("c5", 50))
	fwd = append(fwd, m[6:]...)
	op := rewrite("op12", "context-guru.coref", "cg:coref:5e2d", msg(5), 20000, 50)
	return req{at: t0.Add(time.Minute), client: mk(tools, sysB, with(m, b("m12", 2000))), clientBPs: clientBPs,
		ops: []Operation{op}, fwd: mk(tools, sysB, with(fwd, b("m12", 2000))), facts: exact5m, usage: usage}.run(ts)
}

func TestAChangeInsideTheCachedPrefixBreaksFromTheLastBreakpoint(t *testing.T) {
	res := ex7(bps5(sys(0), msg(12)), Usage{CacheRead: 15300, CacheWrite: 21010})
	if p := findOp(t, res, "op12"); p.Saved != (TierTokens{CacheRead: 19950}) {
		t.Fatalf("coref saved %+v, want 19950 read", p.Saved)
	}
	br := effect(t, res, EffectBreak)
	if br.ChangeID != "cg:coref:5e2d" || !samePos(br.From, ptr(sys(0))) || !samePos(br.To, ptr(msg(11))) ||
		br.Saved != (TierTokens{CacheRead: 19010}) || br.Caused != (TierTokens{CacheWrite5m: 19010}) {
		t.Fatalf("break %+v, want 19010 read->write, system[0] to messages[11], cg:coref:5e2d", br)
	}
	noChecks(t, res)
}

func TestABreakpointBeforeTheChangeShortensThePenalty(t *testing.T) {
	// 15,300 + messages 0 to 4 (11,060) are read; the penalty is 50 + 7,900 = 7,950.
	res := ex7(bps5(sys(0), msg(4), msg(12)), Usage{CacheRead: 26360, CacheWrite: 9950})
	if br := effect(t, res, EffectBreak); br.Saved != (TierTokens{CacheRead: 7950}) || !samePos(br.From, ptr(msg(4))) {
		t.Fatalf("break %+v, want 7950 from messages[4]", br)
	}
}

// §5.2 row 2: the client edits inside the cached prefix. The break has no cause, and B* snaps to the
// last client breakpoint before the edit (§5.1, case 20).
func TestAClientEditBreaksWithNoCauseAndSnapsBStar(t *testing.T) {
	sysB := []blk{b("sys", 3000)}
	m := msgs(6, 1000)
	ts := NewTrackers()
	req{at: t0, client: mk(nil, sysB, m), clientBPs: bps5(sys(0), msg(5)), facts: exact5m, usage: Usage{CacheWrite: 9000}}.run(ts)
	edited := with(m)
	edited[3] = b("e3", 1000)
	res := req{at: t0.Add(time.Minute), client: mk(nil, sysB, edited), clientBPs: bps5(sys(0), msg(5)), facts: exact5m,
		usage: Usage{CacheRead: 3000, CacheWrite: 6000}}.run(ts)
	if !samePos(res.Boundary, ptr(sys(0))) {
		t.Fatalf("boundary %v, want system[0]", res.Boundary)
	}
	if br := effect(t, res, EffectBreak); br.ChangeID != "" {
		t.Fatalf("a client edit has no cause: %+v", br)
	}
}

// §5.2 row 4: nothing changed, the phase is hot, but the provider read less than it held.
func TestAnEntryLostEarlyIsABreakWithNoCause(t *testing.T) {
	sysB := []blk{b("sys", 3000)}
	ts := NewTrackers()
	req{at: t0, client: mk(nil, sysB, msgs(3, 1000)), clientBPs: bps5(sys(0), msg(2)), facts: exact5m, usage: Usage{CacheWrite: 6000}}.run(ts)
	res := req{at: t0.Add(time.Minute), client: mk(nil, sysB, msgs(4, 1000)), clientBPs: bps5(sys(0), msg(3)), facts: exact5m,
		usage: Usage{CacheRead: 3000, CacheWrite: 4000}}.run(ts)
	if br := effect(t, res, EffectBreak); br.ChangeID != "" || br.Saved != (TierTokens{CacheRead: 3000}) {
		t.Fatalf("break %+v, want 3000 tokens with no cause", br)
	}
}

// Example 5b: an effort change that the provider renders ahead of the messages.
func ex5b(at *Position, usage Usage) Result {
	tools, sysB := []blk{b("t", 5000)}, []blk{b("sys", 3000)}
	m := msgs(4, 8000)
	ts := NewTrackers()
	req{at: t0, client: mk(tools, sysB, m), clientBPs: bps5(sys(0), msg(3)), facts: exact5m, usage: Usage{CacheWrite: 40000}}.run(ts)
	high, low := "high", "low"
	op := Operation{ID: "op1", Kind: KindSetting, Operator: "inference-router", ChangeID: "rt:effort-3b9e",
		Settings: map[string]SettingChange{"effort": {From: &high, To: &low, At: at}}}
	return req{at: t0.Add(time.Minute), client: mk(tools, sysB, with(m, b("m4", 1000))), clientBPs: bps5(sys(0), msg(4)),
		ops: []Operation{op}, facts: exact5m, usage: usage}.run(ts)
}

func TestASettingBreakStartsAtTheSettingPosition(t *testing.T) {
	res := ex5b(ptr(msg(0)), Usage{CacheRead: 8000, CacheWrite: 33000})
	br := effect(t, res, EffectBreak)
	if br.ChangeID != "rt:effort-3b9e" || !samePos(br.From, ptr(sys(0))) || br.Saved != (TierTokens{CacheRead: 32000}) {
		t.Fatalf("break %+v, want 32000 from system[0] under rt:effort-3b9e", br)
	}
	if p := findOp(t, res, "op1"); p.Saved != (TierTokens{}) || p.Caused != (TierTokens{}) {
		t.Fatalf("a setting is not priced (R50): %+v", p)
	}
}

func TestASettingWithNoPositionUsesTheReadAndSaysSo(t *testing.T) {
	res := ex5b(nil, Usage{CacheRead: 8000, CacheWrite: 33000})
	if br := effect(t, res, EffectBreak); br.ChangeID != "rt:effort-3b9e" || br.Saved != (TierTokens{CacheRead: 32000}) {
		t.Fatalf("break %+v", br)
	}
	if !hasUnpriced(res, ReasonPenaltyUnknown) {
		t.Fatalf("Unpriced = %+v, want penalty_unknown", res.Unpriced)
	}
}

func TestASettingWithTheReadIntactHasNoBreak(t *testing.T) {
	noEffects(t, ex5b(ptr(msg(0)), Usage{CacheRead: 40000, CacheWrite: 1000}))
}

// Example 8: a keep-alive ping at minute 24 keeps turn 9's entry alive for turn 10 at minute 28.
func ex8(turn10 time.Duration, facts EntryFacts) Result {
	m := msgs(6, 1000)
	sysB := []blk{b("sys", 4000)}
	ts := NewTrackers()
	start := t0.Add(20 * time.Minute)
	req{at: start, client: mk(nil, sysB, m), clientBPs: bps5(sys(0), msg(5)), facts: facts, usage: Usage{CacheWrite: 10000}}.run(ts)
	return req{at: t0.Add(turn10), client: mk(nil, sysB, with(m, b("m6", 500))), clientBPs: bps5(sys(0), msg(6)), facts: facts,
		usage: Usage{CacheRead: 10000, CacheWrite: 500}}.run(ts)
}

func pingFacts() EntryFacts {
	f := exact5m
	f.LastPing, f.PingRead, f.PingWindow = t0.Add(24*time.Minute), ptr(msg(5)), "ka:main:w1"
	return f
}

func TestAPingThatKeptTheEntryAliveIsARescue(t *testing.T) {
	res := ex8(28*time.Minute, pingFacts())
	if res.Phase != PhaseHot {
		t.Fatalf("phase %q, want hot", res.Phase)
	}
	e := effect(t, res, EffectRescue)
	if e.ChangeID != "ka:main:w1" || e.From != nil || !samePos(e.To, ptr(msg(5))) ||
		e.Saved != (TierTokens{CacheWrite5m: 10000}) || e.Caused != (TierTokens{CacheRead: 10000}) {
		t.Fatalf("rescue %+v, want 10000 write->read to messages[5]", e)
	}
}

func TestAPingTooLateIsNoRescue(t *testing.T) {
	res := ex8(30*time.Minute, pingFacts())
	if res.Phase != PhaseCold {
		t.Fatalf("phase %q, want cold", res.Phase)
	}
	for _, e := range res.CacheEffects {
		if e.Kind == EffectRescue {
			t.Fatalf("no rescue when the entry gap is past the TTL: %+v", e)
		}
	}
}

func TestAPingInsideTheTTLIsNotCredited(t *testing.T) {
	res := ex8(23*time.Minute, pingFacts())
	noEffects(t, res)
}

// R45: on a minimum lifetime a gap proves no expiry. A read in the usage makes it hot, with no rescue.
func TestAMinimumLifetimeIsUnknownAndGetsNoRescue(t *testing.T) {
	f := pingFacts()
	f.Lifetime = LifetimeMinimum
	res := ex8(28*time.Minute, f)
	if res.Phase != PhaseHot || !hasUnpriced(res, ReasonPenaltyUnknown) {
		t.Fatalf("phase %q unpriced %+v, want hot from the read and penalty_unknown", res.Phase, res.Unpriced)
	}
	noEffects(t, res)
}

// Example 5a: a model change starts at tools[0], so the whole cached prefix is written again.
func TestAModelChangeBreaksTheWholePrefix(t *testing.T) {
	tools, sysB := []blk{b("t", 5000)}, []blk{b("sys", 3000)}
	m := msgs(4, 8000)
	ts := NewTrackers()
	req{at: t0, client: mk(tools, sysB, m), clientBPs: bps5(sys(0), msg(3)), facts: exact5m, usage: Usage{CacheWrite: 40000}}.run(ts)
	large, small := "large-model", "small-model"
	op := Operation{ID: "op1", Kind: KindSetting, Operator: "inference-router", ChangeID: "rt:model-7c1a",
		Settings: map[string]SettingChange{"model": {From: &large, To: &small, At: ptr(tool(0))}}}
	res := req{at: t0.Add(time.Minute), client: mk(tools, sysB, with(m, b("m4", 1000))), clientBPs: bps5(sys(0), msg(4)),
		ops: []Operation{op}, facts: exact5m, usage: Usage{CacheWrite: 41000}}.run(ts)
	br := effect(t, res, EffectBreak)
	if br.ChangeID != "rt:model-7c1a" || br.From != nil || br.Saved != (TierTokens{CacheRead: 40000}) {
		t.Fatalf("break %+v, want 40000 from the start of the prompt", br)
	}
}

// The setting's position decides where the break starts, not the read: here the provider read
// nothing, but the setting says tools and system stay valid.
func TestASettingPositionWinsOverTheRead(t *testing.T) {
	res := ex5b(ptr(msg(0)), Usage{CacheWrite: 41000})
	if br := effect(t, res, EffectBreak); !samePos(br.From, ptr(sys(0))) || br.Saved != (TierTokens{CacheRead: 32000}) {
		t.Fatalf("break %+v, want 32000 from system[0]", br)
	}
	if hasUnpriced(res, ReasonPenaltyUnknown) {
		t.Fatalf("a known position is not penalty_unknown: %+v", res.Unpriced)
	}
}

// Check 1: a plugin changed the body and did not report it.
func TestAnUnreportedChangeFailsTheSumCheck(t *testing.T) {
	res := req{at: t0, client: mk(nil, nil, msgs(2, 1000)), fwd: mk(nil, nil, []blk{b("m0", 1000), b("x", 400)}),
		usage: Usage{Input: 1400}}.run(NewTrackers())
	if len(res.Checks) != 1 || res.Checks[0].Check != "sum" {
		t.Fatalf("Checks = %+v, want one sum failure", res.Checks)
	}
}

// Case 21: a summary splices messages 0 to 2 into one; cmdfilter, after it in the pipeline, reports
// a position in its own output. Its client position is 2 higher. On the next request both are
// applied again and the sum check holds.
func TestAPositionAfterASpliceIsMappedToClientSpace(t *testing.T) {
	sysB := []blk{b("sys", 1000)}
	client := []blk{b("m0", 1000), b("m1", 1000), b("m2", 1000), b("m3", 1000), b("m4", 2000)}
	fwd := []blk{b("S", 100), b("m3", 1000), b("f4", 200)}
	sum := rewrite("op1", "context-guru.summarize", "cg:sum", msg(0), 3000, 100)
	sum.MessageSplice = &MessageSplice{At: 0, Removed: 3, Inserted: 1}
	cmd := rewrite("op2", "context-guru.cmdfilter", "cg:cmd", msg(2), 2000, 200) // messages[2] in its input
	ts := NewTrackers()
	req{at: t0, client: mk(nil, sysB, client), clientBPs: bps5(sys(0), msg(4)), ops: []Operation{sum, cmd},
		fwd: mk(nil, sysB, fwd), fwdBPs: bps5(sys(0), msg(2)), facts: exact5m, usage: Usage{CacheWrite: 2300}}.run(ts)
	res := req{at: t0.Add(time.Minute), client: mk(nil, sysB, with(client, b("m5", 500))), clientBPs: bps5(sys(0), msg(5)),
		fwd: mk(nil, sysB, with(fwd, b("m5", 500))), fwdBPs: bps5(sys(0), msg(3)), facts: exact5m,
		usage: Usage{CacheRead: 2300, CacheWrite: 500}}.run(ts)
	noChecks(t, res)
	if p := findOp(t, res, "active:cg:cmd"); p.Saved != (TierTokens{CacheRead: 1800}) {
		t.Fatalf("cmdfilter %+v, want 1800 read", p.Saved)
	}
	noEffects(t, res)
}

// §4.5: a summary splices away a message that an active mask reduced. The summary's own size says
// which ran first: when it saw the original bytes, the summary ran first and mask has ended; when it
// saw the mask marker, mask ran first and keeps its credit. The total is the same.
func summaryOverMask(t *testing.T, sumBefore int) (Result, *Trackers) {
	t.Helper()
	sysB := []blk{b("sys", 1000)}
	client := []blk{b("m0", 1000), b("m1", 2000), b("m2", 1000)}
	ts := NewTrackers()
	mask := rewrite("op1", "context-guru.mask", "cg:mask", msg(1), 2000, 100)
	req{at: t0, client: mk(nil, sysB, client), clientBPs: bps5(sys(0), msg(2)), ops: []Operation{mask},
		fwd: mk(nil, sysB, []blk{b("m0", 1000), b("k1", 100), b("m2", 1000)}), facts: exact5m, usage: Usage{CacheWrite: 3100}}.run(ts)
	sum := rewrite("op2", "context-guru.summarize", "cg:sum", msg(0), sumBefore, 50)
	sum.MessageSplice = &MessageSplice{At: 0, Removed: 2, Inserted: 1}
	res := req{at: t0.Add(time.Minute), client: mk(nil, sysB, client), clientBPs: bps5(sys(0), msg(2)), ops: []Operation{sum},
		fwd: mk(nil, sysB, []blk{b("S", 50), b("m2", 1000)}), fwdBPs: bps5(sys(0), msg(1)), facts: exact5m,
		usage: Usage{CacheRead: 1000, CacheWrite: 1050}}.run(ts)
	noChecks(t, res)
	return res, ts
}

func TestASummaryThatSawTheOriginalEndsTheChangeInside(t *testing.T) {
	res, ts := summaryOverMask(t, 3000)
	for _, p := range res.Ops {
		if p.ChangeID == "cg:mask" {
			t.Fatalf("mask ended under the summary and must not be priced: %+v", p)
		}
	}
	if st := ts.tracker(ThreadKey("s1", Thread{ID: "main"})).snapshot(); len(st.Active) != 1 || st.Active[0].ChangeID != "cg:sum" {
		t.Fatalf("Active = %+v, want only the summary", st.Active)
	}
}

func TestASummaryAfterTheReducerKeepsItsCredit(t *testing.T) {
	res, ts := summaryOverMask(t, 1100)
	if p := findOp(t, res, "active:cg:mask"); p.Saved.CacheRead+p.Saved.CacheWrite5m != 1900 {
		t.Fatalf("mask %+v, want 1900 saved", p.Saved)
	}
	if st := ts.tracker(ThreadKey("s1", Thread{ID: "main"})).snapshot(); len(st.Active) != 2 {
		t.Fatalf("Active = %+v, want mask and the summary", st.Active)
	}
}

// §4.4 condition 4: the rescue is the smaller of what the ping read and what this request read.
func TestARescueIsWhatThePingRead(t *testing.T) {
	f := pingFacts()
	f.PingRead = ptr(msg(1)) // the ping read system and messages 0 to 1: 6,000
	e := effect(t, ex8(28*time.Minute, f), EffectRescue)
	if e.Saved != (TierTokens{CacheWrite5m: 6000}) {
		t.Fatalf("rescue %+v, want 6000", e.Saved)
	}
}

// §4.4 condition 3: when the provider wrote more than it read, the ping rescued nothing.
func TestNoRescueWhenTheRequestWroteMoreThanItRead(t *testing.T) {
	m := msgs(6, 1000)
	sysB := []blk{b("sys", 4000)}
	ts := NewTrackers()
	req{at: t0.Add(20 * time.Minute), client: mk(nil, sysB, m), clientBPs: bps5(sys(0), msg(5)), facts: pingFacts(),
		usage: Usage{CacheWrite: 10000}}.run(ts)
	res := req{at: t0.Add(28 * time.Minute), client: mk(nil, sysB, with(m, b("m6", 500))), clientBPs: bps5(sys(0), msg(6)),
		facts: pingFacts(), usage: Usage{CacheRead: 4000, CacheWrite: 6500}}.run(ts)
	for _, e := range res.CacheEffects {
		if e.Kind == EffectRescue {
			t.Fatalf("no rescue when write > read: %+v", e)
		}
	}
}
