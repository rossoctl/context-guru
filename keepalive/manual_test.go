package keepalive

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
)

// reportSink is an Emitter that keeps every keep-alive report, so a test can see exactly what a
// host's recorder would have been told.
type reportSink struct {
	mu   sync.Mutex
	reps []components.KeepAliveReport
}

func (*reportSink) Component(components.Report) {}
func (*reportSink) Run(components.RunReport)    {}
func (s *reportSink) KeepAlivePing(r components.KeepAliveReport) {
	s.mu.Lock()
	s.reps = append(s.reps, r)
	s.mu.Unlock()
}
func (s *reportSink) all() []components.KeepAliveReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]components.KeepAliveReport(nil), s.reps...)
}

// recordWithSink records the two turns a pingable thread needs, reporting through sink.
func recordWithSink(k *Keeper, sink *reportSink, key Key, at time.Time) {
	recordBodyWithSink(k, sink, key, at, kaBody)
}

// recordBodyWithSink is recordWithSink for a given body.
func recordBodyWithSink(k *Keeper, sink *reportSink, key Key, at time.Time, body string) {
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: key, StartedAt: at.Add(d), Body: []byte(body), Provider: "anthropic",
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: kaPolicy(), Header: kaHeader(), Emitter: sink, Hold: fakeOf(k).hold(key)})
	}
}

func manualKeeper(t *testing.T) (*Keeper, *reportSink, *testClock) {
	t.Helper()
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	sink := &reportSink{}
	recordWithSink(k, sink, sess1, clock.now())
	return k, sink, clock
}

// One manual ping is ONE request of the ordinary ping shape: the recorded body with max_tokens 1
// and stream false, so it reads the same cache entry the automatic ping would.
func TestPingNowSendsOneOrdinaryPingAndReportsItAsManual(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	fd := fakeOf(k)

	res, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second, RewriteUSD: 0.18})
	if err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	if fd.n() != 1 {
		t.Fatalf("sent %d requests, want exactly 1", fd.n())
	}
	sent := fd.sent[0]
	if gjson.GetBytes(sent.Body, "max_tokens").Int() != 1 || gjson.GetBytes(sent.Body, "stream").Bool() {
		t.Errorf("not an ordinary ping body: %s", sent.Body)
	}
	if res.Status != http.StatusOK || res.Usage.CacheRead != 48576 || !res.StartedAt.Equal(clock.now()) {
		t.Errorf("result = %+v", res)
	}
	if res.CostUSD <= 0 || !res.Priced {
		t.Errorf("a priced ping must report its cost: %+v", res)
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	r := reps[0]
	if !r.Manual || r.Strategy != "manual" || r.RewriteUSD != 0.18 || r.CostUSD != res.CostUSD {
		t.Errorf("report = %+v, want Manual, strategy \"manual\", the avoided rewrite and the ping's cost", r)
	}
}

