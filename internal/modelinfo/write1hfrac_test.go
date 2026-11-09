package modelinfo

import (
	"context"
	"math"
	"strings"
	"testing"
)

const flatEntry = "models:\n  - {match: acme/flat-1, in: 3, out: 15, cache_write_1h_frac: 1.0}\n"

// An entry may set its 1-hour cache-write rate as a multiple of `in`.
func TestParseTableReadsCacheWrite1hFrac(t *testing.T) {
	tb, err := ParseTable([]byte(flatEntry))
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.entries[0].price.Write1hFrac; got != 1.0 {
		t.Fatalf("Write1hFrac = %v, want 1.0", got)
	}
}

func TestParseTableLeavesCacheWrite1hFracZeroWhenUnset(t *testing.T) {
	tb, err := ParseTable([]byte("models:\n  - {match: claude-sonnet*, in: 3, out: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.entries[0].price.Write1hFrac; got != 0 {
		t.Fatalf("Write1hFrac = %v, want 0", got)
	}
}

func TestParseTableRejectsNegativeCacheWrite1hFrac(t *testing.T) {
	_, err := ParseTable([]byte("models:\n  - {match: acme/flat-1, in: 3, out: 15, cache_write_1h_frac: -1}\n"))
	if err == nil || !strings.Contains(err.Error(), "cache_write_1h_frac") {
		t.Fatalf("err = %v, want a cache_write_1h_frac error", err)
	}
}

// Table.Price resolves the entry. The 1-hour multiple must survive that, or the 2.0x default
// comes back on every lookup.
func TestTablePriceKeepsTheCacheWrite1hFrac(t *testing.T) {
	tb, err := ParseTable([]byte(flatEntry))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Price(context.Background(), "acme/flat-1")
	if !ok {
		t.Fatal("acme/flat-1 not priced")
	}
	if got, want := p.CacheWrite1h(), 3e-6; math.Abs(got-want) > 1e-18 {
		t.Fatalf("CacheWrite1h = %v, want %v (the input rate)", got, want)
	}
}

// Resolved fills the read and 5-minute tiers. No field holds the 1-hour rate, so the multiple
// must stay on the price.
func TestResolvedKeepsWrite1hFrac(t *testing.T) {
	r := Price{Input: 3e-6, Write1hFrac: 1.0}.Resolved("acme/flat-1")
	if r.Write1hFrac != 1.0 {
		t.Fatalf("Write1hFrac after Resolved = %v, want 1.0", r.Write1hFrac)
	}
	if got, want := r.CacheWrite1h(), 3e-6; math.Abs(got-want) > 1e-18 {
		t.Fatalf("CacheWrite1h after Resolved = %v, want %v", got, want)
	}
}

// A 1-hour write billed at the input rate adds no extra cost. acme/flat-1 is not a Claude id,
// so the 5-minute write is 1.0x as well.
func TestEntryWithOneHourFracOfOneAddsNoExtraCost(t *testing.T) {
	tb, err := ParseTable([]byte(flatEntry))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tb.Price(context.Background(), "acme/flat-1")
	got := p.CostWithCacheWrite1h(4, 0, 20315, 12, 20157)
	want := p.CostWithCacheWrite1h(4, 0, 20315, 12, 0)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost with 1h writes = %v, want %v (no extra cost)", got, want)
	}
	// 20,319 input-rate tokens at $3/MTok plus 12 output at $15/MTok.
	if math.Abs(want-(20319*3e-6+12*15e-6)) > 1e-12 {
		t.Fatalf("cost = %v, want %v", want, 20319*3e-6+12*15e-6)
	}
}

// A Claude entry without the key keeps the 2.0x rate: (2.0 - 1.25) x $3/MTok extra on each
// one-hour token.
func TestClaudeEntryWithoutTheKeyKeepsTheTwoXRate(t *testing.T) {
	tb, err := ParseTable([]byte("models:\n  - {match: claude-sonnet*, in: 3, out: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tb.Price(context.Background(), "claude-sonnet-4-6")
	got := p.CostWithCacheWrite1h(4, 0, 20315, 12, 20157)
	want := 4*3e-6 + 20315*3.75e-6 + 12*15e-6 + 20157*0.75*3e-6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

// The shipped price list sets the 1-hour rate on the 6 Bob tiers only. Bob bills a 1-hour write
// at the input rate; every other entry keeps the 2.0x default.
func TestShippedPriceListSetsTheOneHourRateOnTheBobTiersOnly(t *testing.T) {
	tb, err := LoadTable("../../deploy/service/prices.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bob := map[string]bool{"premium": true, "premium-ide": true, "standard": true,
		"standard-ide": true, "fast": true, "fast-ide": true}
	seen := 0
	for _, e := range tb.entries {
		want := 0.0
		if bob[e.match] {
			want, seen = 1.0, seen+1
		}
		if e.price.Write1hFrac != want {
			t.Errorf("%s: Write1hFrac = %v, want %v", e.match, e.price.Write1hFrac, want)
		}
	}
	if seen != len(bob) {
		t.Fatalf("found %d of the %d Bob tiers", seen, len(bob))
	}
}

// A 1-hour write is never cheaper than a 5-minute one. A Claude entry bills the 5-minute write at
// 1.25x, so a 1-hour multiple of 1.0 gives the 5-minute rate on every path, not 1.0x on some.
func TestOneHourFracBelowTheFiveMinuteRateGivesTheFiveMinuteRate(t *testing.T) {
	tb, err := ParseTable([]byte("models:\n  - {match: claude-sonnet*, in: 3, out: 15, cache_write_1h_frac: 1.0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tb.Price(context.Background(), "claude-sonnet-4-6")
	if got, want := p.CacheWrite1h(), 3.75e-6; math.Abs(got-want) > 1e-18 {
		t.Fatalf("CacheWrite1h = %v, want %v (the 5-minute rate)", got, want)
	}
}
