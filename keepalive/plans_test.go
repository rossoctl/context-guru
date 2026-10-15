package keepalive

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
)

func TestPlansReportOnlyThreadsStillToBePinged(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	main := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, main, start))
	k.Record(trackedRecord(fd, main, start))
	p := k.Plans("t", false, "s")
	pl, ok := p[""]
	if !ok || pl.PingsLeft != 2 || !pl.NextPing.Equal(start.Add(280*time.Second)) || !pl.RefreshedAt.Equal(start) {
		t.Fatalf("plan = %+v ok=%v", pl, ok)
	}
	if len(k.Plans("other", false, "s")) != 0 {
		t.Fatal("another tenant's plans must be empty")
	}
}

func TestAPingThatReadNothingIsNotARefresh(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{FreshInput: 40_000, Output: 1}, 200, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now) // a first miss: still pinging, but nothing was refreshed
	if pl := k.Plans("t", false, "s")[""]; !pl.RefreshedAt.Equal(start) {
		t.Fatalf("RefreshedAt = %v, want the real request's start %v", pl.RefreshedAt, start)
	}
}

func TestAPingThatReadMovesTheRefresh(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	if pl := k.Plans("t", false, "s")[""]; !pl.RefreshedAt.Equal(now) || pl.PingsLeft != 1 {
		t.Fatalf("plan = %+v, want RefreshedAt=%v PingsLeft=1", pl, now)
	}
}

// TestASummarySubstituteThatReadNothingIsNotARefresh is TestAPingThatReadNothingIsNotARefresh for
// the cache_aware_summarizer path, which books its own refresh (recordSummarySubstitute). A
// substitute that read nothing from the cache refreshed nothing that was there, so refreshedAt
// must stay on the real request; one that read the cache moves it to the substitute's start.
//
// Port of main's TestASummarySubstituteThatReadNothingIsNotARefresh
// (git show 67c5f758:proxy/keepaliveplan_test.go).
func TestASummarySubstituteThatReadNothingIsNotARefresh(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, start)) // turn 1: tracked, Held
	k.mu.Lock()
	e := k.live[key.id()]
	k.mu.Unlock()
	j := pingJob{e: e, tenant: "t", session: "s", st: e.st, emitter: e.emitter}
	refreshedAt := func() time.Time {
		k.mu.Lock()
		defer k.mu.Unlock()
		return e.refreshedAt
	}

	now = start.Add(time.Minute)
	k.recordSummarySubstitute(j, offload.KeepAliveSummaryResult{FreshInput: 10, CacheWrite: 48576, Output: 1}, 1, now)
	if got := refreshedAt(); !got.Equal(start) {
		t.Errorf("refreshedAt = %v after a substitute that read nothing, want the real request's start %v", got, start)
	}
	now = start.Add(2 * time.Minute)
	k.recordSummarySubstitute(j, offload.KeepAliveSummaryResult{CacheRead: 48576, Output: 1}, 1, now)
	if got := refreshedAt(); !got.Equal(now) {
		t.Errorf("refreshedAt = %v after a substitute that read the cache, want its start %v", got, now)
	}
}