// A manual ping is not one of the K automatic pings: it must neither spend them nor leave the
// entry due sooner. It DOES refresh the provider's entry, so the automatic clock restarts at it.
func TestPingNowDoesNotSpendAutomaticPingsAndRestartsTheClock(t *testing.T) {
	k, _, clock := manualKeeper(t)
	fd := fakeOf(k)
	clock.advance(100 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if e, _ := k.Entry(sess1); e.Pings != 0 {
		t.Errorf("entry pings = %d, a manual ping must not count against K", e.Pings)
	}
	// 280 s after the RECORD would be due; the manual ping at +100 s moved it to +380 s.
	if n := k.Sweep(clock.advance(181 * time.Second)); n != 0 {
		t.Fatalf("automatic ping fired %d times only 181 s after a manual one", n)
	}
	if n := k.Sweep(clock.advance(100 * time.Second)); n != 1 {
		t.Fatalf("automatic ping did not fire 281 s after the manual one (%d)", n)
	}
	if fd.n() != 2 {
		t.Errorf("sent %d, want manual + one automatic", fd.n())
	}
}

func TestPingNowRefusesWhatTheKeeperDoesNotHold(t *testing.T) {
	k, _, _ := manualKeeper(t)
	if _, err := k.PingNow(ManualRequest{Key: Key{Tenant: "t1", Session: "other"}}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("an unknown session = %v, want ErrNotHeld", err)
	}
	// Another tenant's key addresses nothing: the key carries the tenant.
	if _, err := k.PingNow(ManualRequest{Key: Key{Tenant: "t2", Session: sess1.Session}}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("another tenant's session = %v, want ErrNotHeld", err)
	}
	if fakeOf(k).n() != 0 {
		t.Error("a refused manual ping sent a request")
	}
}

// A refused manual ping is reported, but one button press must not switch off the thread's whole
// automatic schedule: stopping is the scheduled path's call.
func TestPingNowRefusalDoesNotStopTheAutomaticSchedule(t *testing.T) {
	k, _, clock := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil })
	var up *UpstreamError
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.As(err, &up) || up.Status != 400 {
		t.Fatalf("PingNow = %v, want *UpstreamError 400", err)
	}
	if e, _ := k.Entry(sess1); e.Stopped {
		t.Error("a manual 400 stopped the thread's automatic pings")
	}
	fakeOf(k).setResp(okPing)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Errorf("the automatic ping did not fire after a refused manual one (%d)", n)
	}
}

// The scheduled path refuses a ping dearer than the policy's per-ping ceiling; a press is held to
// the same cap, so a huge prefix cannot be pinged 16 threads x every 30 s past it.
func TestPingNowHonoursThePerPingCeiling(t *testing.T) {
	k, _, _ := manualKeeper(t)
	k.mu.Lock()
	for _, e := range k.live {
		e.pingUSD = e.pol.Ceiling() + 1
	}
	k.mu.Unlock()
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrTooCostly) {
		t.Errorf("PingNow = %v, want ErrTooCostly", err)
	}
	if fakeOf(k).n() != 0 {
		t.Error("a ping over the ceiling was sent")
	}
	if !k.PingStates("t1", false, sess1.Session).For("").Pingable {
		t.Error("too costly is not the same as not held")
	}
}

// After a timeout the request may still have been billed, so the gap stays claimed.
func TestPingNowKeepsTheGapAfterATimeout(t *testing.T) {
	k, _, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, 0, context.DeadlineExceeded })
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err == nil {
		t.Fatal("want an error")
	}
	var soon *TooSoonError
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); !errors.As(err, &soon) {
		t.Errorf("after a timeout the retry = %v, want *TooSoonError", err)
	}
}

func TestPingNowIsRateLimitedPerThread(t *testing.T) {
	k, _, clock := manualKeeper(t)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Second)
	_, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second})
	var soon *TooSoonError
	if !errors.As(err, &soon) {
		t.Fatalf("second ping after 10 s = %v, want *TooSoonError", err)
	}
	if soon.RetryAfter != 20*time.Second {
		t.Errorf("RetryAfter = %s, want 20s", soon.RetryAfter)
	}
	if fakeOf(k).n() != 1 {
		t.Error("a rate-limited ping reached the provider")
	}
	clock.advance(20 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Errorf("after the gap: %v", err)
	}
}

// A ping that read nothing paid for a write, so it avoided nothing: no rewrite credit.
func TestPingNowCreditsNothingWhenThePingMissed(t *testing.T) {
	k, sink, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheWrite: 48576, Output: 1}, http.StatusOK, nil
	})
	res, err := k.PingNow(ManualRequest{Key: sess1, RewriteUSD: 0.18})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.CacheRead != 0 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if r := sink.all()[0]; r.RewriteUSD != 0 {
		t.Errorf("a ping that wrote credited a rewrite of %v", r.RewriteUSD)
	}
}

