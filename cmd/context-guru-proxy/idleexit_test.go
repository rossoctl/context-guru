package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// The shipped idle-exit default is 24h, so these tests drive a hand-advanced clock and a
// hand-fed ticker instead of waiting. watchIdle reads the time from o.now() and treats a tick
// purely as "look now", so the value carried on the channel is irrelevant and a tick that
// arrives late still evaluates against the current fake clock.

type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.ns.Store(t.UnixNano())
	return c
}
func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// watcher drives one watchIdle and reports its verdict.
//
// Two things here are deliberate, and both were bugs first:
//
//   - **The tick channel is UNBUFFERED.** With a buffer, the first send succeeds against the
//     buffer whether or not the watcher goroutine has been scheduled at all — so a test could
//     advance its clock believing the watcher had already started, and then measure idleness
//     from the wrong instant. Unbuffered makes a send a rendezvous: it completes only once the
//     watcher has actually received it.
//   - **Every interaction selects on the result channel too.** The moment the watcher exits it
//     stops draining ticks, and an unconditional send then blocks until the test deadline —
//     a hang, which tells you nothing, rather than a failure.
type watcher struct {
	t    *testing.T
	tick chan time.Time
	res  chan string
	clk  *fakeClock
}

func start(t *testing.T, clk *fakeClock, o idleExitOptions) *watcher {
	return startWith(t, clk, o, false)
}

// startWith exposes the one knob start hides: seedAtLaunch=true leaves the activity
// clock unstamped, so watchIdle's own backstop is what gets tested.
func startWith(t *testing.T, clk *fakeClock, o idleExitOptions, seedAtLaunch bool) *watcher {
	t.Helper()
	w := &watcher{t: t, tick: make(chan time.Time), res: make(chan string, 1), clk: clk}
	o.tick = w.tick
	o.now = clk.now
	// Stamp the clock the way main does at launch, unless the test is specifically exercising
	// the unstamped case.
	if !seedAtLaunch {
		o.act.touch(clk.now())
	}
	go func() {
		reason, ok := watchIdle(o)
		if !ok {
			reason = "" // abandoned via stop
		}
		w.res <- reason
	}()
	// Synchronise before returning, with a real tick rather than a sleep: on an unbuffered
	// channel a completed send proves the watcher is running and has reached its select, so a
	// clock the test advances afterwards cannot be mistaken for the launch time.
	//
	// It doubles as an assertion: at zero elapsed time nothing may exit.
	if verdict, done := w.poke(); done {
		t.Fatalf("watchIdle exited (%q) on its first look, with no time elapsed", verdict)
	}
	return w
}

// poke delivers one tick, or reports the verdict if the watcher has already finished.
func (w *watcher) poke() (verdict string, done bool) {
	w.t.Helper()
	select {
	case r := <-w.res:
		return r, true
	case w.tick <- w.clk.now():
		return "", false
	case <-time.After(3 * time.Second):
		w.t.Fatal("watchIdle is neither consuming ticks nor returning")
		return "", true
	}
}

// mustNotExit checks the watcher evaluated the current clock and stayed alive.
//
// It pokes TWICE on purpose: the tick channel holds one, so a second successful send proves
// the first was consumed and the loop came back for more, rather than merely sitting in the
// buffer unexamined. Without that, "no exit" could just mean "never looked".
func (w *watcher) mustNotExit(what string) {
	w.t.Helper()
	for i := 0; i < 2; i++ {
		if verdict, done := w.poke(); done {
			w.t.Fatalf("%s: watchIdle exited (%q) when it must not", what, verdict)
		}
	}
}

// mustExit gives the watcher a bounded number of looks to decide it is idle.
func (w *watcher) mustExit(what string) string {
	w.t.Helper()
	for i := 0; i < 4; i++ {
		if verdict, done := w.poke(); done {
			if verdict == "" {
				w.t.Fatalf("%s: the watch was abandoned instead of exiting", what)
			}
			return verdict
		}
	}
	w.t.Fatalf("%s: idle past the threshold, but watchIdle never exited", what)
	return ""
}

