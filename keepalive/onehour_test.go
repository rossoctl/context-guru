package keepalive

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// #1243: the 1-hour-on-wait ping.

func onWaitPolicy() Policy {
	return Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2, MaxUSDPerPing: 0.25,
		MinPrefixTokens: 20000, WaitPlan: true, OneHourOnWait: true,
		Idle1h: 3540 * time.Second, MaxPings1h: 1}
}

func TestLongWaitGroupsTheWaitCauses(t *testing.T) {
	for _, tc := range []struct {
		cause string
		sub   bool
		want  bool
	}{
		{waitcause.WaitingHuman, false, true},
		{waitcause.WaitingTeammate, false, true},
		{waitcause.WaitingBackground, false, true},
		{waitcause.WaitingUnknown, false, true},
		// MAIN AGENT ONLY: a subagent thread is never long, whatever its cause.
		{waitcause.WaitingBackground, true, false},
		{waitcause.WaitingTeammate, true, false},
		{waitcause.WaitingUnknown, true, false},
		{waitcause.WaitingHuman, true, false},
		{waitcause.WaitingOwnTool, false, false},
		{waitcause.WaitingOwnTool, true, false},
		{waitcause.WaitingRetry, false, false},
		{waitcause.WaitingDone, false, false},
	} {
		if got := LongWait(tc.cause, tc.sub); got != tc.want {
			t.Errorf("LongWait(%q, subagent=%v) = %v, want %v", tc.cause, tc.sub, got, tc.want)
		}
	}
}

// oneHourKeyOf is the route key of the kaBody model on the test route.
func oneHourKeyOf() string {
	return oneHourRoute(bschemas.Anthropic, "", "/v1/messages", "aws/claude-sonnet-5")
}

// proveRoute marks the test route as proven to grant the 1-hour tier.
func proveRoute(k *Keeper) {
	k.mu.Lock()
	k.oneHourRoutes[oneHourKeyOf()] = &oneHourRouteState{proven: true}
	k.mu.Unlock()
}

func routeState(k *Keeper) oneHourRouteState {
	k.mu.Lock()
	defer k.mu.Unlock()
	if st := k.oneHourRoutes[oneHourKeyOf()]; st != nil {
		return *st
	}
	return oneHourRouteState{}
}

// recordWait records two turns on key: one that called a tool (so the thread is pinged at all, #858),
// then one that ends with stop and names. The second turn sets the wait the ping serves.
func recordWait(k *Keeper, pol Policy, key Key, at time.Time, stop string, names []string) {
	u := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use"}
	k.Record(Record{Key: key, StartedAt: at.Add(-time.Second), Body: []byte(kaBody), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, Usage: u, UsageOK: true, Policy: pol,
		ToolUseNames: []string{"Bash"}, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
	u.StopReason = stop
	k.Record(Record{Key: key, StartedAt: at, Body: []byte(kaBody), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, Usage: u, UsageOK: true, Policy: pol,
		ToolUseNames: names, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
}

// A long wait gets ONE ping at the usual time with every marker at 1 hour, and no second ping.
func TestOneHourPingOnALongWait(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49000, CacheWrite: 100, Output: 1}, http.StatusOK, nil // a free switch
	})
	proveRoute(k)
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil) // a person
	if n := k.Sweep(start.Add(239 * time.Second)); n != 0 {
		t.Fatalf("pinged before the usual time (%d)", n)
	}
	if n := k.Sweep(start.Add(240 * time.Second)); n != 1 {
		t.Fatalf("no ping at the usual time (%d)", n)
	}
	waitPings(t, k, 1)
	if e, ok := k.Entry(sess1); !ok || !e.TTL1h {
		t.Errorf("after a free 1-hour ping the entry must exist and be marked 1-hour (found %v, %+v)", ok, e)
	}
	body := fd.sent[0].Body
	if !apply.AllBreakpointsAskExtendedTTL(body) {
		t.Errorf("not every marker asks 1h: %s", body)
	}
	if got := strings.Count(string(body), `"ttl":"1h"`); got != strings.Count(string(body), "cache_control") {
		t.Errorf("%d of %d markers relabelled", got, strings.Count(string(body), "cache_control"))
	}
	// The body is not the real request's body, so a response cache that replays identical bytes cannot answer.
	if string(body) == kaBody {
		t.Error("the 1-hour ping body equals the recorded request body")
	}
	// No second ping, no 59-minute ping.
	for _, at := range []time.Duration{480, 600, 3540, 3600} {
		if n := k.Sweep(start.Add(at * time.Second)); n != 0 {
			t.Fatalf("a second ping fired at %ds (%d)", at, n)
		}
	}
}

