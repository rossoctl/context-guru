package counterfactual

import (
	"sync"
	"testing"
	"time"
)

func stateOf(ts *Trackers, thread string) ThreadState {
	return ts.tracker(ThreadKey("s1", Thread{ID: thread})).snapshot()
}

// R53: two requests of one thread in flight. The second Compute finds the state changed since its
// EndRequest, calculates from the current state, and says so.
func TestAComputeAfterAnotherRequestRecomputes(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	first, second := rs[1].begin(ts), rs[1].begin(ts)
	if res := first.Compute(rs[1].usage); res.Recomputed {
		t.Fatal("the first Compute must not be recomputed")
	}
	res := second.Compute(rs[1].usage)
	if !res.Recomputed {
		t.Fatal("the second Compute must say it recomputed")
	}
	if got := stateOf(ts, "main").Version; got != 3 {
		t.Fatalf("Version = %d, want 3: no request loses its replace", got)
	}
}

func TestConcurrentComputesAllReplace(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	var wg sync.WaitGroup
	for range 20 {
		r := rs[0].begin(ts)
		wg.Add(1)
		go func() { defer wg.Done(); r.Compute(rs[0].usage) }()
	}
	wg.Wait()
	if got := stateOf(ts, "main").Version; got != 20 {
		t.Fatalf("Version = %d, want 20", got)
	}
}

func TestPreviewStoresNothing(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	r := rs[1].begin(ts)
	p := r.Preview(rs[1].usage)
	if stateOf(ts, "main").Version != 1 {
		t.Fatal("Preview changed the ThreadState")
	}
	if c := r.Compute(rs[1].usage); findOp(t, c, "op1").Saved != findOp(t, p, "op1").Saved {
		t.Fatal("Preview and Compute must give the same figure")
	}
}

// R24: each thread compares with its own previous request; a subagent has its own state.
func TestThreadsHaveTheirOwnState(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	sub := rs[1]
	r := ts.BeginRequest(RequestMeta{Session: "s1", At: sub.at}, sub.client, sub.clientBPs)
	r.EndRequest(Thread{ID: "agent-1", Parent: "main"}, sub.client, sub.clientBPs, sub.facts)
	r.Compute(Usage{CacheWrite: 31000})
	if stateOf(ts, "main").Version != 1 || stateOf(ts, "agent-1").Version != 1 {
		t.Fatal("a subagent request must change only its own thread")
	}
	if res := rs[1].run(ts); res.Phase != PhaseHot || res.Recomputed {
		t.Fatalf("main must still compare with its own previous request: phase %q", res.Phase)
	}
}

func TestStandaloneSpendPassesTheCallThrough(t *testing.T) {
	call := &ModelCall{Model: "agent-model", Usage: CallUsage{Usage: Usage{Input: 10, CacheRead: 10000}, Output: 1}}
	res := ComputeStandalone(RequestOps{Version: 1, RequestID: "ping-p1", Session: "s2", Thread: Thread{ID: "main"},
		Ops: []Operation{{ID: "op1", Kind: KindSpend, Operator: "keeper", ChangeID: "ka:main:w1", Call: call},
			{ID: "op2", Kind: KindRewrite}}})
	if len(res.Ops) != 2 || res.Ops[0].Call != call || res.Ops[0].ChangeID != "ka:main:w1" {
		t.Fatalf("Ops = %+v", res.Ops)
	}
	if len(res.Unpriced) != 1 || res.Unpriced[0] != (Unpriced{OpID: "op2", Reason: ReasonNotSpend}) {
		t.Fatalf("Unpriced = %+v, want op2 not_spend", res.Unpriced)
	}
}

