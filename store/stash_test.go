package store

import (
	"strconv"
	"testing"
	"time"
)

// The key mix ONE reversible removal actually writes, in the proportion the pipeline writes
// it. This is the arithmetic #187 turned on, so the regression tests below drive it rather
// than a bare loop of stashes: the payload is one entry of five, and two of the other four
// are PINNED (cg:res:, cg:xres:), so under the old plain-Put behavior the cache kept "that
// output was dropped" and evicted "here is what it was".
//
// Measured before the fix, 600 removals against the shipped defaults of the day (1,000
// entries): 100 of 600 payloads survived against 350 of 600 of each pinned decision — the
// payload was 3.5x less durable than the record naming it.
func writeRemoval(t *testing.T, m *Memory, n int, payload []byte) (stashKey string, retained bool) {
	t.Helper()
	id := strconv.Itoa(n)
	stashKey = "aaaaaaaaaaaaaaaa" + id // a bare marker id, as expand.Marker embeds it
	retained = PutStash(m, stashKey, payload)
	m.Put("cg:own:sess:"+id, []byte{1})     // GET /expand ownership record
	m.Put("cg:xseen:"+id, []byte{1})        // recurrence flag
	m.Put(ResultPrefix+"sess:"+id, payload) // the session decision   (PINNED)
	m.Put(XResultPrefix+id, payload)        // the cross-session copy  (PINNED)
	return stashKey, retained
}

// A payload PutStash accepted must still be there after the cache has churned many times
// over — the #187 regression. What made this a defect rather than a tuning artifact is that
// the marker was already stamped: the request told the model it could have the content back.
func TestAnAcceptedStashSurvivesThePressureThatEvictsThePlainCache(t *testing.T) {
	m := NewMemory(Options{MaxEntries: 100}) // reserve = 50
	payload := make([]byte, 512)
	var accepted []string
	for i := 0; i < 40; i++ { // under the reserve, so every one is accepted
		k, ok := writeRemoval(t, m, i, payload)
		if !ok {
			t.Fatalf("removal %d refused with the reserve not yet full", i)
		}
		accepted = append(accepted, k)
	}
	// Churn the unpinned, un-stashed namespaces hard enough to turn the whole cache over.
	for i := 0; i < 5000; i++ {
		m.Put("cg:xseen:churn"+strconv.Itoa(i), []byte{1})
	}
	for _, k := range accepted {
		if _, ok := m.Get(k); !ok {
			t.Fatalf("stash %s was evicted after being accepted: a marker was stamped for "+
				"content this store can no longer produce, which is exactly #187", k)
		}
	}
}

// The reserve refuses rather than evicting a live payload, and it says so.
//
// The alternative — evict the oldest stash to admit the new one — keeps the mechanism
// removing at full rate and breaks an OLD promise per new one. That is the property #187
// called out: the guarantee degrading as the mechanism succeeds. Refusing moves the cost
// onto removals not yet made.
func TestAFullReserveRefusesInsteadOfEvictingALivePayload(t *testing.T) {
	m := NewMemory(Options{MaxEntries: 20}) // reserve = 10
	payload := make([]byte, 64)
	first, ok := writeRemoval(t, m, 0, payload)
	if !ok {
		t.Fatal("the first removal of an empty store was refused")
	}
	for i := 1; i < 10; i++ {
		if _, ok := writeRemoval(t, m, i, payload); !ok {
			t.Fatalf("removal %d refused below the reserve size", i)
		}
	}
	if _, ok := writeRemoval(t, m, 10, payload); ok {
		t.Fatal("the 11th removal into a 10-slot reserve was accepted; a refusal is what lets " +
			"the component decline the removal instead of stamping an unresolvable marker")
	}
	if _, ok := m.Get(first); !ok {
		t.Fatal("the OLDEST payload was evicted to admit a new one: an outstanding marker just " +
			"stopped resolving, which is the failure this reserve exists to prevent")
	}
	live, capacity, refused, _ := m.StashStats()
	if live != 10 || capacity != 10 || refused != 1 {
		t.Fatalf("StashStats() = (live %d, capacity %d, refused %d), want (10, 10, 1)", live, capacity, refused)
	}
}

