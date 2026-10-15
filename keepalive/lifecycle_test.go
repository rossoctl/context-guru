package keepalive

import (
	"testing"
	"time"
)

func TestARequestThatStartedBeforeTheEndIsNotHeld(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.MarkEnded("t", "s", start.Add(time.Second))
	k.Record(trackedRecord(fd, key, start)) // in flight when the session ended
	if _, ok := k.Entry(key); ok {
		t.Fatal("a request that started before SessionEnd must not re-arm the keep-alive")
	}
	k.Record(trackedRecord(fd, key, start.Add(2*time.Second))) // the session resumed
	if _, ok := k.Entry(key); !ok {
		t.Fatal("a request that started after the end mark is a resume and must be held")
	}
}

func TestCompactRetiresOnlyTheMainThreadStartedBefore(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	main := Key{Tenant: "t", Session: "s"}
	sub := Key{Tenant: "t", Session: "s", Thread: "a:x"}
	for _, key := range []Key{main, sub} {
		k.Record(trackedRecord(fd, key, start))
		k.Record(trackedRecord(fd, key, start))
	}
	if n := k.RetireSessionStartedBefore("t", "s", start.Add(time.Second)); n != 1 {
		t.Fatalf("retired %d, want 1 (the main thread only)", n)
	}
	if _, ok := k.Entry(main); ok {
		t.Fatal("the main thread must be retired")
	}
	if _, ok := k.Entry(sub); !ok {
		t.Fatal("a subagent thread must survive a compact")
	}
}

func TestALateCompactKeepsThePostCompactionEntry(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start.Add(5*time.Second))) // the post-compaction request
	if n := k.RetireSessionStartedBefore("t", "s", start.Add(time.Second)); n != 0 {
		t.Fatalf("retired %d, want 0: the entry started after the cutoff", n)
	}
}

func TestTheClientAliasMatches(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	key := Key{Tenant: "t", Session: "content-hash"}
	r := trackedRecord(fd, key, start)
	r.Client = "codex-session-id"
	k.Record(r)
	k.Record(r)
	if n := k.RetireSessionStartedBefore("t", "codex-session-id", time.Time{}); n != 1 {
		t.Fatalf("retired %d by the client alias, want 1", n)
	}
}

func TestEndedMarksArePerTenant(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	k.MarkEnded("a", "s", start.Add(time.Second))
	if k.EndedAfter("b", "s", start) {
		t.Fatal("tenant a's end mark must not end tenant b's session")
	}
}

func TestForgetClearsEveryNewMapForThatTenantOnly(t *testing.T) {
	start := time.Now()
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return start }})
	for _, ten := range []string{"a", "b"} {
		k.MarkEnded(ten, "s", start.Add(time.Second))
		k.Arrive(Key{Tenant: ten, Session: "s"})
		k.mu.Lock()
		k.routeFailedLocked(routeKey(ten, "e", "/r", "m"), start)
		k.markCredDeadLocked(ten, "c", "m", 401, start)
		k.mu.Unlock()
	}
	k.Forget("a")
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, n := range map[string]int{"ended": len(k.ended), "inflight": len(k.inflight),
		"routeFails": len(k.routeFails), "credDead": len(k.credDead)} {
		if n != 1 {
			t.Errorf("%s has %d entries after Forget(a), want 1 (tenant b's)", name, n)
		}
	}
}
