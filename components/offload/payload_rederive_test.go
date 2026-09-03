package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE PROPERTY THAT MAKES THE SHORT PAYLOAD HORIZON SAFE (#190), driven end to end rather than
// asserted in a comment.
//
// store.DefaultStashTTL gives rewind payloads a fifth of ttl_seconds so a busy period cannot hold
// the reserve saturated for ~2.8h afterwards. That is only safe because a payload, unlike a frozen
// decision, is RE-DERIVABLE: every offloader replays its decision on every turn regardless of the
// cache-tail gate, and that replay re-stashes the payload from the message text the agent just
// re-sent. This test lets a payload be reclaimed and then checks the next turn puts it back —
// on the REQUEST path, which is what matters, because an expand call can only arrive in the
// RESPONSE to that same request.
//
// If this test ever fails, the shorter horizon is no longer safe and store.DefaultStashTTL must go
// back to DefaultTTL — not the other way round.
func TestAReclaimedPayloadIsReDerivedBeforeItsMarkerGoesUpstream(t *testing.T) {
	now := time.Unix(0, 0)
	// A payload horizon a fixture can cross, and a decision horizon it cannot: the decision must
	// survive, or this measures a lost freeze rather than a reclaimed payload.
	st := store.NewMemory(store.Options{TTLSeconds: 10000, StashTTLSeconds: 100})
	st.SetClock(func() time.Time { return now })
	m := maskFor(t)
	body := strings.Repeat("verbose tool output line\n", 30)

	turn := func(maxCached int) string {
		t.Helper()
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			tool(body), tool("new tail output"), // the agent re-sends the ORIGINAL every turn
		}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return schema.MessageText(req.Input[0])
	}

	// Turn 1: nothing is cached yet, so a NEW mask is allowed. It stashes the payload and stamps
	// the marker — the promise this test is about.
	first := turn(-1)
	if first == body {
		t.Fatal("turn 1 did not mask the older output; the fixture never made a promise")
	}
	keys := expand.ParseMarkers(first)
	if len(keys) != 1 {
		t.Fatalf("expected exactly one marker in the replacement, got %d", len(keys))
	}
	key := keys[0]
	if _, ok := expand.Resolve(st, key); !ok {
		t.Fatal("the marker did not resolve on the turn that stamped it")
	}

	// Let the payload cross its own horizon and force the reclamation. Reading it is what
	// enforces the TTL here, and it also makes the test NON-VACUOUS: PutStash's refresh branch
	// does not check expiry, so an expired-but-unswept entry would be resurrected in place and
	// this test would pass without ever exercising re-derivation.
	now = now.Add(101 * time.Second)
	if _, ok := expand.Resolve(st, key); ok {
		t.Fatal("the payload outlived stash_ttl_seconds, so nothing was reclaimed and the " +
			"re-derivation below is never exercised")
	}
	missingBefore := StashMissing()

	// Turn 2: the output now sits inside the provider's cached prefix, so only the frozen replay
	// can act — and the replay is the thing that re-stashes.
	second := turn(0)
	if second != first {
		t.Fatalf("turn 2 flipped representation (cache-destructive):\n want %q\n got  %q",
			first, second)
	}
	orig, ok := expand.Resolve(st, key)
	if !ok {
		t.Fatal("the marker on the wire does NOT resolve after its payload was reclaimed: the " +
			"replay did not re-derive it, so store.DefaultStashTTL is trading reversibility for " +
			"reserve liveness rather than getting both")
	}
	if orig != body {
		t.Fatalf("the re-derived payload is not the original content:\n want %q\n got  %q",
			body, orig)
	}
	// The re-stash succeeded, so no dangling marker was reported — the counter and the resolve
	// have to agree, or one of them is lying about the same event.
	if got := StashMissing() - missingBefore; got != 0 {
		t.Fatalf("stash_missing advanced by %d on a replay that DID resolve", got)
	}
	// And the store booked the reclamation as absorbed, which is the counter an operator reads to
	// know the shorter horizon is costing nothing.
	if sst := st.StashStats(); sst.Revived != 1 || sst.Expired != 1 {
		t.Fatalf("stash_expired=%d stash_revived=%d, want 1 and 1: the pair is what says a "+
			"reclamation was absorbed rather than broken", sst.Expired, sst.Revived)
	}
}

// The same path when the reserve is FULL at replay time, which is the case the re-derivation
// cannot rescue — and the one that must still be reported rather than silent. It is why
// stash_missing's remedy is the reserve first and stash_ttl_seconds second: a reclaimed payload
// only dangles if the write that would have restored it was also refused.
func TestAReclaimedPayloadThatCannotBeReStashedIsReportedMissing(t *testing.T) {
	now := time.Unix(0, 0)
	// A one-slot reserve (max/2), so another session's payload can hold it against the replay.
	st := store.NewMemory(store.Options{MaxEntries: 2, TTLSeconds: 10000, StashTTLSeconds: 100})
	st.SetClock(func() time.Time { return now })
	m := maskFor(t)
	body := strings.Repeat("verbose tool output line\n", 30)

	turn := func(maxCached int) string {
		t.Helper()
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			tool(body), tool("new tail output"),
		}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return schema.MessageText(req.Input[0])
	}

	first := turn(-1)
	keys := expand.ParseMarkers(first)
	if len(keys) != 1 {
		t.Fatalf("expected one marker, got %d", len(keys))
	}
	now = now.Add(101 * time.Second)
	if _, ok := expand.Resolve(st, keys[0]); ok {
		t.Fatal("the payload outlived stash_ttl_seconds")
	}
	// Take the freed slot before the replay can have it.
	if !st.PutStash("bbbbbbbbbbbbbbb1", []byte("another session's payload")) {
		t.Fatal("could not occupy the reserve")
	}
	missingBefore := StashMissing()

	second := turn(0)
	// The replay proceeds anyway — declining would flip an already-cached message and cannot
	// un-send a marker that went out a turn ago. See commitRefresh.
	if second != first {
		t.Fatalf("the replay was declined and the message flipped:\n want %q\n got  %q", first, second)
	}
	if got := StashMissing() - missingBefore; got != 1 {
		t.Fatalf("stash_missing advanced by %d, want 1: a dangling marker went upstream and the "+
			"counter an operator alerts on did not move", got)
	}
}