// A short wait (a tool) behaves exactly as auto: the plain body, and the planned count.
func TestShortWaitIsPlainAuto(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "tool_use", []string{"Bash"})
	if n := k.Sweep(start.Add(240 * time.Second)); n != 1 {
		t.Fatalf("no ping (%d)", n)
	}
	waitPings(t, k, 1)
	if gjson.GetBytes(fd.sent[0].Body, "system.0.cache_control.ttl").Exists() {
		t.Errorf("a tool-wait ping was relabelled: %s", fd.sent[0].Body)
	}
	if n := k.Sweep(start.Add(480 * time.Second)); n != 1 {
		t.Fatalf("a tool wait gets auto's second ping, got %d", n)
	}
}

// MAIN AGENT ONLY: a subagent thread on a long wait (here background work, then a person) gets auto's
// normal 5-minute pings, and its count is auto's.
func TestSubagentLongWaitsAreAutoNotOneHour(t *testing.T) {
	for _, tc := range []struct {
		name, stop string
		names      []string
	}{{"a person", "end_turn", nil}, {"background work", "tool_use", []string{"BashOutput", "TaskOutput"}}} {
		t.Run(tc.name, func(t *testing.T) {
			k, fd, clock := testKeeper(t)
			sub := Key{Tenant: "t1", Session: "sess-1", Thread: "a:x"}
			start := clock.now()
			recordWait(k, onWaitPolicy(), sub, start, tc.stop, tc.names)
			if n := k.Sweep(start.Add(240 * time.Second)); n != 1 {
				t.Fatalf("no ping (%d)", n)
			}
			waitPings(t, k, 1)
			if gjson.GetBytes(fd.sent[0].Body, "system.0.cache_control.ttl").Exists() {
				t.Errorf("a subagent's ping was relabelled: %s", fd.sent[0].Body)
			}
			if n := k.Sweep(start.Add(480 * time.Second)); n != 1 {
				t.Errorf("a subagent does not get auto's second 5-minute ping (%d)", n)
			}
		})
	}
}

