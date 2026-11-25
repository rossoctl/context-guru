package proxy

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for a limiter. Safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// midMinute returns a fixed time 30 seconds into a minute and 30 minutes into an hour.
func midMinute() time.Time { return time.Date(2026, 1, 15, 10, 30, 30, 0, time.UTC) }

// fixLimiterClocks pins every limiter of the fixture's handler to one fake clock fixed at t.
// Call it before the first request. It returns the clock, so a test may advance it.
func (f *hostedFixture) fixLimiterClocks(t time.Time) *fakeClock {
	c := &fakeClock{t: t}
	for _, l := range []*Limiter{f.h.limiter, f.h.regLim, f.h.authLim, f.h.pwLim, f.h.codeLim} {
		l.now = c.now
	}
	return c
}

// pinned fixes a limiter's clock mid-minute and returns it, for tests that build their own.
func pinned(l *Limiter) *Limiter {
	l.now = (&fakeClock{t: midMinute()}).now
	return l
}

// The window is a fixed calendar minute. A burst that crosses a minute boundary gets a
// fresh budget, and a burst inside one minute does not.
func TestLimiterWindowResetsAtTheMinuteBoundary(t *testing.T) {
	c := &fakeClock{t: midMinute()}
	l := NewLimiter(Limits{RequestsPerMinute: 2})
	l.now = c.now
	for i := 0; i < 2; i++ {
		if _, err := l.Acquire("t"); err != nil {
			t.Fatalf("request %d refused inside the budget: %v", i+1, err)
		}
	}
	if _, err := l.Acquire("t"); err == nil {
		t.Fatal("a third request in the same minute was not refused")
	}
	c.advance(29 * time.Second) // 10:30:59, still the same minute
	if _, err := l.Acquire("t"); err == nil {
		t.Fatal("the window reset before the minute ended")
	}
	c.advance(2 * time.Second) // 10:31:01, next minute
	for i := 0; i < 2; i++ {
		if _, err := l.Acquire("t"); err != nil {
			t.Fatalf("request %d refused after the minute boundary: %v", i+1, err)
		}
	}
	if _, err := l.Acquire("t"); err == nil {
		t.Fatal("the new window did not refuse its third request")
	}
}

// The hourly action window resets at the hour boundary, and the retry text counts from
// the fake clock.
func TestAllowActionWindowResetsAtTheHourBoundary(t *testing.T) {
	c := &fakeClock{t: midMinute()}
	l := NewLimiter(Limits{})
	l.now = c.now
	if err := l.AllowAction("k", "arms", 1); err != nil {
		t.Fatalf("first action refused: %v", err)
	}
	err := l.AllowAction("k", "arms", 1)
	if err == nil {
		t.Fatal("second action in the same hour was not refused")
	}
	if want := "retry in 1771s"; !strings.Contains(err.Error(), want) { // 29m30s left, rounded up
		t.Errorf("error %q lacks %q", err.Error(), want)
	}
	c.advance(30 * time.Minute) // 11:00:30
	if err := l.AllowAction("k", "arms", 1); err != nil {
		t.Fatalf("action refused after the hour boundary: %v", err)
	}
}

func TestRateLimitRefusesPastTheWindow(t *testing.T) {
	l := NewLimiter(Limits{RequestsPerMinute: 3})
	for i := 0; i < 3; i++ {
		rel, err := l.Acquire("t1")
		rel()
		if err != nil {
			t.Fatalf("request %d was refused under the limit: %v", i+1, err)
		}
	}
	rel, err := l.Acquire("t1")
	rel()
	if err == nil {
		t.Fatal("the fourth request was allowed past a limit of 3")
	}
	var se StatusError
	if !errors.As(err, &se) || se.HTTPStatus() != 429 {
		t.Errorf("rate-limit error = %v, want a 429 StatusError", err)
	}
	// The message must say when to retry; a bare 429 is indistinguishable from the
	// service being broken, and the caller cannot see our logs.
	if got := err.Error(); got == "" || !strings.Contains(got, "retry in") {
		t.Errorf("rate-limit message does not say when to retry: %q", got)
	}
	// Another tenant is unaffected — the limit is per account, not global.
	rel, err = l.Acquire("t2")
	rel()
	if err != nil {
		t.Errorf("a second tenant was refused because the first hit its limit: %v", err)
	}
}

