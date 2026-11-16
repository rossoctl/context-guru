package keepalive

import (
	"context"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Prices for the refresh-cost tests. The recorded prefix is 48576 + 1200 = 49776 tokens, so:
//
//	ping    = 49776 x 1e-6 + 1 x 1e-5   = $0.04978
//	summary = 49776 x 1e-6 + 2000 x 1e-5 = $0.06978
//	extra   = summary - ping             = $0.01999
//
// A tail of about 1,300 tokens (tailFiller(100)) is worth about $0.039 at 3e-5: above the extra
// cost, below the whole call. That is the gap #1144 opens.
func refreshExtraPrices() proxyPricer {
	return proxyPricer{fixedPrice{modelinfo.Price{CacheRead: 1e-6, Output: 1e-5, CacheWrite: 3e-5}}}
}

// registerRefresh seeds a reserve and registers a refresh candidate with tailText as the new
// material. fullCost sets the component's refresh_cost: full.
func registerRefresh(t *testing.T, session string, st store.Store, model *fakeSummaryModel, tailText string, fullCost bool) {
	t.Helper()
	prefixMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&prefixMsg, "fix the failing tests, there is a lot of context here")
	tailMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant}
	schema.SetMessageText(&tailMsg, tailText)
	span := []bschemas.ChatMessage{prefixMsg, tailMsg}
	offload.SeedReservedCheckpointForTest(session, st, span[:1], "<summary>explored the handler</summary>")
	ask := append([]bschemas.ChatMessage(nil), span...)
	ask = append(ask, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser})
	call := func(ctx context.Context) (string, error) { return model.CompleteMessages(ctx, "", ask) }
	if fullCost {
		offload.RegisterKeepAliveCandidateWithRefreshCostForTest(session, st, call, "messages", span, 1,
			components.CacheStatePreExpiry, 60)
	} else {
		offload.RegisterKeepAliveCandidateForTest(session, st, call, "messages", span, 1,
			components.CacheStatePreExpiry, 60)
	}
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })
}

// runRefreshPing records a turn and sweeps one due ping. It returns after the ping or the
// substitute has finished.
func runRefreshPing(t *testing.T, k *Keeper, pol Policy, clock *testClock) {
	t.Helper()
	recordOne(t, k, pol, kaToolBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
}

// A tail between the new threshold (extra cost) and the old one (whole call) refreshes now.
func TestRefreshOnExtraCostFiresBetweenTheNewAndOldThreshold(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(refreshExtraPrices())
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>refreshed</summary>"}
	registerRefresh(t, "sess-1", st, model, tailFiller(100), false)
	runRefreshPing(t, k, kaPolicy(), clock)
	if k.summarySubstituteRefreshPaid.Load() != 1 || model.calls.Load() != 1 || fs.n() != 0 {
		t.Fatalf("paid=%d calls=%d pings=%d, want 1, 1, 0: a tail worth more than the extra cost must refresh",
			k.summarySubstituteRefreshPaid.Load(), model.calls.Load(), fs.n())
	}
}

// A tail worth less than the extra cost still pings.
func TestRefreshOnExtraCostStillPingsBelowTheNewThreshold(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(refreshExtraPrices())
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>never</summary>"}
	registerRefresh(t, "sess-1", st, model, tailFiller(2), false)
	runRefreshPing(t, k, kaPolicy(), clock)
	if k.summarySubstituteRefreshNotWorthIt.Load() != 1 || model.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("notWorthIt=%d calls=%d pings=%d, want 1, 0, 1",
			k.summarySubstituteRefreshNotWorthIt.Load(), model.calls.Load(), fs.n())
	}
}

// refresh_cost: full restores the old decision: the same tail now pings.
func TestRefreshCostFullKeepsTheOldDecision(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(refreshExtraPrices())
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>never</summary>"}
	registerRefresh(t, "sess-1", st, model, tailFiller(100), true)
	runRefreshPing(t, k, kaPolicy(), clock)
	if k.summarySubstituteRefreshNotWorthIt.Load() != 1 || model.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("notWorthIt=%d calls=%d pings=%d, want 1, 0, 1: refresh_cost full weighs the whole call",
			k.summarySubstituteRefreshNotWorthIt.Load(), model.calls.Load(), fs.n())
	}
}

// The spending cap still uses the whole call, also on a refresh whose saving clears the extra cost.
func TestRefreshCapStillUsesTheFullCost(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(refreshExtraPrices())
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>never</summary>"}
	registerRefresh(t, "sess-1", st, model, tailFiller(100), false)
	pol := kaPolicy()
	pol.MaxUSDPerPing = 0.06 // ping $0.0498 fits; whole call $0.0698 does not; extra $0.02 would
	runRefreshPing(t, k, pol, clock)
	if k.summarySubstituteOverBudget.Load() != 1 || model.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("overBudget=%d calls=%d pings=%d, want 1, 0, 1",
			k.summarySubstituteOverBudget.Load(), model.calls.Load(), fs.n())
	}
}
