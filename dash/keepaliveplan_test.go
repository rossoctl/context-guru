package dash

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/keepalive"
)

// The dashboard replays the per-wait ping plan (#868). These tests keep that replay equal to the keeper.

// nopDispatcher is a keeper host that never sends: the drift test only reads the plan.
type nopDispatcher struct{ next int }

func (d *nopDispatcher) Do(context.Context, dispatch.Request) (dispatch.Response, error) {
	return dispatch.Response{}, dispatch.ErrNotHeld
}
func (d *nopDispatcher) Release(dispatch.Ref) {}
func (d *nopDispatcher) Price(context.Context, string, string, int64) (dispatch.Price, bool) {
	return dispatch.Price{}, false
}

const planBody = `{"model":"aws/claude-sonnet-5","max_tokens":32000,"stream":true,` +
	`"tools":[{"name":"Bash","description":"run a command","input_schema":{"type":"object"}}],` +
	`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

// turn is one finished response of a thread.
type turn struct {
	names []string
	stop  string
	// status is the HTTP status (0: 200). hour asks the 1-hour tier on every breakpoint; billed1h says the
	// response billed its write at 1 hour. read says the turn only reads the cache (writes nothing).
	status         int
	hour, billed1h bool
	read           bool
	partial        bool // only the first breakpoint asks 1 hour (stored cache_ttl still says 1 hour)
}

func (t turn) withHour(billed bool) turn { t.hour, t.billed1h = true, billed; return t }
func (t turn) readOnly() turn            { t.read = true; return t }
func (t turn) failed() turn              { t.status = 500; return t }

var (
	tCall = turn{names: []string{"Bash"}, stop: "tool_use"}
	tEnd  = turn{stop: "end_turn"}
)

// keeperPings is the ping count the KEEPER plans for the wait after the last of seq, on thread.
// 0 when the keeper plans no ping (no tool yet, done).
func keeperPings(t *testing.T, base int, thread string, bg, tm int, seq []turn) (int, time.Duration) {
	t.Helper()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	d := &nopDispatcher{}
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go:        func(f func()) { f() },
		OpenTasks: func(string, string, string) (int, int) { return bg, tm }})
	pol := keepalive.Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: base,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 80000, WaitPlan: true}
	key := keepalive.Key{Tenant: "t1", Session: "s1", Thread: thread}
	for i, r := range seq {
		if i > 0 {
			k.Arrive(key) // the next request arrives: the earlier entry is retired, as live
		}
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer sk-x")
		hdr.Set("Anthropic-Version", "2023-06-01")
		body, status := planBody, r.status
		if status == 0 {
			status = 200
		}
		if r.hour {
			n := -1
			if r.partial {
				n = 1
			}
			body = strings.Replace(body, `"cache_control":{"type":"ephemeral"}`, `"cache_control":{"type":"ephemeral","ttl":"1h"}`, n)
		}
		u := keepalive.Usage{CacheRead: 98576, CacheWrite: 1200, StopReason: r.stop}
		if r.read {
			u.CacheWrite = 0
		}
		if r.billed1h {
			u.CacheWrite1h = u.CacheWrite
		}
		k.Record(keepalive.Record{Key: key, StartedAt: now.Add(-time.Duration(len(seq)-1-i) * time.Second),
			Body: []byte(body), Provider: bschemas.Anthropic, Route: "/v1/messages", Status: status,
			Usage:        u,
			ToolUseNames: r.names, UsageOK: true, Policy: pol, Header: hdr,
			Hold: func() dispatch.Ref { d.next++; return dispatch.Ref(fmt.Sprint(d.next)) }})
	}
	pl := k.Plans("t1", false, "s1")[thread]
	return pl.PingsLeft, pl.Idle
}

// dashPings is the count the DASHBOARD replay gives the same thread: it stores the rows the proxy
// would write (the wait cause comes from the same classifier), reads them back through pingSpans,
// and asks the span for its count. The last span is open, so it gets its full count.
func dashPings(t *testing.T, base int, thread string, bg, tm int, seq []turn) (int, time.Duration) {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for i, r := range seq {
		e := kaAgent(1_000_000+int64(i)*1000, "s1", 0.01)
		e.CacheRead, e.CacheWrite, e.ThreadID, e.StopReason = 98576, 1200, thread, r.stop
		if r.read {
			e.CacheWrite = 0
		}
		if r.hour {
			e.CacheTTL = "ephemeral_1h"
		}
		if r.billed1h {
			e.CacheWrite1h = e.CacheWrite
		}
		if r.status != 0 {
			e.Status = r.status
		}
		// What the proxy stores: the classifier's answer for this response. The last row's open-task
		// counts are the ones the keeper's OpenTasks hook reports.
		b, m := 0, 0
		if i == len(seq)-1 {
			b, m = bg, tm
		}
		e.WaitingFor, e.WaitTool = waitcause.ClassifyWaitingFor(r.names, r.stop, r.status != 0, true, b, m)
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	spans, err := db.pingSpans(Filter{TenantAll: true}, DefaultWaitPolicy(base), true)
	if err != nil {
		t.Fatal(err)
	}
	pol := keepalive.Policy{WaitPlan: true, MaxPings: base, Idle: 280 * time.Second}
	var open *pingSpan
	for i := range spans {
		if spans[i].open {
			open = &spans[i]
		}
	}
	if open == nil {
		return 0, 0
	}
	_, idle := pol.SpanPings(open.facts)
	return open.planPings(pol), idle
}

// TestDashboardReplayMatchesTheKeeper pins the dashboard's plan to the keeper's: the same responses
// give the same ping count through both paths. It fails when either side changes a rule alone.
func TestDashboardReplayMatchesTheKeeper(t *testing.T) {
	sub := "a:abc"
	ask := turn{names: []string{"AskUserQuestion"}, stop: "tool_use"}
	for _, base := range []int{2, 10} {
		for _, tc := range []struct {
			name   string
			thread string
			seq    []turn
			bg, tm int
		}{
			{"main, own tool", "", []turn{tCall, tCall}, 0, 0},
			{"subagent, own tool", sub, []turn{tCall, tCall}, 0, 0},
			{"main, end_turn, background", "", []turn{tCall, tEnd}, 1, 0},
			{"main, end_turn, a person", "", []turn{tCall, tEnd}, 0, 0},
			{"main, ask-a-person tool", "", []turn{tCall, ask}, 0, 0},
			{"main, end_turn, teammate", "", []turn{tCall, tEnd}, 0, 1},
			{"subagent, end_turn", sub, []turn{tCall, tEnd}, 0, 0},
			{"subagent, end_turn, background", sub, []turn{tCall, tEnd}, 1, 0},
			{"subagent, handback", sub, []turn{tCall, {names: []string{"SubagentHandback"}, stop: "tool_use"}}, 0, 0},
			{"done", "", []turn{tCall, {names: []string{"StructuredOutput"}, stop: "tool_use"}}, 0, 0},
			{"no tool call yet", "", []turn{tEnd, tEnd}, 0, 0},
			{"no tool call yet, background open", "", []turn{tEnd, tEnd}, 1, 0},
			{"earlier tool call counts", "", []turn{tCall, tEnd, tEnd}, 0, 0},
			{"tool stop, no names, background open", "", []turn{tCall, {stop: "tool_use"}}, 1, 0},
			{"1-hour tier, own tool", "", []turn{tCall.withHour(true), tCall.withHour(true)}, 0, 0},
			{"1-hour tier, a person", "", []turn{tCall.withHour(true), tEnd.withHour(true)}, 0, 0},
			{"1-hour tier, no tool call yet", "", []turn{tEnd.withHour(true), tEnd.withHour(true)}, 0, 0},
			{"1-hour tier, read-only turn inherits", "", []turn{tCall.withHour(true), tEnd.withHour(true).readOnly()}, 0, 0},
			{"mixed: asked 1 hour, billed 5 minutes", "", []turn{tCall.withHour(false), tCall.withHour(false)}, 0, 0},
			{"mixed: 1 hour then billed 5 minutes", "", []turn{tCall.withHour(true), tCall.withHour(false)}, 0, 0},
			{"failed call after a tool call", "", []turn{tCall, tEnd.failed()}, 0, 0},
		} {
			t.Run(fmt.Sprintf("%s/base%d", tc.name, base), func(t *testing.T) {
				want, wantIdle := keeperPings(t, base, tc.thread, tc.bg, tc.tm, tc.seq)
				got, gotIdle := dashPings(t, base, tc.thread, tc.bg, tc.tm, tc.seq)
				if got != want {
					t.Errorf("dashboard replay = %d pings, keeper = %d", got, want)
				}
				if want > 0 && gotIdle != wantIdle {
					t.Errorf("dashboard replay spaces pings %v apart, keeper %v", gotIdle, wantIdle)
				}
			})
		}
	}
}

// One known gap, stated and pinned (not hidden): a tool stop with NO tool name and nothing open is stored
// as a wait for a person, as an ask-a-person tool is. The row cannot tell the two apart. The keeper reads
// the first as a tool wait (8 pings), the replay as a person (base + 1). The case needs a spliced
// sniff window, which is rare. The same window gives the second known gap: the keeper counts a response
// with an incomplete name list as a tool call (calledATool), and the stored row has no such fact, so the
// replay's "tool seen" misses it (a thread whose only tool calls were spliced reads "no tool yet" until a
// tool stop is stored). The third gap: the stored cache_ttl says ANY breakpoint asked 1 hour, where the
// keeper's ResolveTier needs EVERY breakpoint to ask. A mixed request with a billed 1-hour write reads
// as 1-hour in the replay (one ping at 59 minutes) and as 5-minute in the keeper.
func TestDashboardReplayKnownGapSplicedToolStop(t *testing.T) {
	seq := []turn{tCall, {stop: "tool_use"}}
	if got, _ := keeperPings(t, 2, "", 0, 0, seq); got != 8 {
		t.Fatalf("keeper = %d, want 8", got)
	}
	if got, _ := dashPings(t, 2, "", 0, 0, seq); got != 3 {
		t.Errorf("dashboard replay = %d, want 3 (base + 1)", got)
	}
}

func planPrice(string) (modelinfo.Price, bool) { return ibmSonnet, true }

// planFixture: session "a" has a tool wait that a fixed K=2 cuts short; session "b" has a thread that
// never called a tool, which the plan does not ping. Two expiries show the same split on reach.
func planFixture(t *testing.T) *kaFixture {
	t.Helper()
	row := func(ts int64, sess, stop, wf string) *Event {
		e := kaAgent(ts, sess, 0.05)
		e.CacheRead, e.CacheWrite, e.StopReason, e.WaitingFor = 100_000, 0, stop, wf
		return e
	}
	sec := func(s int64) int64 { return s * 1000 }
	t0 := int64(10_000_000)
	evs := []*Event{
		row(t0, "a", "tool_use", "own_tool"),
		row(t0+sec(60), "a", "tool_use", "own_tool"),
		row(t0+sec(1560), "a", "end_turn", "human"),
		row(t0, "b", "end_turn", "human"),
		row(t0+sec(60), "b", "end_turn", "human"),
	}
	// E1: a tool wait of 1000 s (reached by 8 pings, not by 2). E2: a thread with no tool call yet, 700 s.
	e1 := kaExpiry(t0+sec(20_000), "e1", 1000, 1.0, 100_000)
	e1[0].StopReason, e1[0].WaitingFor = "tool_use", "own_tool"
	e2 := kaExpiry(t0+sec(40_000), "e2", 700, 1.0, 100_000)
	e2[0].StopReason, e2[0].WaitingFor = "end_turn", "human"
	evs = append(evs, e1...)
	evs = append(evs, e2...)
	return newKAFixture(t, evs...)
}

func rungOf(t *testing.T, c *KeepAliveCalc, plan string, k int) CalcRow {
	t.Helper()
	for _, r := range c.Rows {
		if r.Plan == plan && r.MaxPings == k {
			return r
		}
	}
	t.Fatalf("no %s rung with K=%d in %+v", plan, k, c.Rows)
	return CalcRow{}
}

func TestCalculatorHasALivePlanRungMarkedCurrent(t *testing.T) {
	fx := planFixture(t)
	on := keepalive.Policy{WaitPlan: true}
	c, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 100_000, "aws/claude-sonnet-5", planPrice, 2, on)
	if err != nil {
		t.Fatal(err)
	}
	if c.Plan != PlanWait || len(c.Rows) != 5 {
		t.Fatalf("plan = %q, rows = %d, want wait_plan and 5 (the plan, then K=1..4)", c.Plan, len(c.Rows))
	}
	plan, fixed2 := rungOf(t, c, PlanWait, 2), rungOf(t, c, PlanFixed, 2)
	if !plan.Current || fixed2.Current {
		t.Errorf("current: plan %v, fixed K=2 %v; want the plan only", plan.Current, fixed2.Current)
	}
	// Session a: a 1500 s tool wait gets floor(1220/280)+1 = 5 pings (fixed: 2), and the last request is a
	// person (base+1 = 3; fixed: 2). Session b never called a tool: 0 (fixed: 2). The e1/e2 sessions add
	// their own spans, the same on both sides of this comparison only where noted below.
	if plan.Pings == fixed2.Pings {
		t.Errorf("plan pings = fixed pings = %d; the plan should differ from fixed K here", plan.Pings)
	}
	// Reach: E1 (tool wait, 1000 s) only the plan reaches; E2 (no tool yet, 700 s) only the fixed K=2 does.
	if plan.Convertible != 1 || fixed2.Convertible != 1 {
		t.Errorf("convertible: plan %d, fixed K=2 %d, want 1 and 1", plan.Convertible, fixed2.Convertible)
	}
	if rungOf(t, c, PlanFixed, 3).Convertible != 2 {
		t.Errorf("fixed K=3 should reach both expiries")
	}
}

// The off switch: an account with keepalive_wait_plan false gets fixed-K figures, as before.
func TestCalculatorWithThePlanOffModelsFixedK(t *testing.T) {
	fx := planFixture(t)
	c, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 100_000, "aws/claude-sonnet-5", planPrice, 2, keepalive.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Plan != PlanFixed || len(c.Rows) != 4 {
		t.Fatalf("plan = %q, rows = %d, want fixed_k and 4", c.Plan, len(c.Rows))
	}
	for _, r := range c.Rows {
		if r.Plan != PlanFixed || r.Current != (r.MaxPings == 2) {
			t.Errorf("rung %+v: want fixed_k, current only at K=2", r)
		}
	}
}

func TestRecommendationStatesItsModelAndTheFixedKComparison(t *testing.T) {
	fx := newKAFixture(t)
	rec, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true}, keepalive.Policy{WaitPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Plan != PlanWait {
		t.Errorf("plan = %q, want wait_plan", rec.Plan)
	}
	off, err := fx.db.KeepAliveRecommend(Filter{TenantAll: true}, keepalive.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if off.Plan != PlanFixed || off.FixedK != nil {
		t.Errorf("plan off: plan = %q, fixed_k = %v; want fixed_k and no comparison", off.Plan, off.FixedK)
	}
}

// The plan replay adds the facts window to two reads. It must stay near the cost of the fixed-K reads.
// Relative bound, as in zz_perf_test.go: an absolute bound is a machine away from meaningless.
func TestPlanReplayStaysNearTheFixedKCost(t *testing.T) {
	db := openTestDB(t)
	const n = 6_000
	batch := make([]*Event, 0, 1000)
	for i := 0; i < n; i++ {
		e := kaAgent(int64(1_700_000_000_000+i*100_000), fmt.Sprintf("s%d", i/40), 0.01)
		e.ThreadID = map[bool]string{true: "a:x", false: ""}[i%3 == 0]
		e.CacheRead, e.CacheWrite, e.StopReason, e.WaitingFor = 90_000, 100, "tool_use", "own_tool"
		if i%7 == 0 {
			e.CacheMissReason, e.CacheRead, e.CacheWrite = CacheTTLExpiry, 0, 90_000
		}
		batch = append(batch, e)
		if len(batch) == 1000 {
			if err := db.insertBatch(batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	f := Filter{TenantAll: true}
	run := func(plan bool) time.Duration {
		t0 := time.Now()
		if _, err := db.pingSpans(f, keepalive.Policy{}, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := db.expiries(f, plan); err != nil {
			t.Fatal(err)
		}
		return time.Since(t0)
	}
	fixed, plan := run(false), run(true)
	t.Logf("%d rows: fixed-K reads %v, plan reads %v", n, fixed, plan)
	if plan > 6*fixed+2*time.Second {
		t.Errorf("plan reads %v against fixed-K reads %v", plan, fixed)
	}
}

// At an idle of 300 s or more the keeper keeps the base count, so the plan rung models fixed K.
func TestCalculatorAtThreeHundredSecondsModelsFixedK(t *testing.T) {
	fx := planFixture(t)
	c, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 300, 100_000, "aws/claude-sonnet-5", planPrice, 2, keepalive.Policy{WaitPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Plan != PlanFixed || len(c.Rows) != 4 {
		t.Errorf("plan = %q, rows = %d, want fixed_k and 4", c.Plan, len(c.Rows))
	}
}

// The plan's counts reach the response, so the page does not hard-code them.
func TestCalculatorStatesThePlanCounts(t *testing.T) {
	fx := planFixture(t)
	c, err := fx.db.KeepAliveCalc(Filter{TenantAll: true}, 280, 100_000, "aws/claude-sonnet-5", planPrice, 2,
		keepalive.Policy{WaitPlan: true, ToolWaitMaxPings: 6})
	if err != nil {
		t.Fatal(err)
	}
	if c.Counts == nil || c.Counts.ToolWaitMax != 6 || c.Counts.HumanExtra != keepalive.DefaultHumanExtraPings {
		t.Errorf("plan_counts = %+v", c.Counts)
	}
}

// Third known gap: the stored cache_ttl means ANY breakpoint asked 1 hour; the keeper's ResolveTier needs
// EVERY breakpoint to ask. A mixed request billed at 1 hour is a 5-minute thread for the keeper (the plan
// counts, 5-minute spacing) and a 1-hour thread for the replay (one ping, 59 minutes apart).
func TestDashboardReplayKnownGapMixedBreakpointTier(t *testing.T) {
	mixed := tCall.withHour(true)
	mixed.partial = true
	seq := []turn{tCall, mixed}
	_, kIdle := keeperPings(t, 2, "", 0, 0, seq)
	_, dIdle := dashPings(t, 2, "", 0, 0, seq)
	if kIdle >= time.Hour || dIdle < 50*time.Minute {
		t.Errorf("keeper spacing %v, replay spacing %v: want a 5-minute keeper and a 1-hour replay", kIdle, dIdle)
	}
}

// The replay gates each span on the floor of its own wait, as the keeper does.
func TestSpanPrefixOKFollowsTheWaitCause(t *testing.T) {
	pol := DefaultWaitPolicy(2)
	tool := keepalive.SpanFacts{Cause: "own_tool", StopReason: "tool_use"}
	human := keepalive.SpanFacts{Cause: "human", StopReason: "end_turn"}
	sub := keepalive.SpanFacts{Cause: "human", StopReason: "end_turn", Subagent: true}
	for _, c := range []struct {
		name string
		f    keepalive.SpanFacts
		want bool
	}{{"main tool wait", tool, true}, {"main person wait", human, false}, {"subagent person wait", sub, true},
		{"main background wait", keepalive.SpanFacts{Cause: "background", StopReason: "end_turn"}, true},
		{"main teammate wait", keepalive.SpanFacts{Cause: "teammate", StopReason: "end_turn"}, true},
		{"main unknown wait", keepalive.SpanFacts{Cause: "unknown", StopReason: "end_turn"}, false}} {
		if got := pol.SpanPrefixOK(c.f, 30000); got != c.want {
			t.Errorf("%s at 30k: %v, want %v", c.name, got, c.want)
		}
		if !pol.SpanPrefixOK(c.f, 80000) || pol.SpanPrefixOK(c.f, 19999) {
			t.Errorf("%s: wrong at 80k or 19999", c.name)
		}
	}
}