// TestIdleExitFiresWhenNothingIsHappening is the base case the feature exists for: a proxy a
// session started, and then nobody used, goes away by itself instead of being left on the
// evaluator's machine.
func TestIdleExitFiresWhenNothingIsHappening(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	w := start(t, clk, idleExitOptions{threshold: time.Hour, act: &activityClock{},
		pending: func() int { return 0 }, stop: make(chan struct{})})

	clk.advance(30 * time.Minute)
	w.mustNotExit("half a threshold")

	clk.advance(31 * time.Minute)
	t.Logf("exit reason: %s", w.mustExit("past the threshold"))
}

// TestIdleExitWaitsForAPendingKeepAlivePing is the case a naive watchdog gets wrong.
//
// The keep-alive INVERTS the meaning of idle: pinging is what the proxy does precisely while
// no client traffic is arriving — the quiet gap after `end_turn`, where 83.7% of the
// recoverable dollars sit. A watchdog counting requests alone would kill the process in
// exactly the window the feature was built for.
//
// Two properties, the second subtler than the first:
//
//  1. a pending ping VETOES the exit, however long the client silence;
//  2. it also RESETS the clock, so retiring the last ping does not exit moments later — it
//     buys a full fresh threshold. Veto-only would drop the in-memory store at the instant
//     the session is most likely to come back, which is the cache-write regression the floor
//     and this whole feature are meant to avoid.
func TestIdleExitWaitsForAPendingKeepAlivePing(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	var pending atomic.Int64
	pending.Store(1)
	w := start(t, clk, idleExitOptions{threshold: time.Hour, act: &activityClock{},
		pending: func() int { return int(pending.Load()) }, stop: make(chan struct{})})

	// (1) Veto: two full thresholds of silence with a ping still scheduled.
	clk.advance(2 * time.Hour)
	w.mustNotExit("a keep-alive ping is still scheduled")

	// (2) The ping retires. If the veto reset the clock, a threshold measured from the START
	// is not enough — only 30m have passed since the last pending observation.
	pending.Store(0)
	clk.advance(30 * time.Minute)
	w.mustNotExit("30m after the last ping retired")

	clk.advance(31 * time.Minute)
	w.mustExit("genuinely idle for a whole threshold")
}

// TestRequestsDeferIdleExit covers the stamping half: a real request is use, and use defers the
// exit.
func TestRequestsDeferIdleExit(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	act := &activityClock{}
	stampedBeforeHandler := false
	h := stampActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The stamp must land BEFORE the handler runs, so a long streaming response cannot
		// age out while it is still being served.
		stampedBeforeHandler = act.last().Equal(clk.now())
		w.WriteHeader(http.StatusOK)
	}), act, clk.now)

	w := start(t, clk, idleExitOptions{threshold: time.Hour, act: act,
		pending: func() int { return 0 }, stop: make(chan struct{})})

	clk.advance(50 * time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/anthropic/v1/messages", nil))
	if !stampedBeforeHandler {
		t.Fatal("stampActivity did not record the request before invoking the handler")
	}
	// 80m since launch, but only 30m since the request.
	clk.advance(30 * time.Minute)
	w.mustNotExit("30m after serving a request")

	clk.advance(31 * time.Minute)
	w.mustExit("an hour after the last request")
}

