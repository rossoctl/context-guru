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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		// The stamp must land BEFORE the handler runs, so a burst of requests keeps the clock warm
		// without waiting for each to finish.
		stampedBeforeHandler = act.last().Equal(clk.now())
		w.WriteHeader(http.StatusOK)
	})
	h := stampActivity(mux, act, clk.now)

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
	h := stampActivity(probeMux(), act, clk.now)

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
	h2 := stampActivity(probeMux(), act2, clk2.now)
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
		err := checkIdleExit(c.d, c.upstreams, "", ok)
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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		// The request takes 20 minutes of wall clock, as far as the injected clock is concerned.
		clk.advance(20 * time.Minute)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	h := stampActivity(mux, &act, clk.now)

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

// probeMux is the route shape stampActivity is asked about: the two machine probes, one real API
// route, and the dashboard's SSE endpoint. Registered with methods, as the proxy registers them.
func probeMux() *http.ServeMux {
	m := http.NewServeMux()
	nop := func(w http.ResponseWriter, r *http.Request) {}
	m.HandleFunc("GET /healthz", nop)
	m.HandleFunc("GET /metrics", nop)
	m.HandleFunc("GET /api/events", nop)
	m.HandleFunc("POST /anthropic/v1/messages", nop)
	return m
}

// TestProbeExemptionSurvivesATrailingSlash is the hole an exact path compare left open, and it is
// the one that matters most because it fails SILENTLY in the safe-looking direction.
//
// A probe configured with a trailing slash — `GET /healthz/` — used to count as activity, so it
// refreshed the clock forever: --idle-exit never fired, and the only log line was `idle-exit armed`
// at startup.
//
// Be exact about WHY it is exempt now, because the first version of this comment was not:
// http.ServeMux does NOT redirect `/healthz/` to `/healthz`. cleanPath re-appends the trailing slash
// and matchOrRedirect only ever ADDS one, so with this route table `/healthz/` is a plain 404 and
// `Handler` reports the EMPTY pattern. That is what exempts it — the same branch that exempts
// `/nope`. (The redirect Go does generate, for a subtree root, also reports an empty pattern, so
// nothing here relies on a redirect resolving to its post-redirect pattern.)
//
// So every row below that is not a real route is exempt for one reason: no pattern matched.
func TestProbeExemptionSurvivesATrailingSlash(t *testing.T) {
	for _, c := range []struct {
		method, path string
		isUse        bool
		why          string
	}{
		{"GET", "/healthz", false, "the probe itself"},
		{"GET", "/healthz/", false, "404, empty pattern — and a k8s probe spelled this way must not count"},
		{"GET", "/metrics", false, "a Prometheus scrape"},
		{"GET", "/metrics/", false, "404, empty pattern"},
		{"GET", "//healthz", false, "cleanPath collapses this to /healthz, which IS the probe pattern"},
		{"GET", "/health", false, "404 — a stray probe is not use"},
		{"GET", "/nope", false, "404 — a port scanner is not use"},
		{"GET", "/api/events", true, "the dashboard's SSE stream: a person is watching"},
		{"POST", "/anthropic/v1/messages", true, "an actual agent request"},
	} {
		clk := newFakeClock(time.Unix(1_700_000_000, 0))
		var act activityClock
		h := stampActivity(probeMux(), &act, clk.now)

		clk.advance(time.Minute)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))

		stamped := !act.last().IsZero()
		if stamped != c.isUse {
			verb := "did not count"
			if stamped {
				verb = "counted"
			}
			t.Errorf("%s %s %s as activity, want the opposite — %s", c.method, c.path, verb, c.why)
		}
	}
}

// TestCheckIdleExitKeepsTheOneHourMinimumWithNoStore: skipping the floor when the store is off
// removed it ENTIRELY, and a startup refusal became a startup PANIC.
//
// `STORE=false --idle-exit=10ns` passed the check, and threshold/20 then reached time.NewTicker as
// 0, which panics. Only the `2 x ttl_seconds` term is about the store; the bare 1h term is not, and
// dropping it also broke the invariant README and docs/reference/config.md state unconditionally.
func TestCheckIdleExitKeepsTheOneHourMinimumWithNoStore(t *testing.T) {
	off := false
	noStore := store.Options{Enabled: &off}

	// The 2x TTL term does not apply: ~5h34m would otherwise be the floor.
	if err := checkIdleExit(2*time.Hour, "", "", noStore); err != nil {
		t.Errorf("2h refused with the store disabled, where only the 1h minimum applies: %v", err)
	}
	// The 1h term still does.
	for _, d := range []time.Duration{10 * time.Nanosecond, time.Millisecond, 30 * time.Minute,
		time.Hour - time.Nanosecond} {
		if err := checkIdleExit(d, "", "", noStore); err == nil {
			t.Errorf("accepted --idle-exit=%s with the store disabled; anything under an hour is "+
				"shorter than the keep-alive's ping window, and a sub-second value cannot be "+
				"scheduled at all", d)
		}
	}
	// Off is still always valid, and a gateway is still refused.
	if err := checkIdleExit(0, "", "", noStore); err != nil {
		t.Errorf("off refused: %v", err)
	}
	if err := checkIdleExit(24*time.Hour, "/etc/x.yaml", "", noStore); err == nil {
		t.Error("a gateway with the store off may still not self-terminate")
	}

	// And the store-off path must not become an escape hatch from the FULL floor: a store that is
	// explicitly on, or simply unconfigured (which means on), is still protected by 2x the TTL.
	on := true
	if err := checkIdleExit(30*time.Minute, "", "", store.Options{Enabled: &on}); err == nil {
		t.Error("accepted a threshold below the floor with the store explicitly enabled")
	}
	if err := checkIdleExit(30*time.Minute, "", "", store.Options{}); err == nil {
		t.Error("accepted a threshold below the floor with the store unconfigured (which is on)")
	}
	// 2h clears the 1h minimum but not 2x the default TTL (~5h34m), so it must be refused when the
	// store is on and accepted when it is off — that difference IS the store-off exemption.
	if err := checkIdleExit(2*time.Hour, "", "", store.Options{}); err == nil {
		t.Error("accepted 2h with the store on, where the floor is ~5h34m")
	}
}

