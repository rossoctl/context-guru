package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/store"
)

// PR #402 review, finding 2: cache_aware_summarizer's keep-alive candidate registry
// (offload's own keepAliveCand map) was never drained when the KEEPER itself stopped tracking a
// session. offload.ClearKeepAliveCandidate always existed and always worked when called — the
// bug was that nothing on the keeper's own exit paths (retire, forget, evictLocked) ever called
// it, so a session's entry outlived the session itself. After offload.maxKeepAliveCandidates
// (2,048) sessions had EVER registered, every later session's registration was refused, forever
// — a proxy that stayed up long enough stopped substituting keep-alive pings with summaries
// permanently, with no counter distinguishing it from "no session here has this component in
// its pipeline" (the overwhelmingly common, benign reason the same refusal looks identical to).
//
// Each sub-test below drives one of the keeper's own removal paths and checks that the
// candidate registry agrees the session is gone. None of them would have passed before
// keeper.retire/forget/evictLocked were given their own calls to
// offload.ClearKeepAliveCandidate.

// candCall is a minimal summaryCaller-shaped function for RegisterKeepAliveCandidateForTest —
// never actually invoked by anything these tests exercise, so its body does not matter.
func candCall(ctx context.Context) (string, error) { return "<summary>unused</summary>", nil }

func TestKeeperRetireDrainsTheKeepAliveCandidateRegistry(t *testing.T) {
	const tenant, session = "it-retire-tenant", "it-retire-session"
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })

	st := store.NewMemory(store.Options{})
	offload.RegisterKeepAliveCandidateForTest(session, st, candCall, "messages", nil, 0, "", 0)
	if _, _, reason, ok := offload.KeepAliveSubstitute(session); !ok {
		t.Fatalf("candidate did not register in the first place (reason=%q)", reason)
	}

	k := newKeeper(&Handler{})
	key := kaKey(tenant, session)
	k.live[key] = &kaEntry{tenant: tenant, session: session, startedAt: time.Now()}
	k.retire(key)

	if _, _, reason, ok := offload.KeepAliveSubstitute(session); ok {
		t.Fatalf("the candidate survived keeper.retire (reason=%q) — retire must drop it the same "+
			"moment it drops the kaEntry, or the registry never drains a session the keeper itself "+
			"has stopped tracking", reason)
	}
}

func TestKeeperForgetDrainsTheKeepAliveCandidateRegistry(t *testing.T) {
	const tenant, session = "it-forget-tenant", "it-forget-session"
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })

	st := store.NewMemory(store.Options{})
	offload.RegisterKeepAliveCandidateForTest(session, st, candCall, "messages", nil, 0, "", 0)

	k := newKeeper(&Handler{})
	key := kaKey(tenant, session)
	k.live[key] = &kaEntry{tenant: tenant, session: session, startedAt: time.Now()}
	k.forget(tenant)

	if _, _, reason, ok := offload.KeepAliveSubstitute(session); ok {
		t.Fatalf("the candidate survived keeper.forget (reason=%q) — a tenant deletion/revocation "+
			"must drop every candidate the keeper held for it, not only the kaEntry", reason)
	}
}

func TestKeeperEvictionDrainsTheKeepAliveCandidateRegistry(t *testing.T) {
	const tenant, session = "it-evict-tenant", "it-evict-session"
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })

	st := store.NewMemory(store.Options{})
	offload.RegisterKeepAliveCandidateForTest(session, st, candCall, "messages", nil, 0, "", 0)

	k := newKeeper(&Handler{})
	key := kaKey(tenant, session)
	// evictLocked picks the entry whose startedAt is the MOST RECENT — the longest still to
	// wait before its own idle deadline, so the least imminent value (see its own comment). This
	// session's entry is given the newest startedAt of the whole set so it is deterministically
	// the one evictLocked's one eviction (one entry over maxKeepAliveSessions) picks.
	now := time.Now()
	k.live[key] = &kaEntry{tenant: tenant, session: session, startedAt: now}
	k.live[kaKey("other-tenant", "other-session")] = &kaEntry{
		tenant: "other-tenant", session: "other-session", startedAt: now.Add(-time.Hour)}
	for i := 0; i < maxKeepAliveSessions; i++ {
		s := fmt.Sprintf("%s-filler-%d", session, i)
		k.live[kaKey("filler-tenant", s)] = &kaEntry{
			tenant: "filler-tenant", session: s, startedAt: now.Add(-time.Hour)}
	}
	k.evictLocked()

	if _, _, reason, ok := offload.KeepAliveSubstitute(session); ok {
		t.Fatalf("the evicted entry's candidate survived keeper.evictLocked (reason=%q)", reason)
	}
}

// TestKeepAliveRegistryAcceptsALiveSessionAfterManyEndedOnesDrain is the reviewer's own probe
// (PR #402 review, Technical notes): 2,048 sessions register and then end — simulated here with
// offload.ClearKeepAliveCandidate, which is what each of the three tests above just proved
// keeper.retire/forget/evictLocked now call on every real exit — and a live session's own
// registration right after must still succeed. Before this PR, nothing on any of the keeper's
// exit paths ever called ClearKeepAliveCandidate, so 2,048 sessions that had EVER registered
// (ended or not) filled the registry permanently; this test's own two steps are what the fix
// makes true in sequence rather than in isolation.
func TestKeepAliveRegistryAcceptsALiveSessionAfterManyEndedOnesDrain(t *testing.T) {
	st := store.NewMemory(store.Options{})
	for i := 0; i < 2048; i++ {
		session := fmt.Sprintf("ended-session-%d", i)
		offload.RegisterKeepAliveCandidateForTest(session, st, candCall, "messages", nil, 0, "", 0)
		offload.ClearKeepAliveCandidate(session)
	}
	const live = "it-live-after-drain"
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(live) })
	offload.RegisterKeepAliveCandidateForTest(live, st, candCall, "messages", nil, 0, "", 0)
	if _, _, reason, ok := offload.KeepAliveSubstitute(live); !ok {
		t.Fatalf("a live session could not register after 2,048 ended sessions drained (reason=%q) "+
			"— the registry stayed full on the strength of sessions that no longer exist", reason)
	}
}
