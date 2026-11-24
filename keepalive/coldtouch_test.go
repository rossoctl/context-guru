package keepalive

import (
	"context"
	"net/http"
	"strconv"
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

// One fact, two indexes: when the route that gave the 1-hour tier is switched off, the keeper's touch ends
// AND the stored mirror the cold check reads ends with it. Without that the cold check would keep an hour for a
// tier the keeper no longer believes.
func TestSwitchingTheRouteOffEndsTheStoredTierToo(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	proveRoute(r.k)
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)
	if _, ok := touchOf(r.k, sess1); !ok {
		t.Fatal("fixture: no free 1-hour touch")
	}
	r.k.oneHourSwitchOff(oneHourKeyOf(), bschemas.Anthropic, "/v1/messages", "", "aws/claude-sonnet-5", "test")
	if _, ok := touchOf(r.k, sess1); ok {
		t.Error("the keeper still holds a touch of a route that is off")
	}
	if cold, _ := r.request(t0.Add(13 * time.Minute)); !cold {
		t.Error("the cold check kept an hour for a tier the keeper dropped")
	}
}

// A sub-agent's real request is its own thread: it ends its own touch only, never the main thread's.
func TestASubAgentRequestKeepsTheMainThreadsOneHourTouch(t *testing.T) {
	k, _ := probeThenWait(t)
	if _, ok := touchOf(k, sess1); !ok {
		t.Fatal("fixture: no touch on the main thread")
	}
	sub := Key{Tenant: sess1.Tenant, Session: sess1.Session, Thread: "a:sub1"}
	k.Record(Record{Key: sub, StartedAt: k.now().Add(6 * time.Minute), Body: []byte(kaBody), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, UsageOK: true, Policy: onWaitPolicy(),
		Usage: Usage{CacheRead: 40000, CacheWrite: 100, StopReason: "end_turn"}, Header: kaHeader(), Hold: fakeOf(k).hold(sub)})
	if _, ok := touchOf(k, sess1); !ok {
		t.Error("a sub-agent request ended the main thread's 1-hour touch")
	}
}

// pingedThread builds the field shape: a request, then a free 1-hour ping at +4 minutes.
func pingedThread(t *testing.T) (*coldRig, time.Time) {
	t.Helper()
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	proveRoute(r.k)
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)
	if _, ok := touchOf(r.k, sess1); !ok {
		t.Fatal("fixture: no free 1-hour touch")
	}
	return r, t0
}

// recordStatus hands the keeper the end of a real request that ended with status.
func (r *coldRig) recordStatus(at time.Time, status int) {
	r.k.Record(Record{Key: sess1, StartedAt: at, Body: []byte(kaBody), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: status, UsageOK: status == 200, Policy: onWaitPolicy(),
		Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"}, Header: kaHeader(),
		Hold: fakeOf(r.k).hold(sess1), Store: r.st, TouchAlias: r.alias})
}

// A real request that FAILS touched nothing at the provider, so the ping's hour still runs. Apply wrote the plain
// clock before the forward, so the tier must be carried until the keeper confirms an accepted request. Every
// failure path leaves the keeper's touch and the stored tier together: a 429, a 5xx and a 4xx never reach the
// line that ends them, and a timeout, a cancel or no Record call never reach it either.
func TestAFailedRealRequestKeepsTheOneHourPing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int // 0: the host never called Record (a timeout or a cancel)
	}{{"429", 429}, {"500", 500}, {"400", 400}, {"no Record call", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			r, t0 := pingedThread(t)
			r.request(t0.Add(6 * time.Minute)) // apply writes its clock before the forward
			if tc.status != 0 {
				r.recordStatus(t0.Add(6*time.Minute), tc.status)
			}
			if _, ok := touchOf(r.k, sess1); !ok {
				t.Fatal("a failed request ended the keeper's touch")
			}
			if cold, _ := r.request(t0.Add(13 * time.Minute)); cold {
				t.Fatal("a return after a failed request read COLD on an entry the ping holds for an hour")
			}
		})
	}
}

