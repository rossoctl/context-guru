package keepalive

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// #706. The gateway answers a byte-identical request from its own response cache, so an identical
// second ping never reaches the provider and refreshes nothing. Every ping an entry sends must
// therefore differ from the one before it, and only outside the prefix the provider hashes.
func TestConsecutivePingsAreNeverByteIdentical(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	r := trackedRecord(fd, key, start)
	r.Policy.MaxPings = 3
	k.Record(r) // turn 0: counted, not tracked
	k.Record(r) // turn 1: tracked, Held
	for i := 1; i <= 3; i++ {
		now = start.Add(time.Duration(i) * 281 * time.Second)
		k.Sweep(now)
	}
	if fd.n() != 3 {
		t.Fatalf("sent %d pings, want 3", fd.n())
	}
	for i := 1; i < len(fd.sent); i++ {
		if bytes.Equal(fd.sent[i].Body, fd.sent[i-1].Body) {
			t.Fatalf("ping %d is byte-identical to ping %d: the gateway answers it from its response "+
				"cache and the provider's cache is not refreshed", i+1, i)
		}
	}
	if got := gjson.GetBytes(fd.sent[0].Body, "max_tokens").Int(); got != 1 {
		t.Errorf("first ping max_tokens = %d, want 1 (unchanged from before #706)", got)
	}
	for _, p := range []string{"model", "system", "messages", "tools", "tool_choice", "thinking"} {
		a, b := gjson.GetBytes(fd.sent[0].Body, p).Raw, gjson.GetBytes(fd.sent[1].Body, p).Raw
		if a != b {
			t.Errorf("%s differs between pings: the hashed prefix must stay byte-identical", p)
		}
	}
}

// A manual press is a ping too: it must not repeat the automatic ping before it.
func TestAManualPingDiffersFromTheAutomaticPingBeforeIt(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start))
	k.Record(trackedRecord(fd, key, start))
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	now = now.Add(10 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: key}); err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	if fd.n() != 2 {
		t.Fatalf("sent %d pings, want 2", fd.n())
	}
	if bytes.Equal(fd.sent[0].Body, fd.sent[1].Body) {
		t.Fatal("the manual ping is byte-identical to the automatic ping before it")
	}
}

// The output budget cycles within a small range, so a long-lived entry never asks for a large
// answer.
func TestPingOutputBudgetStaysSmall(t *testing.T) {
	for n := 0; n < 3*pingVary; n++ {
		out, ok := pingBodyN([]byte(`{"model":"m","max_tokens":4096,"messages":[]}`), "/v1/messages", n)
		if !ok {
			t.Fatal("pingBodyN refused a well-formed body")
		}
		if got := gjson.GetBytes(out, "max_tokens").Int(); got < 1 || got > pingVary {
			t.Fatalf("ping %d: max_tokens = %d, want 1..%d", n, got, pingVary)
		}
	}
	out, _ := pingBodyN([]byte(`{"model":"m","max_output_tokens":4096,"input":[]}`), "/v1/responses", 5)
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 21 {
		t.Errorf("Responses ping 5: max_output_tokens = %d, want 16+5", got)
	}
}

// #712 review, finding 2. A client that re-sends a byte-identical turn starts a NEW entry on the
// same thread with the same body. Its first ping must not repeat the last ping the previous entry
// sent, or the gateway answers it from its response cache.
func TestANewEntryDoesNotRepeatThePreviousEntrysLastPing(t *testing.T) {
	start := time.Now()
	now := start
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() }})
	key := Key{Tenant: "t", Session: "s"}
	k.Record(trackedRecord(fd, key, start)) // turn 0: counted, not tracked
	k.Record(trackedRecord(fd, key, start)) // turn 1: tracked
	now = start.Add(281 * time.Second)
	k.Sweep(now)
	// The same request again: a new entry, byte-identical body.
	again := now.Add(5 * time.Second)
	k.Record(trackedRecord(fd, key, again))
	now = again.Add(281 * time.Second)
	k.Sweep(now)
	if fd.n() != 2 {
		t.Fatalf("sent %d pings, want 2", fd.n())
	}
	if bytes.Equal(fd.sent[0].Body, fd.sent[1].Body) {
		t.Fatal("the new entry's first ping is byte-identical to the previous entry's last ping")
	}
}

// #712 review, finding 1. A cycle that sends nothing (the limiter refused it) must not use up a
// step. Otherwise pingVary such cycles between two sent pings make the two identical again.
func TestACycleThatSendsNothingUsesNoStep(t *testing.T) {
	start := time.Now()
	now := start
	refuse := false
	fd := newFake(okPing)
	k := New(Options{Dispatcher: fd, Now: func() time.Time { return now }, Go: func(f func()) { f() },
		Spare: func(string, float64) (func(), error) {
			if refuse {
				return nil, errors.New("no spare slot")
			}
			return func() {}, nil
		}})
	key := Key{Tenant: "t", Session: "s"}
	r := trackedRecord(fd, key, start)
	r.Policy.MaxPings = pingVary + 2
	k.Record(r)
	k.Record(r)
	now = start.Add(281 * time.Second)
	k.Sweep(now) // sent
	refuse = true
	for i := 0; i < pingVary-1; i++ {
		now = now.Add(281 * time.Second)
		k.Sweep(now) // due, refused, nothing sent
	}
	refuse = false
	now = now.Add(281 * time.Second)
	k.Sweep(now) // sent
	if fd.n() != 2 {
		t.Fatalf("sent %d pings, want 2", fd.n())
	}
	if bytes.Equal(fd.sent[0].Body, fd.sent[1].Body) {
		t.Fatalf("after %d refused cycles the next sent ping repeats the last sent one", pingVary-1)
	}
}