// Refreshing a payload that is already present is never refused, however full the reserve.
//
// Components re-stash on every replay turn (offload.reapplyFrozen, extract_llm's apply) to
// keep an already-emitted marker resolvable. If a refresh could be refused, a component would
// decline to replay a decision it had already stamped, and the message would flip
// representation inside the provider's cached prefix — the cache-destructive direction, for
// content that is sitting right there.
func TestRefreshingALivePayloadIsNeverRefused(t *testing.T) {
	m := NewMemory(Options{MaxEntries: 4}) // reserve = 2
	if !m.PutStash("aaaaaaaaaaaaaaa1", []byte("one")) {
		t.Fatal("first stash refused")
	}
	if !m.PutStash("aaaaaaaaaaaaaaa2", []byte("two")) {
		t.Fatal("second stash refused")
	}
	if m.PutStash("aaaaaaaaaaaaaaa3", []byte("three")) {
		t.Fatal("a third stash was accepted into a 2-slot reserve")
	}
	if !m.PutStash("aaaaaaaaaaaaaaa1", []byte("one again")) {
		t.Fatal("a REFRESH of a payload already in the reserve was refused; a component would " +
			"decline to replay a marker it has already stamped and flip a cached message")
	}
	if b, ok := m.Get("aaaaaaaaaaaaaaa1"); !ok || string(b) != "one again" {
		t.Fatalf("refreshed payload = (%q, %v), want (\"one again\", true)", b, ok)
	}
	// The same contract for an entry that is PRESENT but holds no reserve slot — what a
	// plain Put under a marker key leaves behind (a store shared with a caller that does not
	// use the capability, or a key written before the reserve filled). "Retained" is a
	// question about the payload, not about the slot: the bytes are right there, and answering
	// false would make a component decline to replay a marker it has already stamped.
	m.Put("aaaaaaaaaaaaaaa4", []byte("four"))
	if !m.PutStash("aaaaaaaaaaaaaaa4", []byte("four again")) {
		t.Fatal("PutStash refused a payload that is IN THE STORE, because the reserve was full")
	}
	if b, ok := m.Get("aaaaaaaaaaaaaaa4"); !ok || string(b) != "four again" {
		t.Fatalf("payload after a refused-slot refresh = (%q, %v), want (\"four again\", true)", b, ok)
	}
}

// The TTL is the only thing that releases a reserve slot now that pressure cannot, so it has
// to actually release one — otherwise a long-lived proxy refuses every removal forever on the
// strength of payloads that expired hours ago.
func TestTheTTLReleasesReserveSlots(t *testing.T) {
	m := NewMemory(Options{MaxEntries: 4, TTLSeconds: 10}) // reserve = 2
	now := time.Now()
	m.SetClock(func() time.Time { return now })
	if !m.PutStash("aaaaaaaaaaaaaaa1", []byte("one")) || !m.PutStash("aaaaaaaaaaaaaaa2", []byte("two")) {
		t.Fatal("filling an empty reserve was refused")
	}
	if m.PutStash("aaaaaaaaaaaaaaa3", []byte("three")) {
		t.Fatal("a third stash was accepted into a full reserve")
	}
	now = now.Add(11 * time.Second) // both payloads are now past their TTL
	if !m.PutStash("aaaaaaaaaaaaaaa3", []byte("three")) {
		t.Fatal("the reserve stayed full after its payloads expired: it never recovers")
	}
	if _, _, _, expired := m.StashStats(); expired == 0 {
		t.Fatal("StashStats() reports 0 expired payloads after two were reclaimed by the TTL; " +
			"an operator reading expand_unresolved_missing cannot tell 'raise ttl_seconds' " +
			"from 'raise max_entries'")
	}
}

// The property #187 asks for by name: the guarantee must not degrade as the mechanism
// succeeds.
//
// Before the fix, every extra removal both consumed a slot and added a pinned decision, so
// BROKEN promises grew with the amount of work done. Here the number of broken promises is
// zero at every load — what grows instead is the count of removals declined, which is a
// refusal to promise rather than a promise broken.
func TestBrokenPromisesDoNotGrowWithLoad(t *testing.T) {
	payload := make([]byte, 512)
	prevRefused := int64(-1)
	for _, removals := range []int{40, 200, 1000} {
		m := NewMemory(Options{MaxEntries: 100}) // reserve = 50
		var accepted []string
		for i := 0; i < removals; i++ {
			if k, ok := writeRemoval(t, m, i, payload); ok {
				accepted = append(accepted, k)
			}
		}
		broken := 0
		for _, k := range accepted {
			if _, ok := m.Get(k); !ok {
				broken++
			}
		}
		_, _, refused, _ := m.StashStats()
		if broken != 0 {
			t.Errorf("%d removals: %d of %d advertised-reversible removals cannot be reversed; "+
				"reversibility is still load-dependent", removals, broken, len(accepted))
		}
		if refused <= prevRefused {
			t.Errorf("%d removals: refused = %d, not above the previous load's %d — the cost of "+
				"a full reserve must land on removals NOT MADE, and be counted",
				removals, refused, prevRefused)
		}
		prevRefused = refused
	}
}
