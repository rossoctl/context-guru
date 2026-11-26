package store

import (
	"testing"
	"time"
)

// A DIAGNOSTIC MUST NOT RENEW WHAT IT ASKS ABOUT.
//
// Get slides the TTL and reorders the LRU, which is right for a caller about to use the value and
// wrong for one that only wants a fact. It matters most for the PINNED namespaces: pinned entries
// count against the shared exempt budget that gates rewind-reserve admission, so a probe that keeps
// a pin alive forever applies back-pressure to the reserve — a resource decision made by accident.
func TestPeekDoesNotRenewTheEntryItAsksAbout(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemory(Options{TTLSeconds: 100})
	m.SetClock(func() time.Time { return now })
	key := FrozenPrefix + "s:mask:abc"
	m.Put(key, []byte("the compacted bytes"))

	// Ten probes, 60s apart: well past the 100s TTL in total, never more than 60s between probes.
	for i := 0; i < 10; i++ {
		now = now.Add(60 * time.Second)
		m.Peek(key)
	}
	if m.Peek(key) {
		t.Fatal("the entry survived 600s of a 100s TTL because the probe kept sliding it: a " +
			"read-only question is renewing a pinned entry, which is back-pressure on the reserve")
	}
	// And the control: Get is still expected to slide, because its caller is about to use the value.
	now = time.Unix(0, 0)
	m2 := NewMemory(Options{TTLSeconds: 100})
	m2.SetClock(func() time.Time { return now })
	m2.Put(key, []byte("x"))
	for i := 0; i < 10; i++ {
		now = now.Add(60 * time.Second)
		m2.Get(key)
	}
	if _, ok := m2.Get(key); !ok {
		t.Fatal("Get stopped sliding the TTL; that is the sliding-TTL guarantee, not a side effect")
	}
}

// Peek answers the question Get answers, minus the side effects — including for an expired entry,
// which it must report absent without removing (removal would mutate stash accounting from a probe).
func TestPeekAgreesWithGetOnPresence(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemory(Options{TTLSeconds: 100})
	m.SetClock(func() time.Time { return now })
	if m.Peek("nothing") {
		t.Error("Peek claims an absent key is live")
	}
	m.Put("k", []byte("v"))
	if !m.Peek("k") {
		t.Error("Peek claims a live key is absent")
	}
	now = now.Add(101 * time.Second)
	if m.Peek("k") {
		t.Error("Peek claims an expired key is live")
	}
	if _, still := m.items["k"]; !still {
		t.Error("Peek REMOVED an expired entry; a probe must not mutate the store's accounting")
	}
}

// PeekValue is Peek with the payload: the same answer, the same lack of side effects. A store without the
// capability falls back to Get.
func TestPeekValueReturnsThePayloadWithoutRenewingIt(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemory(Options{TTLSeconds: 100})
	m.SetClock(func() time.Time { return now })
	m.Put(SeenPrefix+"a", []byte("2000:1h"))
	if v, ok := m.PeekValue(SeenPrefix + "a"); !ok || string(v) != "2000:1h" {
		t.Fatalf("PeekValue = %q, %v", v, ok)
	}
	if _, ok := PeekValue(m, "missing"); ok {
		t.Error("a missing key was present")
	}
	for i := 0; i < 10; i++ {
		now = now.Add(60 * time.Second)
		PeekValue(m, SeenPrefix+"a")
	}
	if _, ok := m.PeekValue(SeenPrefix + "a"); ok {
		t.Fatal("PeekValue kept the entry alive past its TTL")
	}
	if _, ok := PeekValue(Nop{}, "x"); ok {
		t.Error("a store with no capability and no data returned a value")
	}
}

// Keeps tells a store that holds nothing (nil, Nop) from one that does, by type, not by a missed Get.
func TestKeepsIsTrueOnlyForAStoreThatCanHoldAValue(t *testing.T) {
	var nilStore Store
	var nilNop *Nop
	for name, tc := range map[string]struct {
		s    Store
		want bool
	}{"nil": {nilStore, false}, "Nop": {Nop{}, false}, "*Nop": {&Nop{}, false}, "typed nil *Nop": {nilNop, false},
		"Memory": {NewMemory(Options{}), true}} {
		if got := Keeps(tc.s); got != tc.want {
			t.Errorf("%s: Keeps = %v, want %v", name, got, tc.want)
		}
	}
}
