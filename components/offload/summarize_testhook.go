package offload

import (
	"errors"
	"time"
)

// WaitForSummaryForTest blocks until the session's in-flight summary has landed, or the deadline
// passes. It reports whether one landed.
//
// EXPORTED AND TEST-ONLY, and it is deliberately not a `_test.go` file: components/all's tests
// drive summarize through the registry from another package, and they need the same drain. The
// alternative was every caller sleeping, which is how a suite becomes both slow and flaky.
//
// It waits on the SAME channel a real turn waits on (summaryFlight.done), so a test that passes
// here is exercising the production synchronisation rather than a sleep that happens to be long
// enough on the machine that ran it.
func WaitForSummaryForTest(session string, within time.Duration) bool {
	j, ok := inFlight.peek(session)
	if !ok {
		// Nothing in flight: either it already finished, or it never started. Both are "no wait
		// needed" from the caller's point of view; the caller asserts which by looking at the
		// checkpoint.
		return true
	}
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case <-j.done:
		return true
	case <-t.C:
		return false
	}
}

// ErrSummaryDidNotLand is what a test helper returns when a summary never arrived, so a failure
// message can say that rather than reporting a confusing downstream symptom.
var ErrSummaryDidNotLand = errors.New("no summary landed within the deadline")

// WaitForAllSummariesForTest drains every in-flight summary, whatever session it belongs to.
//
// For tests that drive the pipeline through apply rather than calling the component directly: apply
// DERIVES a session id from the transcript when the caller supplies none (session.Scoped over the
// system prompt and first user message), so such a test cannot name the session it just created and
// cannot wait on it by name.
//
// It polls, unlike WaitForSummaryForTest, because "no summaries are running" is a property of the
// whole registry rather than of one channel. The interval is short and the deadline is the caller's,
// so a passing test is bounded by the work rather than by the poll.
func WaitForAllSummariesForTest(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		inFlight.mu.Lock()
		n := len(inFlight.jobs)
		inFlight.mu.Unlock()
		if n == 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