// TestIdleExitStopAbandonsTheWatch: when the process is already shutting down for another
// reason (SIGTERM), the watchdog must let go rather than hold a goroutine and push a second
// reason into the shutdown path.
func TestIdleExitStopAbandonsTheWatch(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	stop := make(chan struct{})
	w := start(t, clk, idleExitOptions{threshold: time.Hour, act: &activityClock{},
		pending: func() int { return 0 }, stop: stop})
	close(stop)
	select {
	case r := <-w.res:
		if r != "" {
			t.Fatalf("stop should abandon the watch, got exit reason %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchIdle ignored stop")
	}
}

// TestIdleExitStartsItsClockAtLaunch: a proxy that never serves a single request still has to
// exit. Nothing stamps the activity clock in that case, so watchIdle has to seed it itself —
// a zero clock would otherwise read as "idle since 1970" and exit on the first tick, which is
// the opposite failure and just as wrong.
func TestIdleExitStartsItsClockAtLaunch(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	w := startWith(t, clk, idleExitOptions{threshold: time.Hour, act: &activityClock{},
		pending: func() int { return 0 }, stop: make(chan struct{})}, true)
	w.mustNotExit("first tick on a proxy that has served nothing")
	clk.advance(61 * time.Minute)
	w.mustExit("an hour after launch with no traffic at all")
}

// TestIdleCheckIntervalStaysUseful pins the resolution at both ends: a 24h default must not
// mean an hour of slack past the threshold, and the 1h floor must not mean a check every few
// minutes for nothing.
func TestIdleCheckIntervalStaysUseful(t *testing.T) {
	for _, c := range []struct{ threshold, want time.Duration }{
		{24 * time.Hour, 5 * time.Minute},    // clamped high
		{time.Hour, 3 * time.Minute},         // threshold/20
		{10 * time.Minute, 30 * time.Second}, // clamped low
	} {
		if got := idleCheckInterval(c.threshold); got != c.want {
			t.Errorf("idleCheckInterval(%s) = %s, want %s", c.threshold, got, c.want)
		}
	}
}

// TestProbesDoNotDeferIdleExit is the other half, and it is the one that was a live bug.
//
// A liveness probe or a Prometheus scrape is a machine asking whether the process is up — not
// somebody using it. Counting those did not weaken --idle-exit, it DISABLED it: any probe on a
// schedule shorter than the threshold means the exit never fires, and the only log line is the
// `idle-exit armed` one at startup, so nothing says it silently stopped working. Measured on a
// 1h-threshold proxy: 2h03m of wall clock, then `idle for 1h3m0s`, the clock having been held
// up for an hour by a /healthz poller alone.
func TestProbesDoNotDeferIdleExit(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	act := &activityClock{}
	h := stampActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), act, clk.now)

	w := start(t, clk, idleExitOptions{threshold: time.Hour, act: act,
		pending: func() int { return 0 }, stop: make(chan struct{})})

	// A probe every 10 minutes for two hours — the shape of a real monitoring loop.
	for i := 0; i < 12; i++ {
		clk.advance(10 * time.Minute)
		for _, path := range []string{"/healthz", "/metrics"} {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		}
	}
	if got := w.mustExit("two hours of nothing but liveness probes"); got == "" {
		t.Fatal("no exit reason")
	}

	// And the asymmetry is deliberate, so pin it: a dashboard poll IS use. Exiting under
	// somebody who is watching is a worse failure than a process left running.
	clk2 := newFakeClock(time.Unix(1_700_000_000, 0))
	act2 := &activityClock{}
	h2 := stampActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), act2, clk2.now)
	w2 := start(t, clk2, idleExitOptions{threshold: time.Hour, act: act2,
		pending: func() int { return 0 }, stop: make(chan struct{})})
	clk2.advance(50 * time.Minute)
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/events", nil))
	clk2.advance(30 * time.Minute)
	w2.mustNotExit("a dashboard tab is open and polling")
}

// TestCheckIdleExitRefusesAGatewaySelfTerminating covers the second startup refusal.
//
// `--upstreams` means this process serves other people's agents. A proxy that vanishes overnight
// there is a far worse failure than one left running on a laptop — and the protection used to be
// accidental: it held only because a hosted deployment runs a liveness probe, and every probe
// stamped the activity clock. probeRoutes deliberately stopped counting probes, which removes
// that accident, so the refusal has to be explicit or the combination silently becomes live.
func TestCheckIdleExitRefusesAGatewaySelfTerminating(t *testing.T) {
	ok := store.Options{}  // default TTL => floor 5h33m20s
	good := 24 * time.Hour // clears the floor
	for _, c := range []struct {
		name      string
		d         time.Duration
		upstreams string
		wantErr   string
	}{
		{"laptop install: no upstreams", good, "", ""},
		{"gateway with idle-exit", good, "/etc/context-guru/upstreams.yaml", "--upstreams"},
		// Off is always fine, including on a gateway: that is the shipped default and the
		// refusal must not fire on a configuration everybody runs.
		{"gateway without idle-exit", 0, "/etc/context-guru/upstreams.yaml", ""},
		// The floor still applies, and it is reported first — a threshold that is BOTH too short
		// and on a gateway should name the floor, since that is the value the operator typed.
		{"below the floor", 30 * time.Minute, "", "floor"},
		{"below the floor on a gateway", 30 * time.Minute, "/etc/x.yaml", "floor"},
	} {
		err := checkIdleExit(c.d, c.upstreams, ok)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: refused a valid configuration: %v", c.name, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: accepted a configuration that must not start", c.name)
		case c.wantErr != "" && err != nil && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: message does not mention %q: %v", c.name, c.wantErr, err)
		}
	}
}