// R19 and §8: a malformed operation goes to Unpriced; the others are priced.
func TestMalformedOperationsAreUnpriced(t *testing.T) {
	ts := NewTrackers()
	rs := ex2Requests()
	rs[0].run(ts)
	q := rs[1]
	q.ops = append(q.ops, Operation{ID: "x", Kind: "cacheChange"},
		Operation{ID: "y", Kind: KindRewrite, ChangeID: "neg", SizeBefore: Size{Bytes: -1}})
	res := q.run(ts)
	if !hasUnpricedOp(res, "x", ReasonUnknownKind) || !hasUnpricedOp(res, "y", ReasonNegativeSize) {
		t.Fatalf("Unpriced = %+v", res.Unpriced)
	}
	if findOp(t, res, "op1").Saved != (TierTokens{CacheWrite5m: 7000}) {
		t.Fatal("the valid operation must still be priced")
	}
	for _, op := range stateOf(ts, "main").Active {
		if op.ChangeID == "neg" {
			t.Fatal("a malformed operation must not become active")
		}
	}
}

func hasUnpricedOp(res Result, id, reason string) bool {
	for _, u := range res.Unpriced {
		if u.OpID == id && u.Reason == reason {
			return true
		}
	}
	return false
}

// R12, §4.6: a DryRun is priced as "would have saved", stays active as DryRun, and is not in the sum.
// Reporting the same ChangeID applied replaces it (observe to enforce), and §5.2 row 1 adds the break.
func TestObserveThenEnforce(t *testing.T) {
	sysB := []blk{b("sys", 1000)}
	m := []blk{b("m0", 1000), b("m1", 2000)}
	dry := rewrite("op1", "context-guru.mask", "cg:mask", msg(1), 2000, 100)
	dry.DryRun = true
	ts := NewTrackers()
	r0 := req{at: t0, client: mk(nil, sysB, m), clientBPs: bps5(sys(0), msg(1)), ops: []Operation{dry}, facts: exact5m,
		usage: Usage{CacheWrite: 4000}}.run(ts)
	if p := findOp(t, r0, "op1"); !p.DryRun || p.Saved == (TierTokens{}) {
		t.Fatalf("a DryRun is priced and marked: %+v", p)
	}
	noChecks(t, r0)

	m2 := with(m, b("m2", 500))
	r1 := req{at: t0.Add(time.Minute), client: mk(nil, sysB, m2), clientBPs: bps5(sys(0), msg(2)), facts: exact5m,
		usage: Usage{CacheRead: 4000, CacheWrite: 500}}.run(ts)
	if p := findOp(t, r1, "active:cg:mask"); !p.DryRun || p.Saved != (TierTokens{CacheRead: 1900}) {
		t.Fatalf("the DryRun stays active: %+v", p)
	}
	noChecks(t, r1)

	on := rewrite("op1", "context-guru.mask", "cg:mask", msg(1), 2000, 100)
	m3 := with(m2, b("m3", 500))
	r2 := req{at: t0.Add(2 * time.Minute), client: mk(nil, sysB, m3), clientBPs: bps5(sys(0), msg(3)), ops: []Operation{on},
		fwd: mk(nil, sysB, []blk{b("m0", 1000), b("k1", 100), b("m2", 500), b("m3", 500)}), facts: exact5m,
		usage: Usage{CacheRead: 2000, CacheWrite: 1100}}.run(ts)
	if len(r2.Ops) != 1 || r2.Ops[0].DryRun {
		t.Fatalf("the applied report replaces the DryRun: %+v", r2.Ops)
	}
	if br := effect(t, r2, EffectBreak); br.ChangeID != "cg:mask" {
		t.Fatalf("leaving observe mode breaks the cache under its ChangeID: %+v", br)
	}
	noChecks(t, r2)
}

func TestTwoRewritesWithOneChangeIDFailTheCheck(t *testing.T) {
	a := rewrite("a", "context-guru.mask", "same", msg(0), 1000, 100)
	c := rewrite("c", "context-guru.coref", "same", msg(1), 1000, 100)
	res := req{at: t0, client: mk(nil, nil, msgs(2, 1000)), ops: []Operation{a, c},
		fwd: mk(nil, nil, []blk{b("x0", 100), b("x1", 100)}), usage: Usage{Input: 200}}.run(NewTrackers())
	if len(res.Checks) != 1 || res.Checks[0].Check != "unique_change" {
		t.Fatalf("Checks = %+v, want one unique_change failure", res.Checks)
	}
}