// The strategy off: no change at all.
func TestOneHourOffByDefault(t *testing.T) {
	k, fd, clock := testKeeper(t)
	pol := onWaitPolicy()
	pol.OneHourOnWait = false
	start := clock.now()
	recordWait(k, pol, sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if gjson.GetBytes(fd.sent[0].Body, "system.0.cache_control.ttl").Exists() {
		t.Error("the plain policy relabelled a ping")
	}
}

// The run-time check, costly branch: the first 1-hour ping rewrote the prefix. The route is switched
// off, with one WARN, and the next long wait gets auto's plain ping.
func TestCostlySwitchTurnsTheRouteOff(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(r DispatchRequest) (Usage, int, error) {
		if apply.AllBreakpointsAskExtendedTTL(r.Body) {
			return Usage{CacheRead: 0, CacheWrite: 49776, CacheWrite1h: 49776, Output: 1}, http.StatusOK, nil
		}
		return Usage{CacheRead: 49776, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if !apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatal("the first ping was not a 1-hour ping")
	}
	if e, ok := k.Entry(sess1); ok && e.TTL1h {
		t.Error("a costly switch marked the entry 1-hour")
	}
	// Another thread on the same route, a new long wait: plain ping.
	other := Key{Tenant: "t1", Session: "sess-2"}
	t2 := clock.advance(time.Hour)
	recordWait(k, onWaitPolicy(), other, t2, "end_turn", nil)
	k.Sweep(t2.Add(240 * time.Second))
	waitPings(t, k, 2)
	last := fd.sent[len(fd.sent)-1]
	if last.Key != other || apply.AllBreakpointsAskExtendedTTL(last.Body) {
		t.Errorf("the route was not switched off: key %v relabelled=%v", last.Key, apply.AllBreakpointsAskExtendedTTL(last.Body))
	}
}

// "Keep-alive never shortens the cache strategy": a substitute (here the summarizer's call) renews the
// entry at 5 minutes. On a main-thread long wait it must not take the place of the 1-hour ping. The
// substitute takes the first ping and the 1-hour ping follows it, with no second substitute, so the
// wait still ends with the entry at 1 hour (one 1-hour touch).
func TestSubstituteDoesNotReplaceTheOneHourPing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidate(t, "sess-1", st, model)

	proveRoute(k)
	pol := onWaitPolicy()
	start := clock.now()
	for _, d := range []time.Duration{-time.Second, 0} {
		stop := "tool_use"
		var names []string
		if d == 0 {
			stop = "end_turn" // a person: a long wait
		} else {
			names = []string{"Bash"}
		}
		k.Record(Record{Key: sess1, StartedAt: start.Add(d), Body: []byte(kaToolBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: pol, ToolUseNames: names,
			Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: stop}, Header: kaHeader(), Hold: fd.hold(sess1)})
	}
	if n := k.Sweep(start.Add(240 * time.Second)); n != 1 {
		t.Fatalf("the first ping did not fire (%d)", n)
	}
	testutil.WaitFor(t, "the summary substitute", func() bool { return k.summarySubstituted.Load() == 1 })
	if fd.n() != 0 {
		t.Fatalf("the substitute did not take the first ping: %d ordinary pings", fd.n())
	}
	// The 1-hour ping follows, one idle period after the substitute.
	if n := k.Sweep(start.Add(480 * time.Second)); n != 1 {
		t.Fatalf("the 1-hour ping was not planned after the substitute (%d)", n)
	}
	waitPings(t, k, 2)
	if fd.n() != 1 || !apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatalf("the ping after the substitute is not a 1-hour ping (%d sent)", fd.n())
	}
	if model.calls.Load() != 1 {
		t.Errorf("the substitute ran %d times, want 1: the 1-hour ping is never substituted", model.calls.Load())
	}
	if e, ok := k.Entry(sess1); !ok || !e.TTL1h {
		t.Errorf("the wait did not end with a 1-hour entry: %+v %v", e, ok)
	}
	for _, at := range []time.Duration{720, 960, 3600} {
		if n := k.Sweep(start.Add(at * time.Second)); n != 0 {
			t.Fatalf("a further ping fired at %ds", at)
		}
	}
}

// A free 1-hour ping READS and writes nothing at 1h. That is not a downgrade, so it must not lower the
// thread's remembered tier. And a read does not show the tier, so on a route not yet proven the entry is
// not believed to be 1-hour.
func TestFreeOneHourPingDoesNotLowerTheThreadsTier(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 0, CacheWrite1h: 0, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.mu.Lock()
	before, hadBefore := k.tier1h[sess1.id()]
	k.mu.Unlock()
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	k.mu.Lock()
	after, hadAfter := k.tier1h[sess1.id()]
	k.mu.Unlock()
	if before != after || hadBefore != hadAfter {
		t.Errorf("the ping changed the remembered tier: %v/%v -> %v/%v", before, hadBefore, after, hadAfter)
	}
	if e, ok := k.Entry(sess1); ok && e.TTL1h {
		t.Error("a read on an unproven route marked the entry 1-hour")
	}
}

