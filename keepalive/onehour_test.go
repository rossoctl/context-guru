package keepalive

import (
	"net/http"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/waitcause"
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
	// A free switch: the session is a 1-hour session until the next real request. It is checked
	// right after the ping, before any sweep drops the finished entry.

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

// A free switch leaves the route on: the next long wait gets a 1-hour ping again.
func TestFreeSwitchKeepsTheRouteOn(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheRead: 49500, CacheWrite: 50, Output: 1}, http.StatusOK, nil
	})
	for i, key := range []Key{sess1, {Tenant: "t1", Session: "sess-2"}} {
		at := clock.advance(time.Hour)
		recordWait(k, onWaitPolicy(), key, at, "end_turn", nil)
		k.Sweep(at.Add(240 * time.Second))
		waitPings(t, k, int64(i+1))
		if !apply.AllBreakpointsAskExtendedTTL(fd.sent[len(fd.sent)-1].Body) {
			t.Fatalf("wait %d: not a 1-hour ping", i+1)
		}
	}
}

// A free 1-hour ping READS and writes nothing at 1h. That is not a downgrade, so it must not lower the
// thread's remembered tier: a downgrade can only be judged on a request that wrote tokens.
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
	if e, ok := k.Entry(sess1); !ok || !e.TTL1h {
		t.Errorf("a read with 1h markers is a free switch, not a downgrade: entry %+v found %v", e, ok)
	}
}
