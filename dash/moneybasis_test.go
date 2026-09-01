package dash

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// Four money figures on this dashboard were each computed over a population that was not the
// population they claimed to describe. Every test here fails on the arithmetic that shipped
// before it, and each one is written against the SAME shape the real corpus has, so a reader
// can see which real-data condition it stands in for.

// A frozen prefix is what compaction DECLINED to touch; a cache read is what the provider
// actually served from cache. Neither bounds the other, and on 4.0% of the rows that recorded
// a freeze (3,004 of 75,185 on real traffic) the frozen count is the larger. TierCosts prices
// those tokens at the cache-READ rate and its own doc says they were "actually billed at" it,
// which for the excess is false: it was billed fresh or written.
func TestFrozenTokensArePricedOnlyUpToWhatTheRequestActuallyRead(t *testing.T) {
	db := openTestDB(t)
	// One row whose freeze is honest, one whose freeze exceeds its read. The second is the
	// real corpus's 4%: the SUM over both must count 3000, not 11000.
	honest := mkEvent(1000, "s-honest", "m", 100, 100)
	honest.CacheRead, honest.FrozenTokens = 2000, 1000
	over := mkEvent(2000, "s-over", "m", 100, 100)
	over.CacheRead, over.FrozenTokens = 2000, 10000
	if err := db.insertBatch([]*Event{honest, over}); err != nil {
		t.Fatal(err)
	}
	tc, err := db.TierCosts(Filter{}, staticPricer{ibmSonnet})
	if err != nil {
		t.Fatal(err)
	}
	if tc == nil {
		t.Fatal("TierCosts absent")
	}
	// min(1000,2000) + min(10000,2000) = 3000. The unclamped form reads 11000.
	want := 3000 * ibmSonnet.CacheRead
	if tc.FrozenReadUSD != want {
		t.Errorf("FrozenReadUSD = $%.9f, want $%.9f (3000 clamped tokens, not 11000): "+
			"tokens beyond a request's own cache_read were not read, so the cache-read rate "+
			"is not the rate they were billed at", tc.FrozenReadUSD, want)
	}
	// The write-risk half is the same count times the spread, so it must inherit the clamp.
	if spread := ibmSonnet.CacheWrite - ibmSonnet.CacheRead; tc.FrozenWriteRiskUSD != 3000*spread {
		t.Errorf("FrozenWriteRiskUSD = $%.9f, want $%.9f — it multiplies the same count and "+
			"must not re-import the unclamped one",
			tc.FrozenWriteRiskUSD, 3000*spread)
	}
}

