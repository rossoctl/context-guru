package keepalive

import (
	"errors"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
)

// #1365. The fact "the last touch of this thread was a free 1-hour ping" has ONE record: the store's cg:seen:
// value. The dashboard clock, the manual-ping refusal and the cold check all read it, so they cannot split.
// Each test drives one event through the real keeper and the real apply, then asks all three readers at the
// same moment. Where an event used to leave the keeper's own copy and the stored copy apart (3 review rounds of
// #1357 each found one), the readers must now agree.

// readers asks the 3 readers of the fact. dash is the cache clock's view, press is the manual-ping refusal, and
// warm is the cold check's verdict for a return at at (it runs a real request there, so it is asked last).
func (r *coldRig) readers(at time.Time) (dash, refused, warm bool) {
	r.t.Helper()
	_, dash = touchOf(r.k, sess1)
	refused = r.k.OneHourTouchLive(sess1)
	cold, _ := r.request(at)
	return dash, refused, !cold
}

// agree fails unless the 3 readers say the same, and says want.
func (r *coldRig) agree(at time.Time, want bool, event string) {
	r.t.Helper()
	dash, refused, warm := r.readers(at)
	if dash != want || refused != want || warm != want {
		r.t.Errorf("%s: dashboard shows the hour=%v, manual ping refused=%v, cold check warm=%v; all must be %v",
			event, dash, refused, warm, want)
	}
}

func TestOneRecordAfterAPing(t *testing.T) {
	r, t0 := pingedThread(t)
	r.agree(t0.Add(13*time.Minute), true, "after a free 1-hour ping")
}

func TestOneRecordAfterAnAcceptedRequest(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.recordStatus(t0.Add(6*time.Minute), 200)
	r.agree(t0.Add(13*time.Minute), false, "after an accepted real request")
}

func TestOneRecordAfterAFailedRequest(t *testing.T) {
	for _, status := range []int{0, 400, 429, 500} { // 0: the host never called Record
		r, t0 := pingedThread(t)
		r.request(t0.Add(6 * time.Minute))
		if status != 0 {
			r.recordStatus(t0.Add(6*time.Minute), status)
		}
		r.agree(t0.Add(13*time.Minute), true, "after a failed real request")
	}
}

func TestOneRecordAfterTheRouteIsSwitchedOff(t *testing.T) {
	r, t0 := pingedThread(t)
	r.k.oneHourSwitchOff(oneHourKeyOf(), bschemas.Anthropic, "/v1/messages", "", "aws/claude-sonnet-5", "test")
	r.agree(t0.Add(13*time.Minute), false, "after the route was switched off")
}

// The route is switched off while a real request that has not been confirmed stands over the ping: the carried
// ping ends too.
func TestOneRecordAfterTheRouteIsSwitchedOffUnderAnUnconfirmedRequest(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.k.oneHourSwitchOff(oneHourKeyOf(), bschemas.Anthropic, "/v1/messages", "", "aws/claude-sonnet-5", "test")
	r.agree(t0.Add(13*time.Minute), false, "after a switch-off under an unconfirmed request")
}

// A second ping after a confirmed request writes the tier again, and it is the record.
func TestOneRecordAfterALaterPing(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.recordStatus(t0.Add(6*time.Minute), 200)
	r.recordPing(t0.Add(7*time.Minute), true)
	r.agree(t0.Add(13*time.Minute), true, "after a later free ping")
}

// A manual press after the touch ended is a 5-minute touch: it moves the entry back, and every reader says so.
func TestOneRecordAfterAManualPress(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.recordStatus(t0.Add(6*time.Minute), 200)
	r.clock.advance(7 * time.Minute)
	if _, err := r.k.PingNow(ManualRequest{Key: sess1, MinGap: time.Second}); err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	// The press at +11 minutes is the last touch, at 5 minutes: the entry is gone 6 minutes later.
	r.agree(t0.Add(18*time.Minute), false, "after a manual 5-minute press")
}

// A press while the ping is live is refused and changes nothing: the record is the same afterwards.
func TestOneRecordAfterARefusedPress(t *testing.T) {
	r, t0 := pingedThread(t)
	if _, err := r.k.PingNow(ManualRequest{Key: sess1, MinGap: time.Second}); !errors.Is(err, ErrAtOneHour) {
		t.Fatalf("PingNow = %v, want ErrAtOneHour", err)
	}
	r.agree(t0.Add(13*time.Minute), true, "after a refused press")
}

// The capacity bound loses only the index entry. It may leave the dashboard behind the cold check (the entry is
// warm and the keeper no longer lists the thread), and it must never leave it AHEAD: no hour shown that the cold
// check denies. The stored tier stays, so the cold check keeps reading the entry warm.
func TestAfterTheCapacityBoundTheDashboardNeverShowsMoreThanTheColdCheck(t *testing.T) {
	r, t0 := pingedThread(t)
	r.k.mu.Lock()
	r.k.oneHourThreads = map[string]oneHourThread{}
	r.k.mu.Unlock()
	dash, refused, warm := r.readers(t0.Add(13 * time.Minute))
	if (dash || refused) && !warm {
		t.Errorf("dashboard=%v refused=%v but the cold check reads cold", dash, refused)
	}
	if !warm {
		t.Error("the capacity bound ended the stored tier")
	}
}