// A manual ping that read nothing but wrote re-created the entry (#580): the refresh clock moves to
// it, the automatic schedule does not restart, and there is still no rewrite credit.
func TestPingNowMovesTheRefreshClockWhenThePingReWrote(t *testing.T) {
	k, _, clock := manualKeeper(t)
	k.mu.Lock()
	before := k.live[sess1.id()].refreshedAt
	k.mu.Unlock()
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheWrite: 48576, Output: 1}, http.StatusOK, nil
	})
	at := clock.advance(20 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, RewriteUSD: 0.18}); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	e := k.live[sess1.id()]
	got, started := e.refreshedAt, e.startedAt
	k.mu.Unlock()
	if !got.Equal(at) || got.Equal(before) {
		t.Errorf("refreshedAt = %v after a manual ping that re-wrote, want its start %v (was %v)", got, at, before)
	}
	if started.Equal(at) {
		t.Errorf("a manual ping that only wrote restarted the automatic schedule at %v", started)
	}
}

// The upstream failing is an error to the caller, and the audit row still records the attempt's status.
func TestPingNowReportsAnUpstreamTransportFailure(t *testing.T) {
	k, sink, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, 0, errors.New("dial") })
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err == nil {
		t.Fatal("want an error")
	}
	if len(sink.all()) != 0 {
		t.Error("a ping that never got an answer wrote a row")
	}
}

// Pingable is exactly PingNow's ErrNotHeld condition, so a button gated on it never meets a 409.
func TestPingStatesAgreeWithPingNowNotHeld(t *testing.T) {
	k, _, _ := manualKeeper(t)
	if !k.PingStates("t1", false, sess1.Session).For("").Pingable {
		t.Fatal("a held thread must be pingable")
	}
	if got := k.PingStates("t2", false, sess1.Session); len(got) != 0 {
		t.Errorf("another tenant sees %v", got)
	}
	if got := k.PingStates("", true, sess1.Session); !got.For("").Pingable {
		t.Errorf("all-tenants scope = %v, want the held thread", got)
	}
	// A thread the keeper no longer holds is not pingable, and PingNow says ErrNotHeld for it.
	k.RetireSession("t1", sess1.Session)
	if k.PingStates("t1", false, sess1.Session).For("").Pingable {
		t.Error("a retired thread is reported pingable")
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("PingNow = %v, want ErrNotHeld", err)
	}
	// A thread the scheduled path stopped still holds its copy, so a press can work (#771).
	k2, _, _ := manualKeeper(t)
	k2.mu.Lock()
	for _, e := range k2.live {
		e.stopped = true
	}
	k2.mu.Unlock()
	if !k2.PingStates("t1", false, sess1.Session).For("").Pingable {
		t.Error("a stopped thread that still holds its copy is not reported pingable")
	}
	if _, err := k2.PingNow(ManualRequest{Key: sess1}); errors.Is(err, ErrNotHeld) {
		t.Errorf("PingNow on a stopped thread = %v, want a ping to be sent", err)
	}
}

// The Window of every ping of one idle gap is the start of the last REAL request, whichever path
// sent the ping and however often it has moved the entry's own clock.
func TestPingReportsCarryTheWindowTheyServed(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	gap := clock.now()
	clock.advance(100 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if n := k.Sweep(clock.advance(300 * time.Second)); n != 1 {
		t.Fatalf("scheduled ping fired %d times", n)
	}
	if n := k.Sweep(clock.advance(300 * time.Second)); n != 1 {
		t.Fatalf("second scheduled ping fired %d times", n)
	}
	reps := sink.all()
	if len(reps) != 3 {
		t.Fatalf("reports = %d, want 3", len(reps))
	}
	if gap.IsZero() {
		t.Fatal("test clock is zero")
	}
	for i, r := range reps {
		if !r.Window.Equal(gap) {
			t.Errorf("report %d (manual=%v) Window = %v, want the real request's start %v", i, r.Manual, r.Window, gap)
		}
	}
}

// A new real request opens a new gap: the next ping's Window is that request's start.
func TestPingWindowFollowsTheNewestRealRequest(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	first := clock.now()
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("fired %d", n)
	}
	second := clock.advance(10 * time.Second)
	recordWithSink(k, sink, sess1, second)
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("fired %d", n)
	}
	reps := sink.all()
	if len(reps) != 2 || !reps[0].Window.Equal(first) || !reps[1].Window.Equal(second) || reps[0].Window.Equal(reps[1].Window) {
		t.Fatalf("windows %+v", reps)
	}
}