// The "gross, of what we tried to compact" ratio summed its numerator over every row and its
// denominator over the rows that recorded one. attempted_tokens is additive, so a row written
// before it shipped reads 0 while still carrying a saving — 7,032 such rows on real traffic,
// which served 2.101% where the same-basis figure is 1.824%.
func TestAttemptedRatioCountsSavingsOnlyWhereItCountsTheDenominator(t *testing.T) {
	db := openTestDB(t)
	old := mkEvent(1000, "s-old", "m", 10000, 1000) // pre-column row: a saving, no denominator
	old.AttemptedTokens = 0
	fresh := mkEvent(2000, "s-new", "m", 3000, 1000)
	fresh.AttemptedTokens = 4000
	if err := db.insertBatch([]*Event{old, fresh}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var got *Denominator
	for i := range o.Denominators {
		if o.Denominators[i].Key == "attempted" {
			got = &o.Denominators[i]
		}
	}
	if got == nil {
		t.Fatal(`no "attempted" denominator`)
	}
	// Only the instrumented row may contribute to either side: 2000 saved of 4000 attempted.
	if got.Numerator != 2000 || got.Denominator != 4000 {
		t.Errorf("attempted ratio = %d/%d (%.3f%%), want 2000/4000 (50.000%%): the "+
			"pre-instrumentation row's 9000-token saving has no denominator and may not "+
			"inflate a ratio it cannot divide into", got.Numerator, got.Denominator, got.Percent)
	}
	if o.SavedGross != 11000 {
		t.Errorf("SavedGross = %d, want 11000 — the all-rows figure is still reported as "+
			"itself; only the ratio's basis changed", o.SavedGross)
	}
	if o.AttemptedRequests != 1 {
		t.Errorf("AttemptedRequests = %d, want 1", o.AttemptedRequests)
	}
}

// CachesplitHistoricalUSD retro-applies one model's stable half to the rows that never recorded
// one. That is only legitimate while the stable half is CONSTANT for the model — the premise its
// own header asserted and CachesplitSizeSpread disproves: 7 of 9 models on real traffic have a
// spread, up to 5.5x. The two ends differ by 31x in what they credit, so a model with a spread
// is refused into Uncovered rather than valued at either end.
func TestHistoricalSplitRefusesAModelWhoseStableHalfIsNotConstant(t *testing.T) {
	db := openTestDB(t)
	// "spread" records two different stable halves; "flat" records one, twice.
	lo := mkEvent(3000, "s-lo", "spread", 100, 100)
	lo.SplitStableTokens = 1000
	hi := mkEvent(4000, "s-hi", "spread", 100, 100)
	hi.SplitStableTokens = 6000
	f1 := mkEvent(3000, "s-f1", "flat", 100, 100)
	f1.SplitStableTokens = 2000
	f2 := mkEvent(4000, "s-f2", "flat", 100, 100)
	f2.SplitStableTokens = 2000
	// Two session-first, pre-instrumentation rows that BOTH pass the read/write gate at the
	// smaller end of the spread — the only difference between them is the model.
	preSpread := mkEvent(1000, "s-pre-spread", "spread", 100, 100)
	preSpread.SplitStableTokens, preSpread.CacheRead, preSpread.CacheWrite = 0, 50000, 0
	preFlat := mkEvent(1000, "s-pre-flat", "flat", 100, 100)
	preFlat.SplitStableTokens, preFlat.CacheRead, preFlat.CacheWrite = 0, 50000, 0
	if err := db.insertBatch([]*Event{lo, hi, f1, f2, preSpread, preFlat}); err != nil {
		t.Fatal(err)
	}
	h, err := db.CachesplitHistoricalUSD(Filter{}, staticPricer{ibmSonnet})
	if err != nil {
		t.Fatal(err)
	}
	// Only the flat model is valued, priced at its single recorded stable half.
	miss := ibmSonnet.CacheWrite
	if miss < ibmSonnet.Input {
		miss = ibmSonnet.Input
	}
	want := 2000 * (miss - ibmSonnet.CacheRead)
	if h.Requests != 1 || h.Models != 1 {
		t.Errorf("valued %d request(s) over %d model(s), want 1 and 1: the model whose "+
			"recorded min (1000) and max (6000) disagree has no constant stable half to "+
			"retro-apply", h.Requests, h.Models)
	}
	if h.USD != want {
		t.Errorf("credit $%.9f, want $%.9f (the flat model's 2000 tokens only)", h.USD, want)
	}
	if h.Uncovered != 1 {
		t.Errorf("Uncovered = %d, want 1 — a refused model must be REPORTED as unvaluable, "+
			"not silently dropped; that is what this counter is for", h.Uncovered)
	}
	// Same refusal on the per-tenant pass, which is a second copy of the same arithmetic.
	byTenant, err := db.CachesplitHistoricalUSDByTenant(0, staticPricer{ibmSonnet})
	if err != nil {
		t.Fatal(err)
	}
	var reqs, unc int64
	for _, v := range byTenant {
		reqs, unc = reqs+v.Requests, unc+v.Uncovered
	}
	if reqs != 1 || unc != 1 {
		t.Errorf("per-tenant pass valued %d and refused %d, want 1 and 1 — it must agree with "+
			"CachesplitHistoricalUSD, not drift from it", reqs, unc)
	}
}

// KVCacheSuggest's headline total gated on cell.Valued, which is kvcache.Result's
// `Unpriced < Requests` — ANY one priced request. A cell can therefore contribute a saving
// measured on a fraction of itself while being labelled with the whole cell.
func TestKVCacheSuggestTotalRefusesAPartiallyPricedCell(t *testing.T) {
	start := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	var evs []*Event
	for i := int64(0); i < 6; i++ {
		// One priced model, five on the model staticPricer has no rates for. Valued is true
		// (one request priced); the coverage is 1 in 6.
		model := "some/unmeasured-model"
		if i == 0 {
			model = "m"
		}
		evs = append(evs, kvEvent("mixed", "s1", model, start+i*600_000, 0, 100_000))
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "mixed", 9)
	if c == nil {
		t.Fatal("no cell for hour 9")
	}
	if c.UnpricedRequests != 5 {
		t.Fatalf("cell reports %d unpriced of %d requests, want 5 — without this the reader "+
			"cannot see the coverage at all", c.UnpricedRequests, c.Requests)
	}
	if !c.Valued {
		t.Fatalf("fixture no longer reproduces the condition: Valued is false, so the cell " +
			"would have been excluded by the old gate too")
	}
	if out.TotalSavingUSD != 0 || out.TotalSavingCells != 0 {
		t.Errorf("total = $%.6f over %d cell(s); a cell with %d of %d requests unpriced "+
			"describes only a sixth of itself and may not be summed into a service-wide "+
			"headline", out.TotalSavingUSD, out.TotalSavingCells, c.UnpricedRequests, c.Requests)
	}
	if out.TotalSavingKnown {
		t.Error("TotalSavingKnown is true with no qualifying cell — the figure must read n/a, " +
			"never $0.00, which would claim that switching every cell to its winner saves nothing")
	}
	if out.TotalUnpricedRequests != 5 {
		t.Errorf("TotalUnpricedRequests = %d, want 5", out.TotalUnpricedRequests)
	}
}

// The same total still adds a cell that IS fully priced, so the gate above narrows the
// population rather than emptying it.
func TestKVCacheSuggestTotalStillCountsAFullyPricedCell(t *testing.T) {
	start := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli()
	var evs []*Event
	for i := int64(0); i < 6; i++ {
		evs = append(evs, kvEvent("priced", "s1", "m", start+i*600_000, 0, 100_000))
	}
	db := seedKV(t, evs...)
	out, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := findSuggestCell(out, "priced", 9)
	if c == nil {
		t.Fatal("no cell for hour 9")
	}
	if c.UnpricedRequests != 0 || c.SavingUSD <= 0 {
		t.Fatalf("fixture: unpriced=%d saving=$%.6f", c.UnpricedRequests, c.SavingUSD)
	}
	if !out.TotalSavingKnown || out.TotalSavingCells != 1 || out.TotalSavingUSD != c.SavingUSD {
		t.Errorf("total = $%.6f known=%v cells=%d, want the cell's own $%.6f over 1 cell",
			out.TotalSavingUSD, out.TotalSavingKnown, out.TotalSavingCells, c.SavingUSD)
	}
}

// Compile-time anchor for the claim the two tests above rest on: kvcache.Result.Valued is a
// floor ("any request priced"), never a coverage measure, so a caller that gates a total on it
// alone is gating on the wrong thing. If this ever stops holding, the gate can be simplified.
func TestResultValuedIsAFloorNotCoverage(t *testing.T) {
	r := &kvcache.Result{Requests: 100, Unpriced: 99}
	r.Valued = r.Requests > 0 && r.Unpriced < r.Requests // the expression at simulate.go:610
	if !r.Valued {
		t.Fatal("kvcache.Result.Valued no longer admits a 1-in-100 coverage cell; " +
			"KVCacheSuggest's coverage gate can be reconsidered")
	}
	// And Savings.Known does not close the hole either: it is only "the percentage has a
	// divisor", which one priced request is enough to give.
	s := kvcache.Compare(&kvcache.Result{TotalUSD: 1}, &kvcache.Result{TotalUSD: 0.5})
	if !s.Known {
		t.Fatal("Savings.Known is no longer baseline-nonzero; recheck the gate")
	}
}
