package dash

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ibmSonnet is the rate ete-litellm actually bills for aws/claude-sonnet-5 — half the
// public API's, which is why the operator price list exists at all.
var ibmSonnet = modelinfo.Price{Input: 1.52e-6, Output: 7.6e-6, CacheRead: 1.52e-7, CacheWrite: 1.9e-6}

// The re-sent part of a removal is worth the cache-READ rate on a turn whose cache hit,
// and the cache-WRITE rate on a turn whose cache missed — because on a miss the provider
// re-billed the whole prompt, so the removed content would have been re-billed too.
//
// This is the correction with the largest effect on the real service: TTL-expired turns
// were 4% of production requests and 31% of spend, every token of it cache_creation, and
// their savings were being valued at a rate 12.5x too low.
func TestRepeatSavingsAreValuedByTheCacheOutcome(t *testing.T) {
	const removed, unique = 10_000, 0 // all of it re-sent history, none of it new

	mk := func(read, write int64) *Event {
		e := &Event{
			TS: 1000, SessionID: "s", Model: "aws/claude-sonnet-5",
			TokensBefore: 90_000, TokensAfter: 90_000 - removed,
			SavedUnique:  unique,
			FreshInput:   10, CacheRead: read, CacheWrite: write, OutputTokens: 50,
		}
		e.Price(ibmSonnet, true)
		return e
	}
	hit, miss := mk(80_000, 0), mk(0, 80_000)

	wantHit := float64(removed) * ibmSonnet.CacheRead
	wantMiss := float64(removed) * ibmSonnet.CacheWrite
	if got := hit.BaselineCostUSD - hit.CostUSD; math.Abs(got-wantHit) > 1e-12 {
		t.Errorf("cache HIT: removal valued at %.10f, want the read rate %.10f", got, wantHit)
	}
	if got := miss.BaselineCostUSD - miss.CostUSD; math.Abs(got-wantMiss) > 1e-12 {
		t.Errorf("cache MISS: removal valued at %.10f, want the write rate %.10f", got, wantMiss)
	}
	// The point of the change, stated as a ratio so a partial revert also fails.
	ratio := (miss.BaselineCostUSD - miss.CostUSD) / (hit.BaselineCostUSD - hit.CostUSD)
	if ratio < 10 {
		t.Errorf("a cold turn's removal is only %.1fx a warm turn's; the write/read spread is 12.5x", ratio)
	}
	// A turn that neither read nor wrote cache (a provider with no cache at all) values
	// the removal at the fresh rate, never at zero.
	none := mk(0, 0)
	if got := none.BaselineCostUSD - none.CostUSD; math.Abs(got-float64(removed)*ibmSonnet.Input) > 1e-12 {
		t.Errorf("no cache at all: removal valued at %.10f, want the fresh rate", got)
	}
}

// The prompt cache is a saving of its own, and the dashboard reported none of it.
func TestCacheSavedIsCacheReadsAgainstTheFreshRate(t *testing.T) {
	e := &Event{TS: 1, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
		FreshInput: 1_000, CacheRead: 200_000, CacheWrite: 0, OutputTokens: 100}
	e.Price(ibmSonnet, true)
	want := 200_000 * (ibmSonnet.Input - ibmSonnet.CacheRead)
	if math.Abs(e.CacheSavedUSD-want) > 1e-12 {
		t.Fatalf("cache_saved_usd = %.10f, want %.10f", e.CacheSavedUSD, want)
	}
	// It must never be part of the compaction counterfactual: this request removed
	// nothing, so its baseline equals its cost.
	if math.Abs(e.BaselineCostUSD-e.CostUSD) > 1e-15 {
		t.Fatalf("cache savings leaked into the baseline: %.10f vs %.10f", e.BaselineCostUSD, e.CostUSD)
	}
	// A request with no rates at all reports nothing rather than zero dollars saved.
	u := &Event{TS: 2, SessionID: "s", Model: "m", CacheRead: 200_000}
	u.Price(modelinfo.Price{}, true)
	if u.CacheSavedUSD != 0 || u.TokenAccounting == AccountingComplete {
		t.Fatalf("an unpriced request reported cache savings: %v / %s", u.CacheSavedUSD, u.TokenAccounting)
	}
}

// And the aggregate: the overview's headline must add the two savings without
// double-counting either, and must attribute the protected subset to the requests where
// one of our cache components actually acted.
func TestOverviewReportsBothSavings(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	for i := range 2 {
		e := &Event{TS: int64(1000 + i), SessionID: "s", Model: "aws/claude-sonnet-5",
			TokensBefore: 50_000, TokensAfter: 49_000, SavedUnique: 1_000,
			FreshInput: 10, CacheRead: 40_000, CacheWrite: 0, OutputTokens: 20}
		if i == 0 { // only the first request had cachesplit act on it
			e.Components = []CompRow{{Component: "cachesplit", Kind: "reformat", Acted: true, Mutated: true}}
		}
		e.Price(ibmSonnet, true)
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	perReq := 40_000 * (ibmSonnet.Input - ibmSonnet.CacheRead)
	if math.Abs(o.CacheSavedUSD-2*perReq) > 1e-12 {
		t.Errorf("cache_saved_usd = %.10f, want %.10f", o.CacheSavedUSD, 2*perReq)
	}
	if math.Abs(o.CacheSavedProtectedUSD-perReq) > 1e-12 {
		t.Errorf("cache_saved_protected_usd = %.10f, want one request's worth %.10f",
			o.CacheSavedProtectedUSD, perReq)
	}
	if math.Abs(o.TotalSavedUSD-(o.NetSavedUSD+o.CacheSavedUSD)) > 1e-15 {
		t.Errorf("total_saved_usd is not the sum of the two savings")
	}
	if o.NetSavedUSD >= o.TotalSavedUSD || o.NetSavedUSD <= 0 {
		t.Errorf("net %.10f vs total %.10f: the two savings must be separable", o.NetSavedUSD, o.TotalSavedUSD)
	}
	// Both are in the waterfall, and the cache step is not folded into the compaction one.
	steps := map[string]float64{}
	for _, s := range o.Waterfall {
		steps[s.Key] = s.DeltaUSD
	}
	if math.Abs(steps["cache_saved"]+o.CacheSavedUSD) > 1e-15 || math.Abs(steps["total_saved"]-o.TotalSavedUSD) > 1e-15 {
		t.Errorf("waterfall is missing the cache savings: %v", steps)
	}
}

// A new column must not cost the history. The version-bump path renames the file aside
// and discards every request row; on the live service that is thousands of rows to gain
// one column, so cache_saved_usd is added by ALTER TABLE on open instead. This opens a
// database that predates the column and checks both halves: the column appears, and the
// rows are still there.
func TestAdditiveColumnKeepsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{{TS: 1, SessionID: "s", Model: "m", TokensBefore: 10}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Drop the column back off to simulate the on-disk shape before this change.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN cache_saved_usd`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the column failed: %v", err)
	}
	defer db2.Close()
	var n int64
	if err := db2.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows survived the migration, want 1", n)
	}
	var sum float64
	if err := db2.sql.QueryRow(`SELECT COALESCE(SUM(cache_saved_usd),0) FROM requests`).Scan(&sum); err != nil {
		t.Fatalf("cache_saved_usd was not added: %v", err)
	}
}