// An accepted real request is the next touch: it ends both records, and 7 minutes later the entry is cold.
func TestAnAcceptedRealRequestEndsBothRecordsOfTheTier(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.recordStatus(t0.Add(6*time.Minute), 200)
	if _, ok := touchOf(r.k, sess1); ok {
		t.Error("the keeper kept its touch after an accepted request")
	}
	if cold, _ := r.request(t0.Add(13 * time.Minute)); !cold {
		t.Error("the stored tier outlived an accepted real request")
	}
}

// A substitute question carries the forwarded body's own markers. Plain markers move a 1-hour entry back to 5
// minutes, so the touch must not claim the hour, even on a thread whose entry the keeper holds at 1 hour.
func TestASubstituteTouchTakesItsTierFromTheBodysMarkers(t *testing.T) {
	r, _ := pingedThread(t)
	hour, _ := apply.RelabelAllTTL1h([]byte(kaBody), bschemas.Anthropic)
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{{"plain markers", []byte(kaBody), ""}, {"1-hour markers", hour, ":1h"}} {
		t.Run(tc.name, func(t *testing.T) {
			r.k.mu.Lock()
			e := r.k.live[sess1.id()]
			e.ttl1h = true
			r.k.mu.Unlock()
			st := store.NewMemory(store.Options{})
			j := pingJob{e: e, tenant: e.tenant, session: e.session, key: e.key, st: st, touchAlias: "al",
				provider: bschemas.Anthropic, raw: tc.raw}
			r.k.recordSubstitutePing(j, "", Usage{CacheRead: 40_000, Output: 1}, 5, r.clock.now(), 0, nil)
			b, _ := st.Get(store.SeenPrefix + "al")
			if got := string(b); got != strconv.FormatInt(r.clock.now().UnixMilli(), 10)+tc.want {
				t.Errorf("clock = %q, want the time and tier %q", got, tc.want)
			}
		})
	}
}

// The touch map is bounded. That is a capacity event, not a fact about any entry: it must not end the stored tier
// of a live touch, or the cold check reads a warm 1-hour entry as cold and a cold cut rewrites it.
func TestTheCapacityBoundDoesNotEndALiveTouchsTier(t *testing.T) {
	r, t0 := pingedThread(t)
	now := r.clock.now()
	r.k.mu.Lock()
	live := r.k.oneHourTouches[sess1.id()] // the real touch, with its store and alias
	r.k.oneHourTouches = map[string]oneHourTouch{}
	old := now.Add(-2 * time.Hour)
	for i := 0; i < MaxTurnKeys-1; i++ { // dead touches fill the map first
		id := "dead" + strconv.Itoa(i)
		r.k.oneHourTouches[id] = oneHourTouch{key: Key{Tenant: "t", Session: id}, at: old, provider: bschemas.Anthropic, model: "m"}
	}
	r.k.oneHourTouches[sess1.id()] = live
	r.k.makeRoomForTouchLocked(now.Add(0)) // evicts the dead ones, keeps the live one
	n := len(r.k.oneHourTouches)
	_, kept := r.k.oneHourTouches[sess1.id()]
	// Now every touch is live: the map starts over, and the stored tier must stay.
	for i := 0; i < MaxTurnKeys; i++ {
		id := "live" + strconv.Itoa(i)
		r.k.oneHourTouches[id] = oneHourTouch{key: Key{Tenant: "t", Session: id}, at: now, provider: bschemas.Anthropic, model: "m"}
	}
	// The map starts over only when all are live: the real touch is among them and is dropped from the keeper.
	r.k.makeRoomForTouchLocked(now)
	r.k.mu.Unlock()
	if !kept || n != 1 {
		t.Errorf("the bound dropped a live touch: kept=%v, %d left, want only the live one", kept, n)
	}
	if cold, _ := r.request(t0.Add(13 * time.Minute)); cold {
		t.Error("the capacity bound ended the stored tier: a warm 1-hour entry read cold")
	}
}
