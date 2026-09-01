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
