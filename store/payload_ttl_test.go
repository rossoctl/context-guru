package store

import (
	"strconv"
	"testing"
	"time"
)

// The payload horizon (#190).
//
// #188 bounded the reserve but left a slot released ONLY by the TTL, and both namespaces shared
// one. The store is a single process-wide instance, so a busy period could hold the reserve
// saturated for the whole of ttl_seconds — ~2.8h at the default — refusing every removal for
// every session the entire time. The fix is a shorter horizon for the payloads alone, and what
// makes it safe is that a payload is re-derivable from the transcript while a frozen decision is
// not: see DefaultStashTTL, and TestAReclaimedPayloadIsReDerivedBeforeItsMarkerGoesUpstream in
// components/offload, which drives the re-derivation end to end.

// The split itself: a payload written alongside its decisions is reclaimed at the payload
// horizon, while those decisions — which nothing else holds a copy of — are still live.
func TestPayloadsExpireSoonerThanTheDecisionsThatNameThem(t *testing.T) {
	now := time.Unix(0, 0)
	// Explicit, and deliberately far apart: ttl_seconds is sized for a long-horizon task's idle
	// gaps, stash_ttl_seconds for one inter-turn gap. 20 entries so the reserve (max/2 = 10) is
	// what binds below rather than the shared exempt budget — this test is about the horizon, and
	// writeRemoval's five-entries-per-removal arithmetic is covered in stash_test.go.
	m := NewMemory(Options{MaxEntries: 20, TTLSeconds: 10000, StashTTLSeconds: 100})
	m.SetClock(func() time.Time { return now })

	// The two PINNED decision records one reversible removal writes, whose loss is the
	// cache-destructive event ttl_seconds is long for.
	m.Put(ResultPrefix+"sess:1", []byte("masked replacement bytes"))
	m.Put(XResultPrefix+"1", []byte("masked replacement bytes"))

	k1 := "aaaaaaaaaaaaaaa0"
	for i := 0; i < m.stashCap(); i++ { // saturate the reserve, as a busy period does
		if !m.PutStash("aaaaaaaaaaaaaaa"+strconv.Itoa(i), []byte("payload")) {
			t.Fatalf("filling an empty reserve was refused at %d", i)
		}
	}
	if m.PutStash("bbbbbbbbbbbbbbb1", []byte("one more")) {
		t.Fatal("a payload was accepted into a saturated reserve")
	}

	now = now.Add(101 * time.Second) // past the PAYLOAD horizon, nowhere near ttl_seconds

	// The slot frees, so the pipeline can promise reversibility again. This is the liveness #190
	// is about: under one shared TTL it would not arrive for another 9,899s.
	if !m.PutStash("bbbbbbbbbbbbbbb1", []byte("one more")) {
		t.Fatalf("the reserve released no slot at the payload horizon (%v): one busy period "+
			"still saturates it for the whole of ttl_seconds", m.stashTTL)
	}
	if _, live := m.Get(k1); live {
		t.Fatal("a payload past stash_ttl_seconds is still being served")
	}
	// And the half that must NOT have expired with it. A frozen decision is the replacement
	// bytes the provider already cached; losing one flips an already-cached message and re-writes
	// the suffix at ~11.5x the read price, which is the cost ttl_seconds is long for.
	for _, k := range []string{ResultPrefix + "sess:1", XResultPrefix + "1"} {
		if _, ok := m.Get(k); !ok {
			t.Fatalf("%s expired with the payload: the two horizons are not actually split, so "+
				"shortening the payload TTL now costs a cache-write per message", k)
		}
	}
}

