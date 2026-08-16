package proxy

import (
	"errors"
	"testing"
	"time"
)

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
	if got := err.Error(); got == "" || !contains(got, "retry in") {
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

// The spend cache must serve from memory rather than querying per request, and must
// forget a value when the cap changes so a manager's help takes effect at once.
func TestSpendCacheAndInvalidation(t *testing.T) {
	c := newSpendCache(time.Minute)
	calls := 0
	load := func(string) (float64, error) { calls++; return 42, nil }

	for i := 0; i < 5; i++ {
		v, err := c.get("t1", load)
		if err != nil || v != 42 {
			t.Fatalf("get = %v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Errorf("the cache queried %d times for 5 reads", calls)
	}
	c.invalidate("t1")
	if _, err := c.get("t1", load); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("invalidate did not force a re-read (calls=%d)", calls)
	}
}

// A failing spend lookup must surface the error so the caller can fail OPEN. Stopping
// someone's agent because a SUM failed would be the wrong trade — the cap is a budget
// guard, not a security boundary.
func TestSpendCacheSurfacesErrors(t *testing.T) {
	c := newSpendCache(time.Minute)
	boom := errors.New("db down")
	if _, err := c.get("t1", func(string) (float64, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Fatalf("get = %v, want the underlying error", err)
	}
	// A failure must not be cached, or one blip suppresses the cap for a whole TTL.
	calls := 0
	if _, err := c.get("t1", func(string) (float64, error) { calls++; return 7, nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Error("a failed lookup was cached")
	}
}

// checkSpend is the gate itself: no cap, under cap, over cap, and a broken lookup.
func TestCheckSpend(t *testing.T) {
	h := &Handler{spend: newSpendCache(time.Minute)}

	// No SpendChecker configured: enforcement is off entirely.
	if err := h.checkSpend(&Tenancy{ID: "t", MonthlyCapUSD: 1}); err != nil {
		t.Errorf("with no spend source: %v, want nil", err)
	}

	h.opts.Spend = spendFunc(func(string) (float64, error) { return 0.5, nil })
	if err := h.checkSpend(&Tenancy{ID: "t1", MonthlyCapUSD: 1}); err != nil {
		t.Errorf("under the cap: %v, want nil", err)
	}
	// An uncapped tenant is never refused.
	if err := h.checkSpend(&Tenancy{ID: "t2", MonthlyCapUSD: 0}); err != nil {
		t.Errorf("uncapped tenant: %v, want nil", err)
	}

	h.spend = newSpendCache(time.Minute)
	h.opts.Spend = spendFunc(func(string) (float64, error) { return 5, nil })
	err := h.checkSpend(&Tenancy{ID: "t3", MonthlyCapUSD: 1})
	if err == nil {
		t.Fatal("over the cap was allowed")
	}
	var se StatusError
	if !errors.As(err, &se) || se.HTTPStatus() != 402 {
		// 402, not 429: retrying will not help until the cap is raised or the month
		// turns over, and the status should say which kind of "no" this is.
		t.Errorf("over-cap status = %v, want 402", err)
	}

	// A broken lookup fails OPEN.
	h.spend = newSpendCache(time.Minute)
	h.opts.Spend = spendFunc(func(string) (float64, error) { return 0, errors.New("down") })
	if err := h.checkSpend(&Tenancy{ID: "t4", MonthlyCapUSD: 1}); err != nil {
		t.Errorf("a broken spend query blocked a request: %v", err)
	}
}

type spendFunc func(string) (float64, error)

func (f spendFunc) MonthToDateUSD(id string) (float64, error) { return f(id) }

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