// returnAt records the thread's return: a real 5-minute request that reads read tokens.
func returnAt(k *Keeper, key Key, at time.Time, read int64) {
	k.Record(Record{Key: key, StartedAt: at, Body: []byte(kaBody), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: onWaitPolicy(),
		Usage: Usage{CacheRead: read, CacheWrite: 50000 - read, StopReason: "end_turn"}, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
}

// The grant is UNKNOWN until proven. Unknown: the first long wait probes, the entry is not believed
// 1-hour, and a second wait on the route gets auto's plan (one probe at a time).
func TestUnknownRouteProbesOnceAndIsNotBelieved(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if !apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatal("the probe is not a 1-hour ping")
	}
	if e, ok := k.Entry(sess1); ok && e.TTL1h {
		t.Error("an unproven entry is marked 1-hour")
	}
	if st := routeState(k); st.proven || st.off {
		t.Errorf("state after the probe = %+v, want unknown", st)
	}
	// Another wait on the route while the probe is out: plain auto, with auto's count.
	other := Key{Tenant: "t1", Session: "sess-2"}
	t2 := clock.advance(time.Minute)
	recordWait(k, onWaitPolicy(), other, t2, "end_turn", nil)
	k.Sweep(t2.Add(240 * time.Second))
	waitPings(t, k, 2)
	last := fd.sent[len(fd.sent)-1]
	if last.Key != other || apply.AllBreakpointsAskExtendedTTL(last.Body) {
		t.Fatalf("a second wait probed while the first probe was out (key %v)", last.Key)
	}
	if n := k.Sweep(t2.Add(480 * time.Second)); n != 1 {
		t.Errorf("the fallback wait did not get auto's second ping (%d): its count was not restored", n)
	}
}

// Proven: the return reads the prefix more than 5 minutes after the probe. The route is then proven and
// the next wait's entry is believed 1-hour.
func TestReturnAfterFiveMinutesReadingThePrefixProvesTheGrant(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	// The return: 20 minutes after the wait began, it still READS the prefix.
	returnAt(k, sess1, start.Add(20*time.Minute), 49500)
	if st := routeState(k); !st.proven {
		t.Fatalf("state = %+v, want proven", st)
	}
	n0 := fd.n()
	t2 := clock.advance(time.Hour)
	other := Key{Tenant: "t1", Session: "sess-2"}
	recordWait(k, onWaitPolicy(), other, t2, "end_turn", nil)
	k.Sweep(t2.Add(240 * time.Second))
	waitPings(t, k, 2)
	if !apply.AllBreakpointsAskExtendedTTL(fd.sent[n0].Body) {
		t.Fatal("the next wait on a proven route did not get a 1-hour ping")
	}
	if e, ok := k.Entry(other); !ok || !e.TTL1h {
		t.Errorf("on a proven route the entry is believed 1-hour: %+v %v", e, ok)
	}
}

// Downgrading: the return is a cold rebuild. The entry died at 5 minutes, so the route is switched off.
func TestColdReturnAfterTheProbeSwitchesTheRouteOff(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	returnAt(k, sess1, start.Add(20*time.Minute), 0)
	if st := routeState(k); !st.off {
		t.Fatalf("state = %+v, want off", st)
	}
	n0 := fd.n()
	t2 := clock.advance(time.Hour)
	other := Key{Tenant: "t1", Session: "sess-2"}
	recordWait(k, onWaitPolicy(), other, t2, "end_turn", nil)
	k.Sweep(t2.Add(240 * time.Second))
	waitPings(t, k, 2)
	if apply.AllBreakpointsAskExtendedTTL(fd.sent[n0].Body) {
		t.Error("a route proven to downgrade still got a 1-hour ping")
	}
}

// A return inside 5 minutes of the probe proves nothing: the probe is released, the state stays unknown.
func TestEarlyReturnProvesNothing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	returnAt(k, sess1, start.Add(5*time.Minute), 49500) // 60 s after the ping
	if st := routeState(k); st.proven || st.off || !st.probeUntil.IsZero() {
		t.Errorf("state = %+v, want unknown with the probe released", st)
	}
}

