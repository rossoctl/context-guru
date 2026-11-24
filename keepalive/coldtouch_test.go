package keepalive

import (
	"context"
	"net/http"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// #1350. A free 1-hour ping leaves the provider's entry at 1 hour. The cold check of the next real request
// must count the 1-hour lifetime from that ping, not the 5 minutes the thread's own request asks for.
// These tests drive the real keeper and the real apply, joined through the store, so they fail when the
// keeper stops telling the cold check, or when the cold check stops asking.

// coldSpy records Ctx.ColdCache and Ctx.CacheTTLMs as the pipeline hands them over.
type coldSpy struct {
	cold *[]bool
	ttl  *[]int64
}

func (coldSpy) Name() string                 { return "coldspy" }
func (coldSpy) Enabled(*components.Ctx) bool { return true }
func (s coldSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.cold = append(*s.cold, c.ColdCache)
	*s.ttl = append(*s.ttl, c.CacheTTLMs)
	return nil
}

// coldRig is one thread with a keeper and an apply, sharing one store.
type coldRig struct {
	t     *testing.T
	k     *Keeper
	clock *testClock
	st    store.Store
	tr    *modes.Tracker
	pipe  *components.Pipeline
	cold  []bool
	ttl   []int64
	alias string
}

func newColdRig(t *testing.T, usage Usage) *coldRig {
	t.Helper()
	k, fd, clock := testKeeper(t)
	fd.setResp(func(DispatchRequest) (Usage, int, error) { return usage, http.StatusOK, nil })
	r := &coldRig{t: t, k: k, clock: clock, st: store.NewMemory(store.Options{}), tr: modes.NewTracker(0)}
	r.pipe = components.NewPipeline([]components.Component{coldSpy{&r.cold, &r.ttl}}, nil)
	return r
}

// request runs apply for a real request at the given time and returns what the cold check said.
func (r *coldRig) request(at time.Time) (cold bool, ttl time.Duration) {
	r.t.Helper()
	res := apply.BodyOpts(context.Background(), r.pipe, r.st, apply.Opts{
		Provider: bschemas.Anthropic, Body: []byte(kaBody), Tracker: r.tr, Now: at, Tenant: "t",
	})
	r.alias = res.TouchAlias
	if len(r.cold) == 0 {
		r.t.Fatal("the pipeline did not run")
	}
	return r.cold[len(r.cold)-1], time.Duration(r.ttl[len(r.ttl)-1]) * time.Millisecond
}

// record hands the finished requests to the keeper, as the host does after each response: a tool turn, then
// the turn that ends with stop (recordWait's shape, so the thread is pinged at all, #858).
func (r *coldRig) record(at time.Time, stop string, pol Policy) {
	u := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use"}
	for i, names := range [][]string{{"Bash"}, nil} {
		when := at.Add(-time.Second)
		if i == 1 {
			u.StopReason, when = stop, at
		}
		r.k.Record(Record{Key: sess1, StartedAt: when, Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: u, UsageOK: true, Policy: pol,
			ToolUseNames: names, Header: kaHeader(), Hold: fakeOf(r.k).hold(sess1), Store: r.st, TouchAlias: r.alias})
	}
}

// The shape from the field: the thread waits for a person, the keeper sends its one free 1-hour ping, and the
// person returns 13 minutes after the request. The entry is warm for another 51 minutes.
func TestAReturnAfterAFreeOneHourPingIsNotCold(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1}) // read = prefix, write = 0
	proveRoute(r.k)
	t0 := r.clock.now()
	if cold, _ := r.request(t0); cold {
		t.Fatal("the first request read cold")
	}
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)
	if _, ok := touchOf(r.k, sess1); !ok {
		t.Fatal("fixture: the keeper did not see a free 1-hour ping")
	}

	cold, ttl := r.request(t0.Add(13 * time.Minute))
	if cold {
		t.Fatalf("a return 13 minutes after the request, 9 minutes after a free 1-hour ping, read COLD "+
			"(ttl %v): a cold-only cut would rewrite a warm 1-hour entry", ttl)
	}
	if ttl != time.Hour {
		t.Errorf("Ctx.CacheTTLMs = %v, want the 1-hour lifetime the entry has", ttl)
	}
}

// The hour is a limit: just inside it the entry is warm, and past it the entry is gone.
func TestAReturnJustInsideTheHourFromAFreeOneHourPingIsWarm(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	proveRoute(r.k)
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)

	// The ping started at t0+4m. 59 minutes later the entry still lives.
	if cold, _ := r.request(t0.Add(4*time.Minute + 59*time.Minute)); cold {
		t.Error("59 minutes after the ping read cold: the entry still lives")
	}
}

func TestAReturnPastTheHourFromAFreeOneHourPingIsCold(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	proveRoute(r.k)
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)

	if cold, _ := r.request(t0.Add(4*time.Minute + 62*time.Minute)); !cold {
		t.Error("62 minutes after the ping read warm: the 1-hour lifetime must end")
	}
}

// A ping with the plain 5-minute markers leaves the entry at 5 minutes: nothing changes for auto.
func TestAReturnAfterAFiveMinutePingStaysCold(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", kaPolicy()) // auto: the plain ping, no 1-hour relabel
	r.clock.advance(281 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)
	if cold, _ := r.request(t0.Add(11 * time.Minute)); !cold {
		t.Error("a return 6 minutes after a 5-minute ping read warm")
	}
}