// TestIdleCheckIntervalIsAlwaysPositive: time.NewTicker panics on a non-positive duration, and
// integer division makes zero reachable for a small enough threshold. checkIdleExit refuses those,
// so this cannot fire in production — but the previous version of the function reasoned exactly that
// way and was then reached with 10ns through a path that skipped the floor. A crash is the wrong
// failure mode for a helper, whatever its caller did.
func TestIdleCheckIntervalIsAlwaysPositive(t *testing.T) {
	for _, d := range []time.Duration{0, 1, 5, 19 * time.Nanosecond, time.Nanosecond, time.Second} {
		if got := idleCheckInterval(d); got <= 0 {
			t.Errorf("idleCheckInterval(%s) = %s; time.NewTicker would panic", d, got)
		}
	}
}

// TestCheckIdleExitRefusesBobModeToo closes the hole the third review found: --bob-upstream is a
// gateway flag too, and it was not covered.
//
// Two reasons it must be refused, and the second is the one that makes it urgent. First, Bob mode
// serves other people's agents, so self-terminating is as wrong there as with --upstreams. Second,
// proxy.Mux registers a `/` catch-all whenever BobUpstream is set — and with a catch-all present,
// mux.Handler answers "/" rather than the empty pattern for EVERY unmatched path, so `/healthz/`,
// `/nope` and every port-scan path count as activity and the watchdog never fires. Silently, with
// `idle-exit armed` as the only log line.
func TestCheckIdleExitRefusesBobModeToo(t *testing.T) {
	good := 24 * time.Hour
	for _, c := range []struct {
		name, upstreams, bob string
		wantRefused          bool
	}{
		{"laptop: neither", "", "", false},
		{"bob gateway", "", "https://api.us-east.bob.ibm.com", true},
		{"hosted gateway", "/etc/context-guru/upstreams.yaml", "", true},
		{"both", "/etc/context-guru/upstreams.yaml", "https://api.us-east.bob.ibm.com", true},
	} {
		err := checkIdleExit(good, c.upstreams, c.bob, store.Options{})
		if c.wantRefused && err == nil {
			t.Errorf("%s: accepted --idle-exit; a proxy with a `/` catch-all cannot also have a "+
				"watchdog, because every unmatched path would count as activity", c.name)
		}
		if !c.wantRefused && err != nil {
			t.Errorf("%s: refused a valid laptop configuration: %v", c.name, err)
		}
		// The message must name the flag the operator actually passed, since that is the one they
		// have to drop.
		if c.wantRefused && err != nil {
			want := "--upstreams"
			if c.upstreams == "" {
				want = "--bob-upstream"
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: message does not name %s: %v", c.name, want, err)
			}
		}
	}
	// Off is always fine, in any mode.
	if err := checkIdleExit(0, "", "https://api.us-east.bob.ibm.com", store.Options{}); err != nil {
		t.Errorf("off refused in Bob mode: %v", err)
	}
}

// TestCatchAllRouteIsNotActivity is the belt to that braces.
//
// checkIdleExit now refuses --idle-exit alongside the flags that mount a `/` catch-all, so this
// combination is unreachable in a shipped configuration — but the two rules live in different files,
// and this is the one whose failure is silent. Over-counting a catch-all as "not use" errs toward
// exiting a laptop proxy; under-counting errs toward a gateway that never exits, which is the
// failure nobody notices.
func TestCatchAllRouteIsNotActivity(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0))
	var act activityClock

	mux := http.NewServeMux()
	nop := func(w http.ResponseWriter, r *http.Request) {}
	mux.HandleFunc("GET /healthz", nop)
	mux.HandleFunc("POST /anthropic/v1/messages", nop)
	// Bob mode's catch-all, and the explicit Bob route beside it.
	mux.HandleFunc("POST /inference/v1/chat/completions", nop)
	mux.HandleFunc("/", nop)
	h := stampActivity(mux, &act, clk.now)

	for _, c := range []struct {
		method, path string
		isUse        bool
		why          string
	}{
		{"GET", "/healthz", false, "the probe itself"},
		{"GET", "/healthz/", false, "falls through to the catch-all, and must still not count"},
		{"GET", "/nope", false, "a port scanner matching `/` is not somebody using the proxy"},
		{"POST", "/anthropic/v1/messages", true, "a real agent request"},
		{"POST", "/inference/v1/chat/completions", true, "Bob's own model route is explicit"},
	} {
		act = activityClock{}
		clk.advance(time.Minute)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if stamped := !act.last().IsZero(); stamped != c.isUse {
			verb := "did not count"
			if stamped {
				verb = "counted"
			}
			t.Errorf("with a `/` catch-all registered, %s %s %s as activity, want the opposite — %s",
				c.method, c.path, verb, c.why)
		}
	}
}