// A 1-hour ping that lands on an entry that had already expired reads ~0 and writes the prefix. It says
// nothing about the route.
func TestMissedOneHourPingDoesNotSwitchTheRouteOff(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 0, CacheWrite: 49776, CacheWrite1h: 49776, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	// A slow tick: the ping goes out 400 s in, after the 5-minute entry is gone.
	k.Sweep(clock.advance(400 * time.Second))
	waitPings(t, k, 1)
	if !apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatal("not a 1-hour ping")
	}
	if st := routeState(k); st.off {
		t.Fatal("a ping on an expired entry switched the route off")
	}
	if st := routeState(k); !st.probeUntil.IsZero() {
		t.Error("the probe was not released")
	}
}

// A route that was switched off after the entry was recorded: the wait is plain auto, with the count the
// plan gave at record time (a person: base + 1).
func TestBlockedAtFireTimeRestoresThePlanCount(t *testing.T) {
	k, fd, clock := testKeeper(t)
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.mu.Lock()
	k.oneHourRoutes[oneHourKeyOf()] = &oneHourRouteState{off: true}
	k.mu.Unlock()
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatal("a ping went out relabelled on a route that is off")
	}
	if n := k.Sweep(start.Add(480 * time.Second)); n != 1 {
		t.Errorf("auto's second ping did not follow (%d): the plan count was not restored", n)
	}
}

// The extra slot (#776) must not follow a 1-hour ping: its request carries 5-minute markers and would
// move the 1-hour entry back to 5 minutes.
func TestNoExtraRequestAfterTheOneHourPing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	proveRoute(k)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored.</summary>"}
	kaCandidate(t, "sess-1", st, model)
	start := clock.now()
	// A body that ends at a text turn: the extra slot would apply.
	for _, d := range []time.Duration{-time.Second, 0} {
		stop, names := "tool_use", []string{"Bash"}
		if d == 0 {
			stop, names = "end_turn", nil
		}
		k.Record(Record{Key: sess1, StartedAt: start.Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: onWaitPolicy(), ToolUseNames: names,
			Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: stop}, Header: kaHeader(), Hold: fd.hold(sess1)})
	}
	k.mu.Lock()
	e := k.live[sess1.id()]
	endsAtText := e != nil && e.endsAtText
	k.mu.Unlock()
	if !endsAtText {
		t.Fatal("the test body does not end at a text turn; the extra slot would not apply")
	}
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	for _, at := range []time.Duration{450, 460, 470, 479} {
		k.Sweep(start.Add(at * time.Second))
	}
	if model.calls.Load() != 0 {
		t.Errorf("the extra substitute ran %d times after the 1-hour ping", model.calls.Load())
	}
}

// The substitute fails: the ordinary ping IS the 1-hour ping, and no second ping follows.
func TestFailedSubstituteLeavesOneOneHourPing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	proveRoute(k)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	kaCandidate(t, "sess-1", st, &fakeSummaryModel{err: errors.New("upstream refused")})
	pol := onWaitPolicy()
	start := clock.now()
	for _, d := range []time.Duration{-time.Second, 0} {
		stop, names := "tool_use", []string{"Bash"}
		if d == 0 {
			stop, names = "end_turn", nil
		}
		k.Record(Record{Key: sess1, StartedAt: start.Add(d), Body: []byte(kaToolBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: pol, ToolUseNames: names,
			Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: stop}, Header: kaHeader(), Hold: fd.hold(sess1)})
	}
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if fd.n() != 1 || !apply.AllBreakpointsAskExtendedTTL(fd.sent[0].Body) {
		t.Fatalf("the fallback ping is not the 1-hour ping (%d sent)", fd.n())
	}
	if n := k.Sweep(start.Add(480 * time.Second)); n != 0 {
		t.Errorf("a second ping followed a failed substitute (%d)", n)
	}
}

