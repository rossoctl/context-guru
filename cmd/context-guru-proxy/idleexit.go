package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// Idle-exit: a proxy that a Claude Code session started should not outlive the machine's use
// of it.
//
// The funnel installs a SessionStart hook that starts the proxy on demand, so nothing has to
// be left running — but only if the process eventually goes away on its own. That is all this
// is: a clock, a probe, and the SAME graceful shutdown path SIGTERM takes. No new teardown
// logic, because the teardown is the part that is already right (armShutdown releases the
// dashboard's SSE connections, the deferred closes flush the capture batch).
//
// It is OFF unless asked for. A gateway deployment or an eval-containers run must never
// self-terminate, and "the proxy vanished overnight" is a much worse failure there than a
// process left running on a laptop.
//
// Two things make this less trivial than a timeout, and both are load-bearing:
//
//  1. **The keep-alive inverts "idle".** Pinging is what the proxy does WHILE no client
//     traffic arrives, so a watchdog that watches requests alone kills the feature in
//     precisely its working window. Hence the pending probe below, which both blocks exit and
//     resets the clock.
//  2. **Exit wipes the in-memory store.** A threshold shorter than the store's entry lifetime
//     drops live frozen decisions and re-bills their prefix at cache-creation prices. That is
//     refused at startup, not documented — see store.ValidateIdleExit.

// activityClock is the last moment this process did something a user would call "in use".
// Nanoseconds in an atomic so the request path pays one store and no lock.
type activityClock struct{ ns atomic.Int64 }

func (a *activityClock) touch(now time.Time) { a.ns.Store(now.UnixNano()) }
func (a *activityClock) last() time.Time     { return time.Unix(0, a.ns.Load()) }

// probeRoutes are the paths that do NOT count as use.
//
// They are what a machine asks, not what a person or an agent does: a Kubernetes liveness
// probe, a Prometheus scrape, a `curl /healthz` in a monitoring loop, and the session hook's
// own start-up check. Counting them was a bug that disabled the whole feature rather than
// weakening it — measured: a proxy with a 1h threshold logged
// `idle-exit armed after=1h0m0s`, then reported `idle for 1h3m0s` after 2h03m of wall clock,
// because a /healthz poller had been stamping the clock for the first hour. Any probe on a
// schedule shorter than the threshold means the exit NEVER fires, and logs nothing to say so.
//
// Everything else still counts, including the dashboard's own polling: a person with the
// dashboard open is using this process, and exiting under them is a worse failure than a
// process left running. That is a deliberate asymmetry — a probe is not a viewer.
var probeRoutes = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// stampActivity records a request as activity, unless its route is a machine probe.
//
// The stamp happens BEFORE the handler runs, so a long streaming response cannot age out while
// it is still being served — its own duration is not idleness. (The stream also cannot be cut
// off mid-flight regardless: srv.Shutdown waits for in-flight requests, which is why this
// reuses that path rather than calling os.Exit.)
func stampActivity(next http.Handler, act *activityClock, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !probeRoutes[r.URL.Path] {
			act.touch(now())
		}
		next.ServeHTTP(w, r)
	})
}

// idleExitOptions is everything watchIdle needs, with the clock and the ticker injected so
// the policy is testable without waiting out a real threshold.
type idleExitOptions struct {
	// threshold is how long the proxy must be unused before it exits.
	threshold time.Duration
	// act is stamped by stampActivity on every request.
	act *activityClock
	// pending reports work that must keep the process alive even with no requests: keep-alive
	// sessions with a ping still ahead of them. nil means "nothing pending, ever".
	pending func() int
	now     func() time.Time
	// tick drives the check. Production uses a ticker at a fraction of the threshold; the
	// resolution only bounds how late the exit is, never how early.
	tick <-chan time.Time
	// stop abandons the watch (the process is shutting down for another reason).
	stop <-chan struct{}
}

// watchIdle blocks until the proxy has been idle for the whole threshold, and returns a
// human-readable reason for the log. ok is false when the watch was abandoned via stop.
//
// Pending keep-alive work does not merely veto the exit, it RESETS the clock. Vetoing alone
// would exit the instant the last ping retired, taking the store with it at the moment a
// session is most likely to come back — the quiet gap after `end_turn` is where the pings
// were aimed in the first place. Treating a pending ping as activity gives the session a full
// threshold of grace after its last one.
func watchIdle(o idleExitOptions) (string, bool) {
	if o.now == nil {
		o.now = time.Now
	}
	// A BACKSTOP, not the real seed. The caller stamps the clock at launch (main), which is
	// the only place that knows when "launch" was; seeding here would date the clock from
	// whenever this goroutine happened to get scheduled, which is both later and unknowable.
	// It stays because the failure mode of an unstamped clock is the worst one available — a
	// zero clock reads as "idle since 1970" and exits on the first tick.
	if o.act.last().UnixNano() == 0 {
		o.act.touch(o.now())
	}
	for {
		select {
		case <-o.stop:
			return "", false
		case <-o.tick:
			now := o.now()
			if o.pending != nil {
				if n := o.pending(); n > 0 {
					o.act.touch(now)
					continue
				}
			}
			if idle := now.Sub(o.act.last()); idle >= o.threshold {
				return "idle for " + idle.Round(time.Second).String() +
					" (--idle-exit " + o.threshold.String() + ")", true
			}
		}
	}
}

// idleCheckInterval is how often the watchdog looks. A twentieth of the threshold keeps the
// exit within 5% of what was asked for, clamped so a 24h default does not mean an hour of
// slack and a 1h floor does not mean a check every three minutes.
func idleCheckInterval(threshold time.Duration) time.Duration {
	d := threshold / 20
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// checkIdleExit is every reason a requested idle-exit threshold must not start.
//
// A function rather than two inline `if`s in main so both refusals are testable: they are
// startup-fatal, which is the one class of check where "it looked right" is the only evidence
// anyone ever gathers.
func checkIdleExit(d time.Duration, upstreamsPath string, o store.Options) error {
	// The floor. Exiting clears the in-memory store, and losing a live frozen decision re-bills
	// its whole prefix as cache creation — the 11.5x regression FrozenLost exists to catch.
	if err := store.ValidateIdleExit(d, o); err != nil {
		return err
	}
	if d > 0 && upstreamsPath != "" {
		// A self-terminating GATEWAY is a different kind of wrong: --upstreams means this
		// process serves other people's agents, where "the proxy vanished overnight" is far
		// worse than a process left running on a laptop.
		//
		// The safety used to be accidental — it held only because a hosted deployment also runs
		// a liveness probe, and every probe stamped the activity clock. That is no longer true
		// (probeRoutes above deliberately excludes them), so what was accidentally safe is now
		// explicitly refused rather than quietly reintroduced.
		return fmt.Errorf("--idle-exit cannot be combined with --upstreams: a gateway serving " +
			"other people's agents must not self-terminate. Drop --idle-exit, or run this " +
			"instance without --upstreams")
	}
	return nil
}
