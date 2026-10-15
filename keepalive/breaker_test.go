package keepalive

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// TestARouteTripIsPerRoutePath pins main's kaRouteKey shape: the key is tenant + endpoint +
// ROUTE PATH + model (`git show 67c5f758:proxy/keepalive.go` L1557-1558), not just
// tenant+endpoint+model. The ping goes to endpoint+route, so two different paths on the same
// endpoint must trip independently.
func TestARouteTripIsPerRoutePath(t *testing.T) {
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil }) // all-zero: failure
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 3, &now) // trips tenant t, endpoint "", route "/v1/messages", model claude-sonnet-5
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("RouteTrips = %d after 3 failures, want 1", s.RouteTrips)
	}
	key := Key{Tenant: "t", Session: "other-route"}
	r := trackedRecord(fd, key, now)
	r.Route = "/v1/chat/completions"
	k.Record(r)
	k.Record(r)
	if _, ok := k.Entry(key); !ok {
		t.Fatal("a trip on one route path must not refuse a hold on a different route path")
	}
}

// TestSweepStopsADueEntryWhoseRouteTrippedWhileIdle covers the Sweep gate (keeper.go, right
// after e.due(now)): a route can trip through OTHER sessions' failures while an entry sits
// idle waiting for its own next ping. When that entry becomes due, Sweep must send it no ping
// and must stop it, not merely skip one tick.
func TestSweepStopsADueEntryWhoseRouteTrippedWhileIdle(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil }) // all-zero: failure
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	// target holds the same route (endpoint "" + "/v1/messages") and model as failRoute's own
	// sessions, but with a long Idle so it is not itself due while the OTHER sessions trip it.
	target := Key{Tenant: "t", Session: "target"}
	r := trackedRecord(fd, target, now)
	r.Policy.Idle = 40 * time.Minute // due well inside the trip's 1-hour life, long after it fires
	k.Record(r)
	k.Record(r)
	if _, ok := k.Entry(target); !ok {
		t.Fatal("target must be tracked before the route trips")
	}
	failRoute(t, k, fd, "t", 3, &now) // same tenant, route and model as target: trips target's route
	if s := k.Stats(); s.RouteTrips != 1 {
		t.Fatalf("RouteTrips = %d, want 1", s.RouteTrips)
	}
	before := fd.n()
	now = start.Add(40*time.Minute + time.Second) // target is now due; the trip is still live
	k.Sweep(now)
	if got := fd.n(); got != before {
		t.Fatalf("a tripped route must send no ping: sent %d, want %d", got, before)
	}
	if e, ok := k.Entry(target); !ok || !e.Stopped {
		t.Fatalf("a tripped route must stop the due entry: entry=%+v ok=%v", e, ok)
	}
}

// TestSweepStopsADueEntryWhoseCredentialDiedWhileIdle is TestSweepStopsADueEntryWhoseRouteTrippedWhileIdle's
// counterpart for the dead-credential mark: a credential can be marked dead by a 401 on
// ANOTHER session while an entry using the same credential sits idle. When that entry becomes
// due, Sweep must send it no ping and must stop it.
func TestSweepStopsADueEntryWhoseCredentialDiedWhileIdle(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusUnauthorized, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	// target holds credential c1 with a long Idle, so it is not itself due when "other"'s ping
	// (same credential, default Idle) fires and marks c1 dead.
	target := Key{Tenant: "t", Session: "target"}
	r := credRecord(fd, target, now, "c1", "claude-sonnet-5")
	r.Policy.Idle = 40 * time.Minute
	k.Record(r)
	k.Record(r)
	other := Key{Tenant: "t", Session: "other"}
	k.Record(credRecord(fd, other, now, "c1", "claude-sonnet-5"))
	k.Record(credRecord(fd, other, now, "c1", "claude-sonnet-5"))
	now = start.Add(281 * time.Second)
	k.Sweep(now) // pings "other"; its 401 marks credential c1 dead for an hour from now
	before := fd.n()
	now = start.Add(40*time.Minute + time.Second) // target is now due; c1's mark is still live
	k.Sweep(now)
	if got := fd.n(); got != before {
		t.Fatalf("a dead credential must send no ping: sent %d, want %d", got, before)
	}
	if e, ok := k.Entry(target); !ok || !e.Stopped {
		t.Fatalf("a dead credential must stop the due entry: entry=%+v ok=%v", e, ok)
	}
}

// lockedBuf is a log sink a ping goroutine may write while the test reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs routes the default logger, at Debug, into a buffer for the rest of the test.
func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The operator-visible lines main logged for a ping's failure outcomes: a Warn for an all-zero 2xx
// and for the trip itself, a Debug for any other 4xx and for a credential refusal. None of them
// names the credential or its id.
func TestPingFailuresLogWhatMainLogged(t *testing.T) {
	logs := captureLogs(t)
	now := time.Now()
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusOK, nil })
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	failRoute(t, k, fd, "t", 3, &now)
	out := logs.String()
	for _, want := range []string{"ping got no usable answer",
		"keep-alive off for a route whose pings keep refreshing nothing", "route=/v1/messages",
		"model=claude-sonnet-5", "failures=3", "for=1h0m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}

	status := http.StatusBadRequest
	fd = newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, status, nil })
	k = New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	ping := func(key Key, cred string) {
		r := trackedRecord(fd, key, now)
		r.Cred = cred
		k.Record(r)
		k.Record(r) // a session's first request is never held
		now = now.Add(281 * time.Second)
		k.Sweep(now)
	}
	ping(Key{Tenant: "t", Session: "x"}, "")
	if !strings.Contains(logs.String(), "ping refused; not pinging this session again") {
		t.Errorf("a 400 did not log main's refusal line:\n%s", logs.String())
	}
	status = http.StatusUnauthorized
	ping(Key{Tenant: "t", Session: "y"}, "secret-cred-id")
	if !strings.Contains(logs.String(), "ping refused the credential; not pinging with it again") {
		t.Errorf("a 401 did not log main's credential line:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "secret-cred-id") {
		t.Errorf("a log line names the credential id:\n%s", logs.String())
	}
}
