package dash

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Verification of the value pass against a REAL dashboard database, which is the only place
// most of these defects were visible at all: every one of them reads correct on a fixture with
// three requests in it and wrong on a corpus with fourteen thousand.
//
// Skipped unless CG_SNAPSHOT_DB names a copy of one. The file is copied to a temp dir before
// opening, because Open migrates and a snapshot someone else is reading must not be written to.
//
//	CG_SNAPSHOT_DB=/path/to/cg.db go test ./dash -run Snapshot -v
//
// The rates come out of the snapshot itself (snapshotPricer), so the numbers are the ones that
// deployment was actually billed at rather than a guess — see that function for the one
// assumption it makes and how it is checked.
func TestSnapshotValueNumbers(t *testing.T) {
	src := os.Getenv("CG_SNAPSHOT_DB")
	if src == "" {
		t.Skip("set CG_SNAPSHOT_DB to a copy of a dashboard database")
	}
	path := filepath.Join(t.TempDir(), "snap.db")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := snapshotPricer(t, db)
	f := Filter{TenantAll: true}

	o, err := db.Overview(f)
	if err != nil {
		t.Fatal(err)
	}
	tiers, err := db.TierCosts(f, p)
	if err != nil {
		t.Fatal(err)
	}
	o.SetTiers(tiers)

	t.Logf("requests %d  sessions %d", o.Requests, o.Sessions)
	t.Logf("saved: gross %d  unique %d  replay %d  projected %d  realized %.2f%%",
		o.SavedGross, o.SavedUnique, o.ReplayTokens, o.ReplayProjectedTokens, o.ReplayRealizedPct)
	for _, d := range o.Denominators {
		t.Logf("denominator %-14s %.3f%%  (%d / %d)", d.Key, d.Percent, d.Numerator, d.Denominator)
	}
	t.Logf("split: acted %d  moved %d  credited %d  credited+moved %d  $%.4f",
		o.SplitRequests, o.SplitTailMoved, o.SplitCredited, o.SplitCreditedMoved, o.CachesplitSavedUSD)
	t.Logf("prefix_change: after-mutation %d req $%.2f · all %d req $%.2f",
		o.PrefixChangeRequests, o.PrefixChangeCost, o.PrefixChangeRequestsAll, o.PrefixChangeCostAll)
	t.Logf("net saved $%.4f  total saved $%.4f  billed $%.2f", o.NetSavedUSD, o.TotalSavedUSD, o.CostUSD)
	if tiers != nil {
		t.Logf("tiers: fresh $%.2f  read $%.2f  write $%.2f  output $%.2f",
			tiers.FreshUSD, tiers.CacheReadUSD, tiers.CacheWriteUSD, tiers.OutputUSD)
		t.Logf("addressable $%.2f of $%.2f (%.1f%%)  billed $%.2f  unpriced %d req",
			tiers.AddressableUSD, tiers.TotalUSD, 100*tiers.AddressableUSD/tiers.TotalUSD,
			tiers.StoredUSD, tiers.Uncovered)
		t.Logf("saved of whole bill %.3f%%  of addressable %.3f%%",
			100*o.TotalSavedUSD/tiers.StoredUSD, 100*o.TotalSavedUSD/tiers.AddressableUSD)
		t.Logf("frozen %d tok: billed $%.2f as reads, avoided $%.2f of re-creation",
			o.FrozenTokens, tiers.FrozenReadUSD, tiers.FrozenWriteRiskUSD)
	}

	rows, err := db.Components(f)
	if err != nil {
		t.Fatal(err)
	}
	var stored float64
	for _, c := range rows {
		stored += c.SavedUSD
	}
	if err := db.EstimateComponentSavedUSD(f, p, rows); err != nil {
		t.Fatal(err)
	}
	var withEst float64
	for _, c := range rows {
		withEst += c.SavedUSD + c.SavedUSDEstimated
		t.Logf("%-12s runs %6d acted %5d struct %5d  gross %9d unique %8d  stored $%.4f "+
			"estimated $%8.4f over %6d rows (%d unpriceable)  net $%.4f",
			c.Component, c.Runs, c.ActedTokens, c.ActedStructural, c.SavedGross, c.SavedUnique,
			c.SavedUSD, c.SavedUSDEstimated, c.SavedUSDEstimatedRows, c.SavedUSDUnpricedRows,
			c.NetUSDWithEstimate)
	}
	t.Logf("components saved: stored $%.4f -> with the read-time valuation $%.4f", stored, withEst)

	// The estimate must not exceed the request-level saving it decomposes: per-component
	// dollars sum to baseline − billed by construction (TestPerComponentSavedUSDReconciles),
	// so a total above that would mean the read path is inventing value.
	if ceiling := o.BaselineCostUSD - o.CostUSD; withEst > ceiling*1.02 {
		t.Errorf("component dollars sum to $%.4f, above the request-level saving of $%.4f — "+
			"the read-time valuation is inflating", withEst, ceiling)
	}
	if o.SplitCredited < o.SplitCreditedMoved {
		t.Errorf("credited-and-moved (%d) exceeds credited (%d)", o.SplitCreditedMoved, o.SplitCredited)
	}
}

// snapshotPricer recovers each model's rates from the snapshot's own rows, so the verification
// prices the traffic at what it was billed rather than at whatever a live gateway quotes today.
//
// cache_saved_usd is stored as cache_read × (Input − CacheRead) (Event.Price), so one row with
// a cache read pins Input − CacheRead exactly. The remaining three tiers come from the
// provider's published ratios (read 0.1×, write 1.25×, output 5× input), which this corpus
// confirms exactly: two models are priced with no cache reads at all and solve to 5.000 /
// 0.500 / 6.250 / 25.000 and 1.000 / 0.100 / 1.250 / 5.000 per MTok. A model with no priced
// cache read is left unpriced, and shows up as TierCosts.Uncovered rather than as free.
func snapshotPricer(t *testing.T, db *DB) modelinfo.Pricer {
	t.Helper()
	rows, err := db.sql.Query(`SELECT model, cache_read, cache_saved_usd FROM requests
		WHERE cache_saved_usd > 0 AND cache_read > 0 GROUP BY model`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := staticTable{}
	for rows.Next() {
		var model string
		var read int64
		var saved float64
		if err := rows.Scan(&model, &read, &saved); err != nil {
			t.Fatal(err)
		}
		in := saved / (float64(read) * 0.9)
		out[model] = modelinfo.Price{Input: in, CacheRead: in * 0.1, CacheWrite: in * 1.25, Output: in * 5}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Skip("no priced cache reads in this snapshot; nothing to value against")
	}
	return out
}

type staticTable map[string]modelinfo.Price

func (s staticTable) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	p, ok := s[model]
	return p, ok
}