// What the keeper keeps per thread holds no tier and no time. The reflection test below fails if one is added.
func TestTheThreadIndexHoldsNoTierAndNoTime(t *testing.T) {
	typ := reflectTypeOfThread()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.String() == "time.Time" || f.Type.Kind().String() == "bool" || f.Type.String() == "time.Duration" ||
			f.Type.Kind().String() == "int64" || f.Type.Kind().String() == "int" {
			t.Errorf("oneHourThread.%s (%s) is a tier or a time: the store's record is the only one", f.Name, f.Type)
		}
	}
}

// A free ping that the store never recorded (the keeper judged it free, the touch write was not made) is not a
// touch: the keeper's index alone shows nothing.
func TestAnIndexEntryWithoutAStoredPingShowsNothing(t *testing.T) {
	r, _ := pingedThread(t)
	st := r.st
	st.Put(store.SeenPrefix+r.alias, []byte("1")) // a plain clock: the record says no ping
	if _, ok := touchOf(r.k, sess1); ok {
		t.Error("the dashboard shows an hour the record does not hold")
	}
	if r.k.OneHourTouchLive(sess1) {
		t.Error("the manual ping is refused for an hour the record does not hold")
	}
	st.Put(store.SeenPrefix+r.alias, []byte("garbage:1h")) // an unreadable record is not a tier either
	if _, ok := touchOf(r.k, sess1); ok {
		t.Error("an unreadable record read as a tier")
	}
}

// recordPing plays a free 1-hour ping's touch: the stored write the keeper's ping makes, and the index entry that
// judgeOneHour adds.
func (r *coldRig) recordPing(at time.Time, oneHour bool) {
	r.t.Helper()
	r.k.mu.Lock()
	th := r.k.oneHourThreads[sess1.id()]
	r.k.mu.Unlock()
	if th.st == nil {
		th.st = r.st
	}
	apply.RecordCacheTouchAliasTier(r.st, r.alias, at.UnixMilli(), oneHour)
	r.k.mu.Lock()
	r.k.oneHourThreads[sess1.id()] = oneHourThread{key: sess1, route: oneHourKeyOf(), provider: bschemas.Anthropic,
		model: "aws/claude-sonnet-5", st: r.st, alias: r.alias}
	r.k.mu.Unlock()
}

// After an accepted real request the thread has no index entry. A later 1-hour touch on the same alias that no
// index entry points to (another thread's ping, a D or C write) is therefore not ended by the switch-off of the
// route the old ping used: the switch-off ends only what it granted.
func TestASwitchOffEndsOnlyThePingsOfItsRoute(t *testing.T) {
	r, t0 := pingedThread(t)
	r.request(t0.Add(6 * time.Minute))
	r.recordStatus(t0.Add(6*time.Minute), 200)
	r.k.mu.Lock()
	_, listed := r.k.oneHourThreads[sess1.id()]
	r.k.mu.Unlock()
	if listed {
		t.Fatal("an accepted real request left the thread in the index")
	}
	apply.RecordCacheTouchAliasTier(r.st, r.alias, t0.Add(7*time.Minute).UnixMilli(), true) // a later 1-hour touch
	r.k.oneHourSwitchOff(oneHourKeyOf(), bschemas.Anthropic, "/v1/messages", "", "aws/claude-sonnet-5", "test")
	if cold, _ := r.request(t0.Add(13 * time.Minute)); cold {
		t.Error("the switch-off of an old ping's route ended a later touch's tier")
	}
}

// Defense in depth: a route that is off hides the hour from the dashboard and does not refuse a press, even if the
// store record still stands.
func TestARouteThatIsOffShowsNoHourAndRefusesNoPress(t *testing.T) {
	r, _ := pingedThread(t)
	r.k.mu.Lock()
	r.k.oneHourRoutes[oneHourKeyOf()] = &oneHourRouteState{off: true}
	r.k.mu.Unlock()
	if _, ok := touchOf(r.k, sess1); ok {
		t.Error("the dashboard shows an hour from a route that is off")
	}
	if r.k.OneHourTouchLive(sess1) {
		t.Error("a route that is off still refuses the press")
	}
}

// A costly route (the switch rewrote the prefix) grants no free hour: the dashboard shows none. The store record
// stays, because the entry really was rewritten at 1 hour, so the cold check keeps reading it warm.
func TestACostlyRouteShowsNoFreeHourButTheColdCheckKeepsIt(t *testing.T) {
	r, t0 := pingedThread(t)
	r.k.mu.Lock()
	r.k.oneHourRoutes[oneHourKeyOf()] = &oneHourRouteState{costly: true}
	r.k.mu.Unlock()
	dash, _, warm := r.readers(t0.Add(13 * time.Minute))
	if dash {
		t.Error("the dashboard shows a free hour on a costly route")
	}
	if !warm {
		t.Error("the cold check lost the hour a rewrite really gave")
	}
}