func TestConcurrencyLimitReleases(t *testing.T) {
	l := NewLimiter(Limits{Concurrent: 2})
	r1, err := l.Acquire("t1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire("t1")
	if err != nil {
		t.Fatal(err)
	}
	r3, err := l.Acquire("t1")
	r3()
	if err == nil {
		t.Fatal("a third concurrent request was allowed with a limit of 2")
	}
	// Releasing must free the slot, or the tenant is locked out after its first burst.
	r1()
	r4, err := l.Acquire("t1")
	r4()
	if err != nil {
		t.Errorf("a slot was not freed on release: %v", err)
	}
	r2()
}

// A zero Limits must be a working no-op, so the single-tenant path pays nothing and
// callers never have to nil-check.
func TestZeroLimitsIsANoOp(t *testing.T) {
	for _, l := range []*Limiter{NewLimiter(Limits{}), nil} {
		for i := 0; i < 100; i++ {
			rel, err := l.Acquire("t")
			rel()
			if err != nil {
				t.Fatalf("zero/nil limiter refused a request: %v", err)
			}
		}
	}
}

func TestCheapModelSemaphoreBlocksAndReleases(t *testing.T) {
	l := NewLimiter(Limits{CheapModelConcurrent: 1})
	r1, ok := l.AcquireCheapModel(nil)
	if !ok {
		t.Fatal("first acquire failed")
	}
	// A second caller must wait, and must give up when its deadline passes rather than
	// blocking an agent's request forever — the fallback is to skip compaction, which
	// is correct, just more expensive.
	done := make(chan struct{})
	close(done)
	if _, ok := l.AcquireCheapModel(done); ok {
		t.Error("acquired a slot that was already taken")
	}
	r1()
	r2, ok := l.AcquireCheapModel(nil)
	r2()
	if !ok {
		t.Error("the slot was not released")
	}
}

// A ping must never crowd out a real request, and the two ways that promise was broken are
// both cheap to assert.
func TestAcquireSpareLeavesRealTrafficAlone(t *testing.T) {
	t.Run("a refused ping does not spend the rate window", func(t *testing.T) {
		// Concurrency of 1 means no slack at all, so every ping is refused — and must cost the
		// tenant nothing. The earlier order incremented the minute counter first and never gave
		// it back, so ten refused pings ate ten of a hundred requests a minute, precisely when
		// the tenant was busiest.
		l := NewLimiter(Limits{RequestsPerMinute: 10, Concurrent: 1})
		for i := 0; i < 10; i++ {
			if rel, err := l.AcquireSpare("t", 0.25); err == nil {
				rel()
				t.Fatalf("ping %d was allowed with no spare concurrency", i)
			}
		}
		// The tenant's own budget must be untouched: ten real requests still fit.
		for i := 0; i < 10; i++ {
			rel, err := l.Acquire("t")
			if err != nil {
				t.Fatalf("real request %d was refused after %d refused pings: %v", i, 10, err)
			}
			rel()
		}
	})

	t.Run("a ping never takes the last slot", func(t *testing.T) {
		// Nothing is in flight, so this is the case the old test missed: with a budget of one,
		// taking the only slot IS crowding out whatever arrives next.
		l := NewLimiter(Limits{Concurrent: 1})
		if rel, err := l.AcquireSpare("t", 0.25); err == nil {
			rel()
			t.Error("a ping took the tenant's only concurrency slot on an idle limiter")
		}
		// With room for four, one ping may use one and three stay free for real traffic.
		l4 := NewLimiter(Limits{Concurrent: 4})
		rel, err := l4.AcquireSpare("t", 0.25)
		if err != nil {
			t.Fatalf("a ping was refused with three spare slots: %v", err)
		}
		defer rel()
		for i := 0; i < 3; i++ {
			r, err := l4.Acquire("t")
			if err != nil {
				t.Fatalf("real request %d refused while a ping held one of four slots: %v", i, err)
			}
			defer r()
		}
	})

	t.Run("reserved floors at zero", func(t *testing.T) {
		for _, tc := range []struct{ limit, want int }{{1, 0}, {2, 1}, {4, 3}, {16, 12}, {100, 75}} {
			if got := reserved(tc.limit, 0.25); got != tc.want {
				t.Errorf("reserved(%d) = %d, want %d", tc.limit, got, tc.want)
			}
		}
	})
}