// stash_expired reports two outcomes at once — a payload nobody wanted, and a payload an
// outstanding marker still needs — and those call for opposite responses. stash_revived is the
// benign half, counted at the one place that knows the entry was absent. Without it an operator
// watching a rising stash_expired cannot tell the shorter horizon working from it running ahead
// of the re-stash. Same argument as stash_refused vs stash_missing in #188.
func TestAReclaimedPayloadRewrittenByAReplayIsCountedAsRevived(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemory(Options{MaxEntries: 8, TTLSeconds: 10000, StashTTLSeconds: 100})
	m.SetClock(func() time.Time { return now })
	key := "aaaaaaaaaaaaaaa1"
	if !m.PutStash(key, []byte("original tool output")) {
		t.Fatal("the first stash was refused")
	}
	if st := m.StashStats(); st.Revived != 0 {
		t.Fatalf("a FIRST stash was counted as a revival (%d): the counter would report "+
			"reclamation being absorbed on a store that has never reclaimed anything", st.Revived)
	}

	now = now.Add(101 * time.Second)
	if _, live := m.Get(key); live {
		t.Fatal("the payload outlived stash_ttl_seconds")
	}

	// What a replay does: re-derive the payload from the message text the agent re-sent and write
	// it back under the same key (the key IS the content hash, so the marker's bytes are
	// unchanged and nothing flips).
	if !m.PutStash(key, []byte("original tool output")) {
		t.Fatal("re-stashing a reclaimed payload was refused")
	}
	st := m.StashStats()
	if st.Expired == 0 {
		t.Fatal("StashStats() reports 0 expired after a payload was reclaimed")
	}
	if st.Revived != 1 {
		t.Fatalf("stash_revived = %d, want 1: the reclamation was absorbed at no cost and "+
			"nothing says so, so stash_expired reads as a possible broken promise", st.Revived)
	}
	if _, ok := m.Get(key); !ok {
		t.Fatal("the revived payload does not resolve, so the marker really is dangling")
	}
	// Counted per RECLAMATION, not per write: a live payload refreshed every turn must not
	// inflate the figure, or "absorbed" stops being readable as a rate against expired.
	if !m.PutStash(key, []byte("original tool output")) {
		t.Fatal("refreshing a live payload was refused")
	}
	if st := m.StashStats(); st.Revived != 1 {
		t.Fatalf("stash_revived = %d after refreshing a LIVE payload, want 1", st.Revived)
	}
}

// A payload outliving the decision that names its marker is memory nothing can ever read: once
// the decision is gone, no replay stamps that marker again. So an operator who shortens
// ttl_seconds below the payload default must get the shorter of the two, not a reserve held open
// by dead payloads — which is the exact saturation #190 is about, arrived at by config.
func TestThePayloadHorizonNeverOutlivesTheDecisionHorizon(t *testing.T) {
	m := NewMemory(Options{TTLSeconds: 60}) // shorter than DefaultStashTTL
	if m.stashTTL > m.ttl {
		t.Fatalf("stashTTL %v exceeds ttl %v: dead payloads would hold reserve slots open for "+
			"%v after the last decision naming them expired", m.stashTTL, m.ttl, m.stashTTL-m.ttl)
	}
	if m.stashTTL != 60*time.Second {
		t.Fatalf("stashTTL %v, want the capped 60s", m.stashTTL)
	}
	// Still configurable in its own right, and still defaulted.
	if m2 := NewMemory(Options{StashTTLSeconds: 42}); m2.stashTTL != 42*time.Second {
		t.Fatalf("stash_ttl_seconds must stay configurable, got %v", m2.stashTTL)
	}
	if m3 := NewMemory(Options{}); m3.stashTTL != DefaultStashTTL {
		t.Fatalf("zero StashTTLSeconds should yield DefaultStashTTL, got %v", m3.stashTTL)
	}
}

