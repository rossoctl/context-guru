package keepalive

import (
	"fmt"
	"slices"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func trackedRecord(fd *fakeDispatcher, key Key, at time.Time) Record {
	return Record{Key: key, StartedAt: at, Provider: bschemas.Anthropic, Route: "/v1/messages",
		Status: 200, UsageOK: true, Usage: Usage{CacheRead: 50_000},
		Body:   []byte(`{"model":"claude-sonnet-5","max_tokens":4096,"messages":[]}`),
		Policy: Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 1},
		Hold:   fd.hold(key)}
}

// Shutdown's clear() loop zeroizes every entry; the §6.3 fix is that it also tells the host to
// Release the credential it Held for each one. Idempotent, and exactly once per Hold even
// across a second Shutdown call.
func TestShutdownReleasesEveryHeldEntryOnce(t *testing.T) {
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	keys := []Key{{Tenant: "t", Session: "a"}, {Tenant: "t", Session: "b"}, {Tenant: "t", Session: "b", Thread: "a:x"}}
	for _, key := range keys {
		k.Record(trackedRecord(fd, key, now)) // turn 0: counted, not tracked
		k.Record(trackedRecord(fd, key, now)) // turn 1: tracked, Held
	}
	for _, key := range keys {
		if got := fd.holds(key); got != 1 {
			t.Fatalf("%v held %d times before Shutdown, want 1", key, got)
		}
	}
	k.Shutdown()
	k.Shutdown() // idempotent
	for _, key := range keys {
		if got := fd.holds(key); got != 0 {
			t.Errorf("%v: Hold-Release balance %d after Shutdown, want 0", key, got)
		}
	}
	if len(fd.released) != len(keys) {
		t.Errorf("Release called %d times, want %d", len(fd.released), len(keys))
	}
}

func TestReRecordReleasesTheOldCredentialFirst(t *testing.T) {
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, now)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, now)) // turn 1: tracked, Held
	k.Record(trackedRecord(fd, key, now)) // replaces: Release old, Hold new
	if got := fd.holds(key); got != 1 {
		t.Fatalf("balance after replacement = %d, want 1", got)
	}
	if len(fd.released) != 1 {
		t.Fatalf("Release calls = %d, want 1", len(fd.released))
	}
	// The balance alone cannot tell the order apart. A Dispatcher keyed by Key (the proxy's is)
	// stores the new hold under the same key the old one used, so Hold-then-Release would delete
	// the NEW credential. The replacement must be release(k) THEN hold(k).
	want := []string{"hold:" + fmt.Sprint(key), "release:" + fmt.Sprint(key), "hold:" + fmt.Sprint(key)}
	if got := fd.log(); !slices.Equal(got, want) {
		t.Fatalf("hold/release order = %q, want %q", got, want)
	}
}

func TestKillSwitchHoldsNothing(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	fd := newFake(func(DispatchRequest) (Usage, int, error) { return Usage{}, 200, nil })
	now := time.Now()
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, now))
	k.Record(trackedRecord(fd, key, now))
	if got := fd.holds(key); got != 0 {
		t.Fatalf("kill switch on, but Hold was called %d times", got)
	}
}

// A nil Dispatcher would otherwise surface as a nil-interface panic on the first ping, far from
// the misconfiguration. New refuses it up front, with a message naming the field.
func TestNewPanicsWithoutADispatcher(t *testing.T) {
	defer func() {
		r := recover()
		if s, _ := r.(string); s != "keepalive.New: Options.Dispatcher is required" {
			t.Fatalf("recover() = %v, want the Dispatcher-required panic", r)
		}
	}()
	New(Options{})
}
