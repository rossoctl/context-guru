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
	limit := effectiveBound(t)
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
	if state != nil {
		t.Fatalf("timed out after %v waiting for %s; state: %s", time.Since(start).Round(time.Millisecond), what, describe(state))
	}
	t.Fatalf("timed out after %v waiting for %s", time.Since(start).Round(time.Millisecond), what)
}

// deadlineMargin is the time kept before the test deadline, so the failure message of WaitFor
// shows in place of a bare "test timed out".
const deadlineMargin = 5 * time.Second

// effectiveBound is the safety bound, cut to the time left before the test deadline minus
// deadlineMargin. It is never below one poll interval.
func effectiveBound(t testing.TB) time.Duration {
	limit := time.Duration(bound.Load())
	if d, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if dl, has := d.Deadline(); has {
			if left := time.Until(dl) - deadlineMargin; left < limit {
				limit = max(left, pollInterval)
			}
		}
	}
	return limit
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
