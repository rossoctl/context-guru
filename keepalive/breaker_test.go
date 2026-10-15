package keepalive

import (
	"net/http"
	"testing"
	"time"
)

// failing pings on one route, each on its own fresh session, so each failure is a separate entry.
func failRoute(t *testing.T, k *Keeper, fd *fakeDispatcher, tenant string, n int, now *time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		key := Key{Tenant: tenant, Session: "s" + string(rune('a'+i))}
		k.Record(trackedRecord(fd, key, *now))
		k.Record(trackedRecord(fd, key, *now))
		*now = now.Add(281 * time.Second)
		k.Sweep(*now)
	}
}

func TestPingsThatFailTripTheRoute(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil }) // all-zero: failure
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 3, &now)
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("RouteTrips = %d after 3 failures, want 1", s.RouteTrips)
	}
	key := Key{Tenant: "t", Session: "new"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); ok {
		t.Fatal("a tripped route must refuse a new hold")
	}
	now = now.Add(61 * time.Minute)
	k.Record(trackedRecord(fd, key, now)) // twice: a refused record may not count as a turn
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); !ok {
		t.Fatal("the trip must expire after an hour")
	}
}

func TestARouteTripIsPerTenant(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "a", 3, &now)
	key := Key{Tenant: "b", Session: "s"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if _, ok := k.Entry(key); !ok {
		t.Fatal("tenant a's trip must not refuse tenant b")
	}
}

func TestAGoodPingResetsTheCount(t *testing.T) {
	now := time.Now()
	status := http.StatusBadRequest
	fd := newFake(func(DispatchRequest) (Usage, int, error) {
		if status == http.StatusOK {
			return Usage{CacheRead: 50_000, Output: 1}, status, nil
		}
		return Usage{}, status, nil
	})
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 2, &now)
	status = http.StatusOK
	failRoute(t, k, fd, "t", 1, &now)
	status = http.StatusBadRequest
	failRoute(t, k, fd, "t", 2, &now)
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatalf("a good ping must reset the run: RouteTrips = %d", s.RouteTrips)
	}
}

func credRecord(fd *fakeDispatcher, key Key, at time.Time, cred, model string) Record {
	r := trackedRecord(fd, key, at)
	r.Cred = cred
	r.Body = []byte(`{"model":"` + model + `","max_tokens":4096,"messages":[]}`)
	return r
}

func TestA401StopsThatCredentialOnEveryModel(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "t", Session: "a"}
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	b := Key{Tenant: "t", Session: "b"}
	k.Record(credRecord(fd, b, now, "c1", "claude-haiku-4-5"))
	k.Record(credRecord(fd, b, now, "c1", "claude-haiku-4-5"))
	if _, ok := k.Entry(b); ok {
		t.Fatal("a 401 must refuse the same credential on another model")
	}
	c := Key{Tenant: "t", Session: "c"}
	k.Record(credRecord(fd, c, now, "c2", "claude-haiku-4-5"))
	k.Record(credRecord(fd, c, now, "c2", "claude-haiku-4-5"))
	if _, ok := k.Entry(c); !ok {
		t.Fatal("another credential must still be held")
	}
	if s := k.Stats(); s.RouteTrips != 0 {
		t.Fatal("an auth refusal with a credential id must not count toward the route breaker")
	}
}

func TestA403BlocksTheKeyOnThatModelOnly(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusForbidden, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "t", Session: "a"}
	k.Record(credRecord(fd, a, now, "c1", "claude-opus-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-opus-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	same := Key{Tenant: "t", Session: "same"}
	k.Record(credRecord(fd, same, now, "c1", "claude-opus-5"))
	k.Record(credRecord(fd, same, now, "c1", "claude-opus-5"))
	if _, ok := k.Entry(same); ok {
		t.Fatal("a 403 must refuse the same credential on the same model")
	}
	other := Key{Tenant: "t", Session: "other"}
	k.Record(credRecord(fd, other, now, "c1", "claude-haiku-4-5"))
	k.Record(credRecord(fd, other, now, "c1", "claude-haiku-4-5"))
	if _, ok := k.Entry(other); !ok {
		t.Fatal("a 403 must not refuse the credential on another model")
	}
}

func TestADeadCredentialIsPerTenant(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	a := Key{Tenant: "a", Session: "s"}
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, a, now, "c1", "claude-sonnet-5"))
	now = now.Add(281 * time.Second)
	k.Sweep(now)
	b := Key{Tenant: "b", Session: "s"}
	k.Record(credRecord(fd, b, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, b, now, "c1", "claude-sonnet-5"))
	if _, ok := k.Entry(b); !ok {
		t.Fatal("tenant a's dead credential must not refuse tenant b")
	}
}

func TestEmptyTenantAndCredUseTheRouteBreaker(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "", 3, &now) // Cortex's shape: no tenant, no credential id
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("a 401 with no credential id must count toward the breaker: RouteTrips = %d", s.RouteTrips)
	}
}