// A manual-only hold never moves its clock on a press, and its Window stays the real request's start.
func TestManualHoldPressKeepsTheGapWindow(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	sink := &reportSink{}
	at := clock.now()
	pol := offPolicy
	pol.ManualHold = true
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: sess1, StartedAt: at.Add(d), Body: []byte(kaBody), Provider: "anthropic",
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: pol, Header: kaHeader(), Emitter: sink, Hold: fakeOf(k).hold(sess1)})
	}
	clock.advance(60 * time.Second)
	for i := 0; i < 2; i++ {
		if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: time.Second}); err != nil {
			t.Fatal(err)
		}
		clock.advance(60 * time.Second)
	}
	reps := sink.all()
	if len(reps) != 2 || !reps[0].Window.Equal(at) || !reps[1].Window.Equal(at) {
		t.Fatalf("windows %+v, want both %v", reps, at)
	}
}

// #771: a press on a STOPPED thread runs the checks the scheduled ping runs right before it sends. A tripped
// route or a dead credential refuses it and reports "unavailable"; once the mark clears the thread is
// pingable again while its copy is held. A thread stopped by a miss or a refused ping is not blocked.
func TestAPressOnAStoppedThreadChecksTheRouteAndTheCredential(t *testing.T) {
	stop := func(k *Keeper) *kaEntry {
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, e := range k.live {
			e.stopped, e.cred = true, "cred-x"
			return e
		}
		t.Fatal("no live entry")
		return nil
	}
	for _, c := range []struct {
		name  string
		mark  func(k *Keeper, e *kaEntry, now time.Time)
		clear func(k *Keeper)
	}{
		{"tripped route",
			func(k *Keeper, e *kaEntry, now time.Time) {
				k.routeTripped[routeKey(e.tenant, e.endpoint, e.route, e.model)] = now.Add(time.Hour)
			},
			func(k *Keeper) { k.routeTripped = map[string]time.Time{} }},
		{"dead credential",
			func(k *Keeper, e *kaEntry, now time.Time) { k.markCredDeadLocked(e.tenant, e.cred, e.model, 401, now) },
			func(k *Keeper) { k.credDead = map[string]time.Time{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _, clock := manualKeeper(t)
			e := stop(k)
			k.mu.Lock()
			c.mark(k, e, clock.now())
			k.mu.Unlock()
			if st := k.PingStates("t1", false, sess1.Session).For(""); st.Pingable || st.Reason != ReasonUnavailable {
				t.Errorf("state = %+v, want not pingable, %q", st, ReasonUnavailable)
			}
			if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
				t.Errorf("press = %v, want ErrNotHeld", err)
			}
			k.mu.Lock()
			c.clear(k)
			k.mu.Unlock()
			if st := k.PingStates("t1", false, sess1.Session).For(""); !st.Pingable {
				t.Errorf("after the mark cleared: %+v, want pingable", st)
			}
			if _, err := k.PingNow(ManualRequest{Key: sess1}); errors.Is(err, ErrNotHeld) {
				t.Errorf("after the mark cleared, press = %v, want a ping", err)
			}
		})
	}
	t.Run("miss-stopped stays pingable", func(t *testing.T) {
		k, _, _ := manualKeeper(t)
		stop(k)
		if st := k.PingStates("t1", false, sess1.Session).For(""); !st.Pingable {
			t.Errorf("state = %+v, want pingable", st)
		}
	})
}
