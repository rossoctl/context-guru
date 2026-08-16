package proxy

import (
	"errors"
	"testing"
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
