package keepalive

import (
	"strconv"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// #902. A ping touches the liveness clock under the alias the host derived from the ARRIVING request
// (Record.TouchAlias), not under one derived from the body the keeper holds. That body went through
// tool deferral, which changes the head the alias hashes.
func TestAPingTouchesTheAliasTheHostHandedIn(t *testing.T) {
	st := store.NewMemory(store.Options{})
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{CacheRead: 50_000, Output: 1}, 200, nil })
	start := time.Now()
	now := start
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	rec := func() Record {
		r := trackedRecord(fd, key, start)
		r.Store = st
		r.TouchAlias = "arriving-alias"
		// A held body whose head differs from the arriving request's, as after deferral.
		r.Body = []byte(`{"model":"claude-sonnet-5","max_tokens":4096,"messages":[` +
			`{"role":"user","content":"hi"},{"role":"system","content":"env\n\nnames list"}]}`)
		return r
	}
	k.Record(rec())
	k.Record(rec())
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	if fd.n() != 1 {
		t.Fatalf("%d pings sent, want 1", fd.n())
	}
	b, ok := st.Get(store.SeenPrefix + "arriving-alias")
	if !ok {
		t.Fatal("the ping did not touch the clock under the alias the host handed in")
	}
	if got, _ := strconv.ParseInt(string(b), 10, 64); got != now.UnixMilli() {
		t.Fatalf("clock = %d, want the ping's start %d", got, now.UnixMilli())
	}
}

func seenAt(t *testing.T, st store.Store, alias string) int64 {
	t.Helper()
	b, ok := st.Get(store.SeenPrefix + alias)
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(string(b), 10, 64)
	return n
}

// The manual ping (PingNow) touches the alias the host handed in, not one derived from the held body.
func TestAManualPingTouchesTheAliasTheHostHandedIn(t *testing.T) {
	k, _, clock := manualKeeper(t)
	st := store.NewMemory(store.Options{})
	k.mu.Lock()
	e := k.live[sess1.id()]
	e.st, e.touchAlias = st, "arriving-alias"
	k.mu.Unlock()
	res, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second})
	if err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	if got := seenAt(t, st, "arriving-alias"); got != res.StartedAt.UnixMilli() || got == 0 {
		t.Fatalf("clock = %d, want the press's start %d", got, res.StartedAt.UnixMilli())
	}
	_ = clock
}

// The substitute booking touches it too.
func TestASubstituteCallTouchesTheAliasTheHostHandedIn(t *testing.T) {
	k, _, clock := manualKeeper(t)
	st := store.NewMemory(store.Options{})
	k.mu.Lock()
	e := k.live[sess1.id()]
	k.mu.Unlock()
	j := pingJob{e: e, tenant: e.tenant, session: e.session, key: e.key, st: st, touchAlias: "arriving-alias",
		raw: []byte(`{"messages":[{"role":"user","content":"x"}]}`)}
	k.recordSubstitutePing(j, "", Usage{CacheRead: 40_000, Output: 1}, 5, clock.now(), 0)
	if got := seenAt(t, st, "arriving-alias"); got != clock.now().UnixMilli() {
		t.Fatalf("clock = %d, want %d", got, clock.now().UnixMilli())
	}
}