// returnWith records the thread's return with full control of the request: the model and whether the client
// rewrote the prefix. The usage is a cold rebuild unless read is large.
func returnWith(k *Keeper, key Key, at time.Time, body string, read, write int64, rewritten bool) {
	k.Record(Record{Key: key, StartedAt: at, Body: []byte(body), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: onWaitPolicy(), PrefixRewritten: rewritten,
		Usage: Usage{CacheRead: read, CacheWrite: write, StopReason: "end_turn"}, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
}

func probeThenWait(t *testing.T) (*Keeper, time.Time) {
	t.Helper()
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	return k, start
}

// A cold return caused by the client (its own compaction, or a fork or rewind) is not a downgrade: the
// route stays unknown and the probe is released.
func TestColdReturnAfterAClientCompactionIsNotADowngrade(t *testing.T) {
	k, start := probeThenWait(t)
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 0, 50000, true)
	if st := routeState(k); st.off || st.proven || !st.probeUntil.IsZero() {
		t.Errorf("state = %+v, want unknown with the probe released", st)
	}
}

// A cold return on another model is not a downgrade either: the provider keeps a cache for each model.
func TestColdReturnOnAnotherModelIsNotADowngrade(t *testing.T) {
	k, start := probeThenWait(t)
	other := strings.Replace(kaBody, "aws/claude-sonnet-5", "aws/claude-opus-5", 1)
	returnWith(k, sess1, start.Add(20*time.Minute), other, 0, 50000, false)
	if st := routeState(k); st.off || st.proven {
		t.Errorf("state = %+v, want unknown", st)
	}
}

// A return that read and wrote nothing (an uncacheable request) is not a cold rebuild.
func TestReturnWithNoCacheActivityIsNotADowngrade(t *testing.T) {
	k, start := probeThenWait(t)
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 0, 0, false)
	if st := routeState(k); st.off {
		t.Errorf("state = %+v: a return with no cache activity switched the route off", st)
	}
}

// The control for the three above: the same cold return, from the same model, with the prefix untouched,
// is a downgrade. The tests above are not vacuous.
func TestColdRebuildFromTheSameModelWithTheSamePrefixIsADowngrade(t *testing.T) {
	k, start := probeThenWait(t)
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 0, 50000, false)
	if st := routeState(k); !st.off {
		t.Errorf("state = %+v, want off", st)
	}
}

// ReadSetsTTL is how the accountant is told a read sets the TTL. A proven route: always. An unproven route:
// only for the return that follows a 1-hour ping, so the return that PROVES the route is credited in the
// same pass; a return that is cold then switches the route off and is never credited. Off: never.
func TestReadSetsTTLForTheAccountant(t *testing.T) {
	k, start := probeThenWait(t)
	g := func(model string, pinged bool) bool {
		return k.ReadSetsTTL(bschemas.Anthropic, "", "/v1/messages", model, pinged)
	}
	const m = "aws/claude-sonnet-5"
	if g(m, false) {
		t.Error("an unproven route with no 1-hour ping before the request: ReadSetsTTL on")
	}
	if !g(m, true) {
		t.Error("the return after a 1-hour ping is not priced as one (the proof return would get no rescue)")
	}
	// The proof arrives: the route is proven, and stays on for every request.
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 49500, 100, false)
	if st := routeState(k); !st.proven {
		t.Fatalf("state = %+v, want proven", st)
	}
	if !g(m, false) || !g(m, true) {
		t.Error("a proven route is not ReadSetsTTL")
	}
	if g("aws/claude-opus-5", false) {
		t.Error("the proof of one model counted for another")
	}
	var nilK *Keeper
	if nilK.ReadSetsTTL(bschemas.Anthropic, "", "/v1/messages", "m", true) {
		t.Error("a nil keeper reported it")
	}
}

// The first return is cold: the route is switched off, and ReadSetsTTL is off after that, pinged or not.
func TestReadSetsTTLIsOffOnceTheColdReturnSwitchedTheRouteOff(t *testing.T) {
	k, start := probeThenWait(t)
	if !k.ReadSetsTTL(bschemas.Anthropic, "", "/v1/messages", "aws/claude-sonnet-5", true) {
		t.Fatal("not priced as a return after a 1-hour ping before the return")
	}
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 0, 50000, false)
	if st := routeState(k); !st.off {
		t.Fatalf("state = %+v, want off", st)
	}
	if k.ReadSetsTTL(bschemas.Anthropic, "", "/v1/messages", "aws/claude-sonnet-5", true) {
		t.Error("ReadSetsTTL stayed on after the route was switched off")
	}
}

