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

// TestIdleCheckIntervalStaysUseful pins one rule and one cap: the watchdog looks every
// threshold/20 — 5% of what was asked for — until that would exceed five minutes.
//
// The third case here used to be labelled "clamped low" and asserted 10m -> 30s, which is exactly
// 10m/20: it passed whether or not the clamp existed, and the clamp it claimed to cover could
// never fire anyway, because checkIdleExit refuses any threshold under an hour. The clamp is gone
// and so is the case that pretended to test it.
func TestIdleCheckIntervalStaysUseful(t *testing.T) {
	for _, c := range []struct {
		threshold, want time.Duration
		why             string
	}{
		{24 * time.Hour, 5 * time.Minute, "capped: 24h/20 is 72m, which would be an hour of slack"},
		{100 * time.Hour, 5 * time.Minute, "capped, well past the cap"},
		{time.Hour, 3 * time.Minute, "the floor: 5% of an hour, and the finest resolution reachable"},
		{80 * time.Minute, 4 * time.Minute, "threshold/20 while under the cap"},
		// Below the floor checkIdleExit refuses to start, so nothing here can be reached in
		// production. Asserted anyway so the function stays total rather than surprising.
		{10 * time.Minute, 30 * time.Second, "unreachable in production (below the floor)"},
	} {
		if got := idleCheckInterval(c.threshold); got != c.want {
			t.Errorf("idleCheckInterval(%s) = %s, want %s — %s", c.threshold, got, c.want, c.why)
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

// TestCheckIdleExitSkipsTheFloorWithNoStore: the floor protects the in-memory store, so with the
// store explicitly OFF there is nothing for it to protect.
//
// `--store=false` resolves to store.Nop, which persists nothing and holds no frozen decisions.
// Refusing `--store=false --idle-exit=30m` cited a consequence — "exiting drops live frozen
// decisions and re-bills their prefix" — that cannot occur in that configuration. A store-less
// proxy may exit whenever it likes.
func TestCheckIdleExitSkipsTheFloorWithNoStore(t *testing.T) {
	off, on := false, true
	short := 30 * time.Minute // far below the ~5h34m floor

	if err := checkIdleExit(short, "", store.Options{Enabled: &off}); err != nil {
		t.Errorf("refused a short threshold with the store disabled, citing a store that does not "+
			"exist: %v", err)
	}
	// Explicitly ON, and nil (= not configured, which means on) must both still be protected.
	if err := checkIdleExit(short, "", store.Options{Enabled: &on}); err == nil {
		t.Error("accepted a threshold below the floor with the store explicitly enabled")
	}
	if err := checkIdleExit(short, "", store.Options{}); err == nil {
		t.Error("accepted a threshold below the floor with the store unconfigured (which is on)")
	}
	// The gateway refusal is independent of the store: a self-terminating gateway is wrong
	// whether or not it keeps state.
	if err := checkIdleExit(24*time.Hour, "/etc/context-guru/upstreams.yaml",
		store.Options{Enabled: &off}); err == nil {
		t.Error("a gateway with the store off may still not self-terminate")
	}
}

// TestActivityClockKeepsItsMonotonicReading is the guard for a defect that no other test here can
// see, because they all inject a fake clock built from time.Unix — which has no monotonic reading
// to lose.
//
// The clock stored `now.UnixNano()` and rebuilt the instant with `time.Unix(0, ns)`. That value
// carries no monotonic reading, so `now.Sub(act.last())` was wall-clock arithmetic: a laptop
// suspend/resume or an NTP step counts as idleness, and the watchdog can fire on its first tick
// after a lid-open, racing the user's first request. On the laptop this feature exists for,
// suspend is the normal case rather than an edge one.
//
// `t.Round(0)` strips the monotonic reading, and time.Time's == compares wall, monotonic and
// location — so `stored.Round(0) != stored` is precisely "this value still has a monotonic
// reading".
func TestActivityClockKeepsItsMonotonicReading(t *testing.T) {
	var act activityClock
	act.touch(time.Now())

	stored := act.last()
	if stored.Round(0) == stored {
		t.Error("the stored instant has no monotonic reading, so idleness is measured against the " +
			"wall clock: a suspend/resume or an NTP step is counted as idle time")
	}
	// And the subtraction the watchdog actually performs must stay monotonic end to end.
	if elapsed := time.Now().Sub(act.last()); elapsed < 0 {
		t.Errorf("elapsed since the stamp is negative (%s), which wall-clock arithmetic permits "+
			"and a monotonic reading does not", elapsed)
	}
}

// TestStampActivityRefreshesOnCompletion: a request that takes a while must not leave the clock
// reading from the moment it STARTED.
//
// Stamping only on entry meant a long request looked like a gap in use the moment it finished. The
// residual — that the clock is not refreshed DURING a request, so a single request outliving the
// whole threshold with no other traffic can still age out — is documented on stampActivity rather
// than fixed, and is unreachable in practice because the dashboard UI polls every 30s.
func TestStampActivityRefreshesOnCompletion(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	var act activityClock
	h := stampActivity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The request takes 20 minutes of wall clock, as far as the injected clock is concerned.
		clk.advance(20 * time.Minute)
		w.WriteHeader(http.StatusOK)
	}), &act, clk.now)

	start := clk.now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/anthropic/v1/messages", nil))

	if got := act.last(); !got.After(start) {
		t.Errorf("clock reads %s, the moment the request STARTED (%s) — a long request then looks "+
			"like 20 minutes of idleness the instant it completes", got, start)
	}
	if want := start.Add(20 * time.Minute); !act.last().Equal(want) {
		t.Errorf("clock = %s, want the completion time %s", act.last(), want)
	}
	// A probe must still be stamped on neither edge.
	before := act.last()
	clk.advance(time.Hour)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if !act.last().Equal(before) {
		t.Errorf("a /healthz probe moved the clock to %s; probes count as neither entry nor "+
			"completion activity", act.last())
	}
}

// TestParseEnvDurationRefusesAUnitlessValue: `IDLE_EXIT=86400` is the natural mistake for something
// documented as a duration, and it used to mean "never exit" — silently, because the
// `idle-exit armed` line is only logged for a value above zero, so the evidence was the ABSENCE of
// a log line.
func TestParseEnvDurationRefusesAUnitlessValue(t *testing.T) {
	const def = 7 * time.Hour
	for _, c := range []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", def, false},    // not set
		{"   ", def, false}, // whitespace only
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1500ms", 1500 * time.Millisecond, false},
		{"86400", 0, true}, // seconds, unitless — the reported mistake
		{"24", 0, true},    // hours, unitless
		{"forever", 0, true},
	} {
		got, err := parseEnvDuration(c.raw, def)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseEnvDuration(%q) returned %s and no error; a typo must not silently "+
					"become a different configuration", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEnvDuration(%q): unexpected error %v", c.raw, err)
		} else if got != c.want {
			t.Errorf("parseEnvDuration(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
}
