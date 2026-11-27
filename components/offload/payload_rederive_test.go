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
// decision, is RE-DERIVABLE: extract and dedup redo their rewrite from the original the agent
// re-sends on every turn, and that write re-stashes the payload. This test lets a payload be reclaimed and then checks the next turn puts it back —
// on the REQUEST path, which is what matters, because an expand call can only arrive in the
// RESPONSE to that same request.
//
// If this test ever fails, the shorter horizon is no longer safe and store.DefaultStashTTL must go
// back to DefaultTTL — not the other way round.
// extractForRederive builds `extract` with a floor the fixture clears.
func extractForRederive(t *testing.T) components.Offload {
	t.Helper()
	c, err := newExtract([]byte("min_tokens: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c.(components.Offload)
}

func TestAReclaimedPayloadIsReDerivedBeforeItsMarkerGoesUpstream(t *testing.T) {
	now := time.Unix(0, 0)
	// A payload horizon a fixture can cross, and a decision horizon it cannot: the decision must
	// survive, or this measures a lost freeze rather than a reclaimed payload.
	st := store.NewMemory(store.Options{TTLSeconds: 10000, StashTTLSeconds: 100})
	st.SetClock(func() time.Time { return now })
	m := extractForRederive(t)
	body := strings.Repeat("verbose tool output line\n", 30)

	turn := func(maxCached int) string {
		t.Helper()
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("go"), tool(body), tool("tail"), // the agent re-sends the ORIGINAL every turn
		}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return schema.MessageText(req.Input[1])
	}

	// Turn 1: nothing is cached yet, so a NEW cut is allowed. It stashes the payload and stamps
	// the marker — the promise this test is about.
	first := turn(-1)
	if first == body {
		t.Fatal("turn 1 did not cut the output; the fixture never made a promise")
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

	// Turn 2: the output now sits inside the provider's cached prefix, so only the re-derived
	// rewrite can act — and that rewrite is the thing that re-stashes.
	second := turn(1)
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

// The same path when the reserve is FULL at re-derivation time. `extract` has no frozen replay: it
// redoes the cut from the re-sent original, and that write goes through the refusable commitMark. A
// refused write makes it decline, so the message goes upstream verbatim and NO marker dangles. The
// cost is a representation flip and a stash_refused count, not a stash_missing count. See the
// comment on store.DefaultStashTTL.
func TestAReclaimedPayloadThatCannotBeReStashedIsDeclinedNotDangling(t *testing.T) {
	now := time.Unix(0, 0)
	// A one-slot reserve (max/2), so another session's payload can hold it against the re-derivation.
	st := store.NewMemory(store.Options{MaxEntries: 2, TTLSeconds: 10000, StashTTLSeconds: 100})
	st.SetClock(func() time.Time { return now })
	m := extractForRederive(t)
	body := strings.Repeat("verbose tool output line\n", 30)

	turn := func(maxCached int) string {
		t.Helper()
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("go"), tool(body), tool("tail"),
		}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return schema.MessageText(req.Input[1])
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
	// Take the freed slot before the re-derivation can have it.
	if !st.PutStash("bbbbbbbbbbbbbbb1", []byte("another session's payload")) {
		t.Fatal("could not occupy the reserve")
	}
	missingBefore, refusedBefore := StashMissing(), StashRefusals()

	second := turn(1)
	if second != body {
		t.Fatalf("the cut was written although its payload could not be stored:\n got %q", second)
	}
	if len(expand.ParseMarkers(second)) != 0 {
		t.Fatal("a marker went upstream with no payload behind it")
	}
	if got := StashRefusals() - refusedBefore; got == 0 {
		t.Fatal("the refused write did not reach stash_refused, the counter an operator reads")
	}
	if got := StashMissing() - missingBefore; got != 0 {
		t.Fatalf("stash_missing advanced by %d, but no marker went upstream", got)
	}
}
