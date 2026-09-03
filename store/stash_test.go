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
	// 400 entries, not 100. One removal writes THREE exempt entries (the payload plus the two
	// pinned decisions), and the exemptions are jointly bounded at max−max/4 so that a quarter
	// of the cache always stays evictable — so the real key mix saturates the joint budget at
	// max/4 removals, well before the reserve's own max/2 cap. 400 leaves room for 100
	// removals; this test takes 40 of them.
	m := NewMemory(Options{MaxEntries: 400})
	payload := make([]byte, 512)
	var accepted []string
	for i := 0; i < 40; i++ { // under both budgets, so every one is accepted
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
	// Bare PutStash rather than the full writeRemoval mix, so the bound under test is the
	// RESERVE's own entry cap and nothing else. With the real mix the joint exempt budget binds
	// first (three exempt entries per removal against max−max/4), which is a different property
	// and has its own test below — sharing one fixture would leave neither bound pinned.
	stashed := func(n int) string { return "aaaaaaaaaaaaaaaa" + strconv.Itoa(n) }
	first := stashed(0)
	if !m.PutStash(first, payload) {
		t.Fatal("the first payload into an empty reserve was refused")
	}
	for i := 1; i < 10; i++ {
		if !m.PutStash(stashed(i), payload) {
			t.Fatalf("payload %d refused below the reserve size", i)
		}
	}
	if m.PutStash(stashed(10), payload) {
		t.Fatal("the 11th payload into a 10-slot reserve was accepted; a refusal is what lets " +
			"the component decline the removal instead of stamping an unresolvable marker")
	}
	if _, ok := m.Get(first); !ok {
		t.Fatal("the OLDEST payload was evicted to admit a new one: an outstanding marker just " +
			"stopped resolving, which is the failure this reserve exists to prevent")
	}
	st := m.StashStats()
	if st.Live != 10 || st.Capacity != 10 || st.Refused != 1 {
		t.Fatalf("StashStats() = (live %d, capacity %d, refused %d), want (10, 10, 1)",
			st.Live, st.Capacity, st.Refused)
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
	if st := m.StashStats(); st.Expired == 0 {
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
		refused := m.StashStats().Refused
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

// The two exemptions must never occupy the whole entry cap, because a cache with nothing
// evictable does not fail loudly — it silently discards writes.
//
// pinCap and stashCap are each max/2, so before the evictable floor they could sum to max. At
// that point evictOldest walks the list, finds every entry exempt and returns false, and the
// next plain Put pushes its entry to the front where the eviction loop immediately takes it
// back as the only non-exempt entry in the cache. The write "succeeds" and the key is gone.
//
// Each of the unpinned namespaces that happens to is load-bearing: cg:keep: is what stops the
// expand loop (content the agent just expanded gets re-compacted without it), cg:sum: is
// summarize's checkpoint, cg:own: gates GET /expand, cg:xseen: prices recurrence. And the
// state is reached by exactly the workload the reserve was built for — a reversible removal
// writes two pinned decisions and one stash, so both exemptions saturate together.
func TestTheExemptionsLeaveAGuaranteedEvictableFloor(t *testing.T) {
	const max = 200
	m := NewMemory(Options{MaxEntries: max})
	payload := make([]byte, 64)
	// Drive the real key mix until the store refuses, which is the joint budget binding: three
	// exempt entries per removal against max−max/4.
	removals, refused := 0, 0
	for i := 0; i < max; i++ {
		if _, ok := writeRemoval(t, m, i, payload); ok {
			removals++
		} else {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("the fixture never saturated the exemptions, so it cannot show whether a floor " +
			"is held back — raise the removal count")
	}
	// The precondition that makes the assertion below meaningful: the exemptions are as full as
	// this store will let them get.
	st := m.StashStats()
	if st.Live == 0 {
		t.Fatal("no payload was retained at all; the fixture is not exercising the reserve")
	}
	// The property. A write to an UNPINNED namespace must still be readable afterwards — under
	// max/2 + max/2 it was evicted by the very Put that inserted it.
	m.Put("cg:keep:justexpanded", []byte{1})
	if _, ok := m.Get("cg:keep:justexpanded"); !ok {
		t.Error("a cg:keep: write did not survive its own Put: the exemptions have consumed the " +
			"whole entry cap, so isKeptVerbatim is permanently false and content the agent just " +
			"expanded will be re-compacted — the expand loop that flag exists to stop")
	}
	m.Put("cg:sum:sess", []byte(`{"c":1}`))
	if _, ok := m.Get("cg:sum:sess"); !ok {
		t.Error("a cg:sum: checkpoint did not survive its own Put: summarize can never " +
			"checkpoint, so it re-pays its model call on every turn")
	}
	// And the floor is a real quantity, not just "one write got through".
	if got := m.evictableEntriesForTest(); got < max/4 {
		t.Errorf("only %d of %d entries are evictable, want at least the max/4 = %d floor",
			got, max, max/4)
	}
}

// The reserve is bounded in BYTES as well as entries, and the byte budget is the one that
// binds first for large payloads.
//
// Entries are a poor proxy for memory in this namespace and only in this namespace: every
// other exempt entry is a marker line, a compacted projection or an integer, while a payload
// is a whole tool output. So "2,500 entries" named a memory figure anywhere across two orders
// of magnitude depending on nothing the operator had chosen.
func TestTheReserveIsBoundedInBytesNotOnlyEntries(t *testing.T) {
	// Entries are deliberately not the constraint: a 1,000-entry cap is a 500-slot reserve,
	// and 6 payloads is nowhere near it. 5 KiB of budget against 2 KiB payloads is.
	m := NewMemory(Options{MaxEntries: 1000, StashMaxBytes: 5 << 10})
	payload := make([]byte, 2<<10)
	accepted := 0
	for i := 0; i < 6; i++ {
		if m.PutStash("aaaaaaaaaaaaaaaa"+strconv.Itoa(i), payload) {
			accepted++
		}
	}
	if accepted != 2 {
		t.Errorf("%d payloads of 2 KiB were accepted into a 5 KiB reserve, want 2: the byte "+
			"budget is not bounding the reserve, so max_entries is still the only limit and it "+
			"does not describe memory", accepted)
	}
	st := m.StashStats()
	if st.Bytes != int64(accepted)*int64(len(payload)) || st.MaxBytes != 5<<10 {
		t.Errorf("StashStats() = (bytes %d, max %d), want (%d, %d): an operator cannot tell "+
			"WHICH budget bound, and so cannot tell whether to raise max_entries or "+
			"stash_max_bytes", st.Bytes, st.MaxBytes, accepted*len(payload), 5<<10)
	}
	if st.Live >= st.Capacity {
		t.Errorf("live %d reached capacity %d, so this test proved nothing about bytes",
			st.Live, st.Capacity)
	}
	// A refusal on bytes is still a refusal, counted the same way: it means removals were
	// declined, not made irreversibly.
	if st.Refused != 4 {
		t.Errorf("StashStats().Refused = %d, want 4; a byte-budget refusal must be reported "+
			"exactly like an entry-budget one", st.Refused)
	}
	// And the bytes are RELEASED with the slot, or the budget ratchets shut permanently.
	m.SetClock(func() time.Time { return time.Now().Add(2 * DefaultTTL) })
	if !m.PutStash("bbbbbbbbbbbbbbbb", payload) {
		t.Error("the reserve stayed byte-full after its payloads expired: stashBytes is never " +
			"credited back, so one busy period disables marker mode for the life of the process")
	}
}

// StashRoom answers the capacity question BEFORE the caller pays to produce the content whose
// marker the payload would back.
//
// It exists for one caller shape: summarize's model call was measured at ~57k prompt tokens,
// and it used to be paid and then thrown away when the reserve refused — every turn, because a
// refusal saves no checkpoint and so changes nothing about the next turn's inputs.
func TestStashRoomAnswersBeforeThePayloadIsBuilt(t *testing.T) {
	// A reserve of exactly ONE slot, so that a probe which claimed a slot would make the very
	// next probe answer differently. Sized at 2 for that reason: with room for two payloads a
	// claiming probe still reports room on the second call, and the assertion below would pass
	// against the bug it exists to catch.
	m := NewMemory(Options{MaxEntries: 2}) // reserve = 1
	payload := make([]byte, 64)
	if !m.StashRoom(len(payload)) {
		t.Fatal("StashRoom said an empty reserve was full")
	}
	// A probe must claim nothing, or two probes in a row would disagree for no reason.
	if !m.StashRoom(len(payload)) {
		t.Error("a second StashRoom probe said full: the probe consumed a slot, so a caller " +
			"that asks twice is told to skip work the store would have taken")
	}
	m.PutStash("aaaaaaaaaaaaaaa1", payload)
	if m.StashRoom(len(payload)) {
		t.Error("StashRoom said a full reserve had room; the model call it guards is paid and " +
			"discarded")
	}
	if m.StashStats().Refused != 0 {
		t.Error("a probe incremented the refusal counter: stash_refused must count removals " +
			"DECLINED, and a probe declines nothing")
	}
	// The probe must reclaim what the TTL has released, exactly as PutStash does — otherwise a
	// long-lived proxy skips work forever on the strength of payloads that expired hours ago.
	m.SetClock(func() time.Time { return time.Now().Add(2 * DefaultTTL) })
	if !m.StashRoom(len(payload)) {
		t.Error("StashRoom reported full after its payloads had expired: the probe's answer is " +
			"staler than PutStash's, so summarize skips checkpoints the store would accept")
	}
}
