package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/testutil"
)

// awaitSignal receives one value from ch. It waits up to the shared test bound, not a short cap,
// so a slow machine does not fail the test. A signal that never comes still fails with what.
func awaitSignal[T any](t testing.TB, what string, ch <-chan T) T {
	t.Helper()
	var v T
	testutil.WaitFor(t, what, func() bool {
		select {
		case v = <-ch:
			return true
		default:
			return false
		}
	})
	return v
}

// awaitUnclaimed waits until no refresh holds the claim on key.
func awaitUnclaimed(t testing.TB, c *jsonCache, key string) {
	t.Helper()
	testutil.WaitFor(t, "the refresh releases its claim on "+key, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.refreshing[key]
	})
}