// #1243, the band: after a FREE 1-hour ping the clock counts an hour from it, until the thread's next real
// request or an hour from the ping. OneHourTouches is the one source; the dashboard reads it.
func touchOf(k *Keeper, key Key) (OneHourTouch, bool) {
	m := k.OneHourTouches(key.Tenant, false, key.Session)
	t, ok := m[key.Thread]
	return t, ok
}

func TestFreeOneHourPingOnAnUnprovenRouteGivesAnUnprovenHour(t *testing.T) {
	k, _ := probeThenWait(t)
	got, ok := touchOf(k, sess1)
	if !ok {
		t.Fatal("no 1-hour touch after a free 1-hour ping")
	}
	if got.Proven {
		t.Error("an unproven route reported as proven: the band would not show 1h?")
	}
}

func TestFreeOneHourPingOnAProvenRouteGivesAPlainHour(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 0, Output: 1}, http.StatusOK, nil
	})
	proveRoute(k)
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	got, ok := touchOf(k, sess1)
	if !ok || !got.Proven {
		t.Fatalf("touch = %+v %v, want a proven hour", got, ok)
	}
}

// The next real request on the thread ends it: the clock returns to the billed tier.
func TestARealRequestEndsTheOneHourTouch(t *testing.T) {
	k, start := probeThenWait(t)
	if _, ok := touchOf(k, sess1); !ok {
		t.Fatal("no touch to end")
	}
	returnWith(k, sess1, start.Add(10*time.Minute), kaBody, 49500, 100, false)
	if _, ok := touchOf(k, sess1); ok {
		t.Error("the touch outlived the thread's next real request")
	}
}

// An hour after the ping the entry is cold: the fact is gone.
func TestTheOneHourTouchEndsAnHourAfterThePing(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 0, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	clock.advance(59 * time.Minute)
	if _, ok := touchOf(k, sess1); !ok {
		t.Fatal("the touch ended before the hour")
	}
	clock.advance(2 * time.Minute) // 61 minutes after the ping
	if _, ok := touchOf(k, sess1); ok {
		t.Error("the touch outlived its hour")
	}
}

// A route switched off never shows an hour, even for a touch recorded before it went off.
func TestASwitchedOffRouteNeverShowsAnHour(t *testing.T) {
	k, _ := probeThenWait(t)
	if _, ok := touchOf(k, sess1); !ok {
		t.Fatal("no touch")
	}
	k.mu.Lock()
	k.oneHourRoutes[oneHourKeyOf()] = &oneHourRouteState{off: true}
	k.mu.Unlock()
	if _, ok := touchOf(k, sess1); ok {
		t.Error("a route that is off still shows an hour")
	}
}

// A ping that is not free (it rewrote the prefix) leaves no touch.
func TestACostlyPingLeavesNoOneHourTouch(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 0, CacheWrite: 49776, CacheWrite1h: 49776, Output: 1}, http.StatusOK, nil
	})
	start := clock.now()
	recordWait(k, onWaitPolicy(), sess1, start, "end_turn", nil)
	k.Sweep(start.Add(240 * time.Second))
	waitPings(t, k, 1)
	if _, ok := touchOf(k, sess1); ok {
		t.Error("a costly ping left a 1-hour touch")
	}
}

// The touch is per session and tenant: another session sees none.
func TestOneHourTouchesAreScopedToTheSession(t *testing.T) {
	k, _ := probeThenWait(t)
	if m := k.OneHourTouches(sess1.Tenant, false, "other-session"); len(m) != 0 {
		t.Errorf("another session sees %v", m)
	}
	if m := k.OneHourTouches("another-tenant", false, sess1.Session); len(m) != 0 {
		t.Errorf("another tenant sees %v", m)
	}
	if m := k.OneHourTouches("another-tenant", true, sess1.Session); len(m) != 1 {
		t.Errorf("an all-tenant read sees %v, want the touch", m)
	}
	var nilK *Keeper
	if nilK.OneHourTouches("t", false, "s") != nil {
		t.Error("a nil keeper returned touches")
	}
}

