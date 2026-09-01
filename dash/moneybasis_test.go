package dash

import (
	"testing"
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
