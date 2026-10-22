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

// TestPlansAllDropsOnlyTheTenantFilter: all=true drops the TENANT filter, exactly like main's
// allTenants, but still requires session to match and still keys by thread — it does not widen
// to "every session" and does not key by id. Two tenants sharing session id "s" each get a
// main-thread ("") entry; tenant "a" additionally has a subagent thread "a:x". Port of the
// controller's ruling against main's Handler.KeepAlivePlans (git show
// 67c5f758:proxy/keepalive.go L2102), whose real caller (dash/cachestate.go:295 via
// dash/api.go:566) passes allTenants=true WITH a specific session for every single-tenant
// deployment.
func TestPlansAllDropsOnlyTheTenantFilter(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	mainA := Key{Tenant: "a", Session: "s"}
	subA := Key{Tenant: "a", Session: "s", Thread: "a:x"}
	mainB := Key{Tenant: "b", Session: "s"}
	for _, key := range []Key{mainA, subA, mainB} {
		k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
		k.Record(trackedRecord(fd, key, start)) // turn 1: tracked, Held
	}

	// all=false: tenant AND session must match, keyed by thread. Only tenant a's two threads.
	byTenant := k.Plans("a", false, "s")
	if len(byTenant) != 2 {
		t.Fatalf("Plans(a, false, s) = %+v, want exactly tenant a's two threads", byTenant)
	}
	if _, ok := byTenant[""]; !ok {
		t.Errorf("Plans(a, false, s) missing the main thread \"\": %+v", byTenant)
	}
	if _, ok := byTenant["a:x"]; !ok {
		t.Errorf("Plans(a, false, s) missing the subagent thread \"a:x\": %+v", byTenant)
	}

	// A session id never matching across tenants with all=false: an unrelated tenant with the
	// right session still gets nothing of its own session back if the tenant is wrong.
	if len(k.Plans("other", false, "s")) != 0 {
		t.Fatal("another tenant's plans must be empty")
	}

	// all=true: only the tenant filter drops. session still must match, and the result is still
	// keyed by thread — never by id, and never "every session". The subagent thread "a:x" has no
	// collision (only tenant a has it) and must appear intact. The main thread "" collides between
	// tenant a and tenant b's entries; main's own code (`out[e.thread] = ...` with no tie-break)
	// lets whichever one map iteration visits last win, so this only asserts presence, not whose
	// data survived.
	byAll := k.Plans("", true, "s")
	if len(byAll) != 2 {
		t.Fatalf("Plans(\"\", true, s) = %+v, want exactly 2 threads (main-thread collision + subagent)", byAll)
	}
	if pl, ok := byAll["a:x"]; !ok || pl.PingsLeft != 2 {
		t.Errorf("Plans(\"\", true, s)[\"a:x\"] = %+v, ok=%v, want tenant a's uncontended subagent plan", pl, ok)
	}
	if _, ok := byAll[""]; !ok {
		t.Error("Plans(\"\", true, s) missing the main thread \"\" (one of the two colliding tenants' entries)")
	}

	// A session id that does not exist anywhere must come back empty even with all=true: all
	// never widens the match to "every session".
	if len(k.Plans("", true, "no-such-session")) != 0 {
		t.Fatal("all=true must still require session to match; got entries for a nonexistent session")
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
