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
