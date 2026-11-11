package dash

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The dashboard reads the operator's compaction_point from the MODEL_PRICES table, as it reads the operator's
// window. Without the field it reads the bootstrap table, exactly as before.

func operatorTable(t *testing.T, withPoint bool) modelinfo.Resolver {
	t.Helper()
	y := "models:\n  - {match: \"m*\", in: 1.0, out: 4.0, window: 1000000}\n"
	if withPoint {
		y = "models:\n  - {match: \"m*\", in: 1.0, out: 4.0, window: 1000000, compaction_point: 835000}\n"
	}
	tb, err := modelinfo.ParseTable([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return modelinfo.Chain{tb}
}

func TestOperatorPointFeedsTheCompactEpisodeSpan(t *testing.T) {
	rows := []compactRow{row(1, 1_000, fresh)}
	with := walkCompactEpisodesWith(rows, exactWindow, operatorPoint(context.Background(), operatorTable(t, true)), testPrice, 0, 0.50, 0)
	if len(with.Assumptions.ClientCeilings) != 1 {
		t.Fatalf("ceilings = %+v", with.Assumptions.ClientCeilings)
	}
	c := with.Assumptions.ClientCeilings[0]
	if math.Abs(c.Frac-0.835) > 1e-9 || c.Provenance != string(compactionpoint.Operator) {
		t.Fatalf("ceiling = %+v, want the operator's 835000 of 1,000,000 (0.835)", c)
	}
}

func TestNoOperatorPointLeavesTheCompactEpisodeAsBefore(t *testing.T) {
	rows := []compactRow{row(1, 1_000, fresh), row(2, 2_000, fresh)}
	old := walkCompactEpisodes(rows, exactWindow, testPrice, 0.1, 0.5, 0)
	now := walkCompactEpisodesWith(rows, exactWindow, operatorPoint(context.Background(), operatorTable(t, false)), testPrice, 0.1, 0.5, 0)
	if a, b := old.Assumptions.ClientCeilings, now.Assumptions.ClientCeilings; len(a) != len(b) || (len(a) == 1 && a[0] != b[0]) {
		t.Fatalf("ceilings differ with no field set: %+v vs %+v", a, b)
	}
	if len(old.Episodes) != len(now.Episodes) {
		t.Fatalf("episodes differ with no field set: %d vs %d", len(old.Episodes), len(now.Episodes))
	}
}

func TestOperatorPointFeedsTheReplayCapCeiling(t *testing.T) {
	rows := []capRow{capTestRow(1, 100_000, 50_000, 400_000, "extract_llm", 1)}
	with := walkReplayCapWith(rows, fixedWindow(1_000_000), operatorPoint(context.Background(), operatorTable(t, true)))
	if got := with.CeilingFrac["m1"]; math.Abs(got-0.835) > 1e-9 {
		t.Fatalf("ceiling = %v, want 0.835 from the operator's compaction_point", got)
	}
}

func TestNoOperatorPointLeavesTheReplayCapAsBefore(t *testing.T) {
	rows := []capRow{
		capTestRow(1, 100_000, 50_000, 400_000, "extract_llm", 1),
		capTestRow(2, 110_000, 50_000, 450_000, "extract_llm", 1),
		capTestRow(3, 120_000, 500_000, 480_000, "extract_llm", 1),
		capTestRow(4, 130_000, 500_000, 500_000, "extract_llm", 1),
	}
	old := walkReplayCap(rows, fixedWindow(1_000_000))
	now := walkReplayCapWith(rows, fixedWindow(1_000_000), operatorPoint(context.Background(), operatorTable(t, false)))
	if old.CeilingFrac["m1"] != now.CeilingFrac["m1"] || old.DeltaUSD() != now.DeltaUSD() ||
		old.ClosedCounterfactual != now.ClosedCounterfactual {
		t.Fatalf("replay cap differs with no field set: %+v vs %+v", old, now)
	}
}

// The cap closes at the operator's ceiling: a stated 835000 ends the credit before the 1.00 default does.
func TestOperatorPointMovesWhereTheReplayCapCloses(t *testing.T) {
	rows := []capRow{
		capTestRow(1, 100_000, 50_000, 400_000, "extract_llm", 1), // cf 450k
		capTestRow(2, 110_000, 50_000, 800_000, "extract_llm", 1), // cf 850k: past 835k, under 1M
	}
	def := walkReplayCap(rows, fixedWindow(1_000_000))
	op := walkReplayCapWith(rows, fixedWindow(1_000_000), operatorPoint(context.Background(), operatorTable(t, true)))
	if def.ClosedCounterfactual != 0 || op.ClosedCounterfactual != 1 {
		t.Fatalf("closed at the default %d, at the operator's %d; want 0 and 1", def.ClosedCounterfactual, op.ClosedCounterfactual)
	}
}

// operatorWindows is testWindows plus a compaction point, as a MODEL_PRICES table would state it.
type operatorWindows struct {
	testWindows
	point int
}

func (o operatorWindows) CompactionPoint(context.Context, string) (int, bool) {
	return o.point, o.point > 0
}

// Through SQL, the replay ledger takes the operator's point: the same rows close earlier with a stated 835000.
func TestReplayLedgerThroughSQLUsesTheOperatorPoint(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, before, after int, fresh, read int64) *Event {
		e := mkEvent(ts, "s", "m1", before, after)
		e.TenantID = "t1"
		e.FreshInput, e.CacheRead = fresh, read
		e.CacheMissReason = CacheHit
		e.TokenAccounting = AccountingComplete
		e.Components = []CompRow{{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: before - after, SavedUnique: 0, SavedUSD: 0.5}}
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(DayMs+1, 300_000, 200_000, 1_000, 300_000),
		mk(DayMs+2, 310_000, 200_000, 1_000, 700_000), // cf ~ 811k: under 835k, closed at nothing
		mk(DayMs+3, 320_000, 100_000, 1_000, 700_000), // cf ~ 921k: past 835k, under 1M
	}); err != nil {
		t.Fatal(err)
	}
	def, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	op, err := db.ReplayLedgerFor(Filter{TenantAll: true}, operatorWindows{point: 835_000}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if def.ClosedCounterfactual != 0 || op.ClosedCounterfactual != 1 || op.CeilingFrac["m1"] != 0.835 {
		t.Fatalf("default closed %d, operator closed %d (ceiling %v); want 0 and 1 (0.835)",
			def.ClosedCounterfactual, op.ClosedCounterfactual, op.CeilingFrac["m1"])
	}
}
