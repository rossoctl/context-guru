// Package testutil holds helpers for tests that wait for something asynchronous.
//
// It is for test files only. A production package must not import it. The test
// TestNoProductionPackageImportsTestutil enforces that.
//
// Why it exists: a fixed wait such as "poll for 2 s" fails on a slow CI runner (2 cores, -race).
// WaitFor uses a long safety bound instead. A passing test still ends as soon as the
// condition holds, so the long bound costs nothing.
package testutil

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// pollInterval is the pause between two checks of the condition.
const pollInterval = 2 * time.Millisecond

// defaultBound is the safety bound: the longest a test waits before it fails.
const defaultBound = 30 * time.Second

var bound atomic.Int64

func init() { bound.Store(int64(defaultBound)) }

// SetBoundForTest sets the safety bound and returns a func that restores the old one.
// Only the tests of this package need it. The bound is process-wide: no test in this package
// may call t.Parallel.
func SetBoundForTest(d time.Duration) (restore func()) {
	old := bound.Swap(int64(d))
	return func() { bound.Store(old) }
}

// WaitFor polls cond until it returns true. If it does not within the safety bound, WaitFor
// calls t.Fatalf with what and the elapsed time.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	WaitForState(t, what, cond, nil)
}

// WaitForState is WaitFor with a func that describes the state the test saw last. The failure
// message shows it, so a reader sees why the condition did not hold. state may be nil.
func WaitForState(t testing.TB, what string, cond func() bool, state func() string) {
	t.Helper()
	start := time.Now()
	limit, deadline, cut := effectiveBound(t)
	for {
		if cond() {
			return
		}
		if time.Since(start) >= limit {
			break
		}
		time.Sleep(pollInterval)
	}
	// One last check: the condition may have become true during the final sleep.
	if cond() {
		return
	}
	msg := fmt.Sprintf("timed out after %v waiting for %s", time.Since(start).Round(time.Millisecond), what)
	if state != nil {
		msg += "; state: " + describe(state)
	}
	if cut {
		msg += cutNote(limit, time.Duration(bound.Load()), time.Until(deadline))
	}
	t.Fatalf("%s", msg)
}

// cutNote explains a wait that the test binary's -timeout cut short. A wait of a few milliseconds
// otherwise reads like a flaky test (v0.7.4 Part A, race-record). It states facts only: the cut
// does not prove the condition would have held with more time. left is measured at the failure.
func cutNote(limit, full, left time.Duration) string {
	when := fmt.Sprintf("ends in %v", left.Round(time.Millisecond))
	if left <= 0 {
		when = "has already passed"
	}
	return fmt.Sprintf("; the test binary's -timeout %s, so this wait got only %v of its %v bound. "+
		"Re-run with a larger -timeout to tell a slow package from a condition that never holds",
		when, limit.Round(time.Millisecond), full)
}

// deadlineMargin is the time kept before the test deadline, so the failure message of WaitFor
// shows in place of a bare "test timed out".
const deadlineMargin = 5 * time.Second

// effectiveBound is the safety bound, cut to the time left before the test deadline minus
// deadlineMargin. It is never below one poll interval. cut says the deadline set the limit, and
// deadline is then the test binary's deadline.
func effectiveBound(t testing.TB) (limit time.Duration, deadline time.Time, cut bool) {
	limit = time.Duration(bound.Load())
	if d, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if dl, has := d.Deadline(); has {
			if left := time.Until(dl) - deadlineMargin; left < limit {
				return max(left, pollInterval), dl, true
			}
		}
	}
	return limit, time.Time{}, false
}

// describe calls state. A panic inside state must not hide the timeout, so it becomes text.
func describe(state func() string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("(state func panicked: %v)", r)
		}
	}()
	return state()
}

// timingEnv is the switch for the timing tests. The value "1" turns them on.
const timingEnv = "CG_TIMING_TESTS"

// TimingEnabled reports whether the timing tests are on: CG_TIMING_TESTS=1.
func TimingEnabled() bool { return os.Getenv(timingEnv) == "1" }

// RequireTiming skips the test unless CG_TIMING_TESTS=1.
//
// Use it in a test whose result depends on the wall clock or on machine load: it asserts an
// elapsed time or a latency budget, or it races a rate limit or a timeout. Such a test fails on a
// busy machine with nothing wrong in the code. The integrator's checks (heavy CI, Part A) set the
// variable. scripts/dev/local-tests.sh sets it only for a PR that adds or changes a timing test.
// Do not use it for a test that only uses a fake clock, or a long timeout as a safety net.
//
// A test that checks more than time can guard only its time check with TimingEnabled.
func RequireTiming(t testing.TB) {
	t.Helper()
	if !TimingEnabled() {
		t.Skip("timing test: set " + timingEnv + "=1 to run it (the integrator's checks do)")
	}
}
