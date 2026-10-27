package dash

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// Tests for the warm credit (#491): a warm drop is credited at the READ rate, and the rewrite it forced
// is charged on the request that paid it.

// warmDropEvent is a warm request that applied a drop removing `removed` tokens and rewrote
// `rewrite` tokens; the provider reported `written` cache-write tokens.
func warmDropEvent(removed, rewrite int, written int64) *Event {
	e := &Event{TS: 5, TenantID: "t", SessionID: "s", Model: "claude-sonnet-5", Status: 200,
		TokensBefore: 100_000, TokensAfter: 100_000 - removed, SavedUnique: removed,
		FreshInput: 50, CacheRead: 90_000, CacheWrite: written, OutputTokens: 500, CacheMissReason: CacheHit,
		Components: []CompRow{{Component: SweepComponent, Kind: "offload", Acted: true, Mutated: true,
			SavedGross: removed, SavedUnique: removed,
			Events: map[string]int{"sweep_applied_warm": 3, "sweep_warm_rewrite_tokens": rewrite}}},
	}
	e.Price(sweepPrice, true)
	return e
}

// netOver is the warm request's own credit plus the saving on `turns` later replays at the read rate.
func netOver(e *Event, removed int, turns int) float64 {
	return e.Components[0].SavedUSD + float64(removed)*sweepPrice.CacheRead*float64(turns)*factorOf()
}

func factorOf() float64 { f, _ := billedFactorForTest(); return f }

func TestAWarmDropIsCreditedAtTheReadRateNotTheWriteRate(t *testing.T) {
	const removed = 10_000
	e := warmDropEvent(removed, 0, 0)
	want := float64(removed) * sweepPrice.CacheRead * factorOf()
	if got := e.Components[0].SavedUSD; !approx(got, want) {
		t.Fatalf("warm credit = %v, want removed x READ rate x factor = %v", got, want)
	}
}

func TestTheRewriteIsChargedOnTheWarmRequestThatPaidIt(t *testing.T) {
	const removed, rewrite = 10_000, 60_000
	e := warmDropEvent(removed, rewrite, 0)
	credit := float64(removed) * sweepPrice.CacheRead * factorOf()
	charge := float64(rewrite) * (sweepPrice.CacheWrite - sweepPrice.CacheRead)
	if got := e.Components[0].SavedUSD; !approx(got, credit-charge) {
		t.Fatalf("warm row = %v, want credit %v minus rewrite charge %v", got, credit, charge)
	}
	// The provider's own write wins when it is smaller than the estimate.
	e2 := warmDropEvent(removed, rewrite, 20_000)
	charge2 := 20_000 * (sweepPrice.CacheWrite - sweepPrice.CacheRead)
	if got := e2.Components[0].SavedUSD; !approx(got, credit-charge2) {
		t.Fatalf("with a reported write: row = %v, want %v", got, credit-charge2)
	}
}

// The net sign: a drop the gate accepts is positive over its expected turns, and the same drop with too
// few turns is negative. A gate bug shows here, not hidden by the credit.
func TestTheNetOfAWarmDropIsPositiveWhenItPaysAndNegativeWhenItDoesNot(t *testing.T) {
	const removed, rewrite = 10_000, 60_000
	e := warmDropEvent(removed, rewrite, 0)
	if net := netOver(e, removed, 60); net <= 0 {
		t.Errorf("net over 60 turns = %v, want positive", net)
	}
	if net := netOver(e, removed, 3); net >= 0 {
		t.Errorf("net over 3 turns = %v, want negative", net)
	}
}

func billedFactorForTest() (float64, bool) { return tokens.BilledDeltaFactor("claude-sonnet-5") }

// The DB-side warm false-drop rate counts expands as well as references, and an old database migrates.
func TestTheWarmFalseDropRateCountsExpandsAsWellAsReferences(t *testing.T) {
	db := openTestDB(t)
	warm := warmDropEvent(1000, 0, 0)
	warm.Components[0].Events = map[string]int{"sweep_applied_warm": 4, "sweep_false_ref_warm": 1}
	later := &Event{TS: 9, TenantID: "t", SessionID: "s", Model: "claude-sonnet-5", Status: 200,
		SweepExpands: 2, SweepExpandTokens: 500, SweepFalseExpands: 2, SweepFalseExpandsWarm: 1}
	later.Price(sweepPrice, true)
	if err := db.insertBatch([]*Event{warm, later}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true}, staticPricer{sweepPrice})
	if err != nil {
		t.Fatal(err)
	}
	if l.AppliedWarm != 4 || l.ReferencedWarm != 1 || l.FalseExpandsWarm != 1 {
		t.Fatalf("ledger = %+v, want 4 applied, 1 referenced and 1 expanded warm", l)
	}
	if l.FalseDropsWarm != 2 || !approx(l.FalseDropRateWarm, 0.5) {
		t.Fatalf("warm false drops = %d rate %v, want 2 and 0.5 (references plus expands over applied)", l.FalseDropsWarm, l.FalseDropRateWarm)
	}
	if l.FalseExpands != 2 {
		t.Errorf("the all-kinds expand count = %d, want it unchanged at 2", l.FalseExpands)
	}
	got, err := db.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got.Requests {
		if r.SweepFalseExpandsWarm == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the column did not round-trip through the request row")
	}
}

func TestTheWarmExpandColumnIsAdditive(t *testing.T) {
	for _, c := range additiveColumns {
		if c.table == "requests" && c.column == "sweep_false_expands_warm" {
			if c.ddl != "INTEGER NOT NULL DEFAULT 0" {
				t.Fatalf("ddl = %q, an additive column needs a default so old rows read 0", c.ddl)
			}
			return
		}
	}
	t.Fatal("sweep_false_expands_warm is not in additiveColumns, so an old database would not migrate")
}