// logBuf captures what slog writes while a test runs. It restores the old logger in the cleanup.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *logBuf {
	t.Helper()
	buf := &logBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The probe leaves a trail at INFO: started (route, model, session, thread), proven, released. It was
// invisible before: the question "did the probe run" could only be answered by inference.
func TestTheProbeIsLoggedAtInfo(t *testing.T) {
	logs := captureLog(t)
	k, start := probeThenWait(t)
	got := logs.String()
	for _, want := range []string{"1-hour probe started on an unproven route", "model=aws/claude-sonnet-5", "session=sess-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the start line lacks %q:\n%s", want, got)
		}
	}
	returnWith(k, sess1, start.Add(20*time.Minute), kaBody, 49500, 100, false)
	if got := logs.String(); !strings.Contains(got, "1-hour keep-alive grant proven") {
		t.Errorf("no proven line:\n%s", got)
	}
}

func TestAReleasedProbeIsLoggedAndASwitchOffIsLogged(t *testing.T) {
	logs := captureLog(t)
	k, start := probeThenWait(t)
	returnWith(k, sess1, start.Add(5*time.Minute), kaBody, 49500, 100, false) // inside 5 minutes of the ping
	if got := logs.String(); !strings.Contains(got, "1-hour probe released") {
		t.Errorf("no released line:\n%s", got)
	}
	k2, start2 := probeThenWait(t)
	returnWith(k2, sess1, start2.Add(20*time.Minute), kaBody, 0, 50000, false) // cold
	if got := logs.String(); !strings.Contains(got, "no more 1-hour keep-alive pings on this route") {
		t.Errorf("no switch-off line:\n%s", got)
	}
}

// A manual "Ping now" sends the held body with 5-minute markers, which would move a 1-hour entry back to
// 5 minutes. The keeper refuses a press while the thread's free 1-hour touch is live. It sends nothing, claims
// no gap, and the touch and the clock stay (#1259).
func TestAManualPressOnALiveOneHourTouchIsRefusedAndChangesNothing(t *testing.T) {
	k, _ := probeThenWait(t)
	before, ok := touchOf(k, sess1)
	if !ok {
		t.Fatal("no touch to protect")
	}
	sent := fakeOf(k).n()
	for range 2 { // the second press shows the first claimed no gap
		if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: time.Hour}); !errors.Is(err, ErrAtOneHour) {
			t.Fatalf("PingNow = %v, want ErrAtOneHour", err)
		}
	}
	if got := fakeOf(k).n(); got != sent {
		t.Errorf("a refused press sent %d request(s)", got-sent)
	}
	after, ok := touchOf(k, sess1)
	if !ok || after != before {
		t.Errorf("touch = %+v %v, want it unchanged: %+v", after, ok, before)
	}
}

// A press goes through once the touch is not live: a real request ended it, or its hour passed. The press then
// clears a stale touch (it is the thread's new last touch) and sends one ping.
func TestAManualPressAfterTheOneHourTouchEndedStillWorks(t *testing.T) {
	k, start := probeThenWait(t)
	returnWith(k, sess1, start.Add(10*time.Minute), kaBody, 49500, 100, false) // a real request ends the touch
	sent := fakeOf(k).n()
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: time.Second}); err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	if got := fakeOf(k).n(); got != sent+1 {
		t.Errorf("a press sent %d request(s), want 1", got-sent)
	}
}

// A touch an hour old is no longer live (the entry is cold): the press is not refused, and it clears the
// stale fact as it dispatches.
func TestAManualPressClearsAnExpiredOneHourTouch(t *testing.T) {
	k, _ := probeThenWait(t)
	k.mu.Lock()
	for id, tc := range k.oneHourTouches {
		tc.at = k.now().Add(-61 * time.Minute)
		k.oneHourTouches[id] = tc
	}
	k.mu.Unlock()
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: time.Second}); err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	k.mu.Lock()
	n := len(k.oneHourTouches)
	k.mu.Unlock()
	if n != 0 {
		t.Errorf("%d stale touch(es) left after a dispatched press", n)
	}
}