// The config surface must report what the store WILL USE, not what was configured.
//
// The cap is applied silently inside NewMemory, so `/config` publishing the raw field showed
// stash_ttl_seconds: 20000 on the dashboard while the store used 10000 — an operator told one thing
// while another runs, which is the same silent divergence #200 is about, in the config surface
// instead of the metrics one. EffectiveStashTTLSeconds derives it from the same code path NewMemory
// uses, so the two cannot drift apart.
func TestTheConfigSurfaceReportsTheEffectivePayloadHorizon(t *testing.T) {
	// The case that diverged: a payload horizon longer than the decision horizon.
	if got := EffectiveStashTTLSeconds(Options{TTLSeconds: 10000, StashTTLSeconds: 20000}); got != 10000 {
		t.Errorf("reported %d, want the capped 10000: /config would advertise a horizon the store "+
			"does not use", got)
	}
	// And the cases that did not, which must keep working.
	if got := EffectiveStashTTLSeconds(Options{TTLSeconds: 10000, StashTTLSeconds: 600}); got != 600 {
		t.Errorf("reported %d, want the configured 600", got)
	}
	if got := EffectiveStashTTLSeconds(Options{}); got != int(DefaultStashTTL/time.Second) {
		t.Errorf("reported %d, want DefaultStashTTL in seconds", got)
	}
	// NO CROSS-CHECK AGAINST NewMemory HERE, deliberately. EffectiveStashTTLSeconds IS
	// `NewMemory(o).stashTTL`, so comparing the two is A == A: it cannot fail, and a reader
	// scanning for coverage would count it as evidence. The three assertions above are the whole
	// test on this side.
	//
	// The drift this helper exists to prevent is between the value /config PUBLISHES and the value
	// the store uses, and nothing in this package can span that — main.go could go back to
	// publishing the raw field with every test here still green. That assertion lives where it can
	// fail: cmd/context-guru-proxy.TestConfigPublishesTheEffectivePayloadHorizon.
}

// The constants' RELATIONSHIP is the design, so it is asserted rather than left to whoever next
// edits one of them: payloads short because a replay re-derives them, decisions long because
// nothing else holds the bytes the provider cached.
func TestDefaultPayloadHorizonIsWellInsideTheDecisionHorizon(t *testing.T) {
	if DefaultStashTTL >= DefaultTTL {
		t.Fatalf("DefaultStashTTL %v is not shorter than DefaultTTL %v: the reserve is still "+
			"held for a whole task horizon by one busy period (#190)", DefaultStashTTL, DefaultTTL)
	}
	// And not so short that an ordinary inter-turn gap — a test suite, a build, a long tool call
	// — outlives a payload whose marker is still live in the transcript.
	if DefaultStashTTL < 15*time.Minute {
		t.Fatalf("DefaultStashTTL %v is shorter than a long tool call, so a live marker's "+
			"payload can be reclaimed between two consecutive turns", DefaultStashTTL)
	}
}

// An entry that BECOMES a payload has its deadline moved EARLIER (ttl -> stashTTL), and
// nextExpiry — the lower bound that lets sweepExpired skip a pass — has to follow it down. A
// bound left above an entry's real expiry makes the sweep return early, and for a payload that
// means a reserve slot no TTL ever reclaims: the exact permanent saturation #190 is about,
// reintroduced by the fix for it.
//
// Sized so the become-a-payload write is the ONLY thing that can lower the bound: a one-slot
// reserve, so no second PutStash gets to seed it on the way in.
func TestBecomingAPayloadLowersTheSweepBound(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemory(Options{MaxEntries: 2, TTLSeconds: 10000, StashTTLSeconds: 100}) // reserve = 1
	m.SetClock(func() time.Time { return now })

	key := "aaaaaaaaaaaaaaa1"
	m.Put(key, []byte("written plain first, so nextExpiry is seeded at now+ttl"))
	if !m.PutStash(key, []byte("and now claimed as a payload")) {
		t.Fatal("claiming a present entry as a payload was refused")
	}
	if m.PutStash("bbbbbbbbbbbbbbb1", []byte("second")) {
		t.Fatal("a second payload was accepted into a one-slot reserve")
	}

	now = now.Add(101 * time.Second) // past the PAYLOAD horizon, far short of ttl_seconds
	if !m.PutStash("bbbbbbbbbbbbbbb1", []byte("second")) {
		t.Fatal("the sweep skipped an expired payload because nextExpiry still held the long " +
			"horizon, so this reserve slot is never reclaimed by any TTL")
	}
	if _, live := m.Get(key); live {
		t.Fatal("the expired payload is still being served")
	}
}