// onThread runs q as a request of the given thread in session s1. req.run is always "main".
func onThread(ts *Trackers, th Thread, q req) Result {
	r := ts.BeginRequest(RequestMeta{RequestID: "r-" + th.ID, Session: "s1", At: q.at}, q.client, q.clientBPs)
	for _, op := range q.ops {
		r.Add(op)
	}
	fwd, fbp := q.fwd, q.fwdBPs
	if fwd.Pos == nil {
		fwd = q.client
	}
	if fbp == nil {
		fbp = q.clientBPs
	}
	r.EndRequest(th, fwd, fbp, q.facts)
	return r.Compute(q.usage)
}

func hasOp(res Result, id string) bool {
	for _, p := range res.Ops {
		if p.OpID == id {
			return true
		}
	}
	return false
}

// One session, three threads (main, a side thread and a subagent). A change on main stays active
// and is priced only on main. Each thread's Version lock moves only on its own requests. Two
// threads that report the same ChangeID, as the operator derives the same one for the same output,
// are not a unique_change failure, because that check runs inside one request.
func TestThreadsOfOneSessionKeepTheirOwnChangesAndVersions(t *testing.T) {
	const change = "active:cg:cmdfilter:c40e"
	ts := NewTrackers()
	main, side, sub := Thread{ID: "main"}, Thread{ID: "p:side", Parent: "main"}, Thread{ID: "a:sub", Parent: "main"}
	rs := ex2Requests()
	versions := func(m, s, a uint64) {
		t.Helper()
		if got := [3]uint64{stateOf(ts, "main").Version, stateOf(ts, "p:side").Version, stateOf(ts, "a:sub").Version}; got != [3]uint64{m, s, a} {
			t.Fatalf("versions (main, p:side, a:sub) = %v, want %v", got, [3]uint64{m, s, a})
		}
	}

	// main reports the change once, so it is active on main.
	onThread(ts, main, rs[0])
	onThread(ts, main, rs[1])
	versions(2, 0, 0)
	if a := stateOf(ts, "main").Active; len(a) != 1 || a[0].ChangeID != "cg:cmdfilter:c40e" {
		t.Fatalf("main Active = %+v, want the cmdfilter change", a)
	}

	// The other threads start empty: main's active change is not priced on them, and their
	// first requests leave main's lock alone.
	for _, th := range []Thread{side, sub} {
		res := onThread(ts, th, rs[0])
		if hasOp(res, change) || len(res.Ops) != 0 {
			t.Fatalf("%s: ops %+v, want none: main's change must not reach another thread", th.ID, res.Ops)
		}
		noChecks(t, res)
	}
	versions(2, 1, 1)
	if len(stateOf(ts, "p:side").Active) != 0 || len(stateOf(ts, "a:sub").Active) != 0 {
		t.Fatal("a change on main must not become active on another thread")
	}

	// main's next request prices the active change, and only main's lock moves.
	res := onThread(ts, main, rs[2])
	if op := findOp(t, res, change); op.Saved != (TierTokens{CacheRead: 7000}) {
		t.Fatalf("main active op %+v, want 7000 read", op)
	}
	versions(3, 1, 1)

	// The side thread and the subagent both report the same ChangeID in their own requests. Each
	// prices it once, as new, and neither fails unique_change.
	for _, th := range []Thread{side, sub} {
		res := onThread(ts, th, rs[1])
		op := findOp(t, res, "op1")
		if op.ChangeID != "cg:cmdfilter:c40e" || op.Saved.CacheWrite5m != 7000 {
			t.Fatalf("%s: op %+v, want the change priced as new (7000 write)", th.ID, op)
		}
		noChecks(t, res)
	}
	versions(3, 2, 2)

	// Each thread now holds the change on its own, and main's stays at its own version.
	for _, name := range []string{"main", "p:side", "a:sub"} {
		if a := stateOf(ts, name).Active; len(a) != 1 {
			t.Fatalf("%s Active = %+v, want exactly one change", name, a)
		}
	}
	res = onThread(ts, side, rs[2])
	if findOp(t, res, change).Saved != (TierTokens{CacheRead: 7000}) {
		t.Fatalf("p:side active op %+v, want 7000 read", findOp(t, res, change))
	}
	versions(3, 3, 2)
}
