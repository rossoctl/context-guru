package dash

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// workWeekOnly puts every Sunday-Thursday row in one segment and drops the rest — KVCacheSuggest's
// own filter, written as a Segmenter.
func workWeekOnly(r *kvcache.Request) (string, bool) { return "all", kvInWorkWeek(r.TS) }

func suggestByFixture(t *testing.T) *DB {
	t.Helper()
	sun := time.Date(2023, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli() // Sunday 09:00
	mon := time.Date(2023, 1, 2, 9, 0, 0, 0, time.UTC).UnixMilli() // Monday 09:00
	fri := time.Date(2023, 1, 6, 9, 0, 0, 0, time.UTC).UnixMilli() // Friday 09:00
	var evs []*Event
	for i := int64(0); i < 8; i++ {
		evs = append(evs,
			kvEvent("u1", "s1", "m", sun+i*240_000, 0, 120_000), // 4-minute gaps
			kvEvent("u1", "s2", "m", mon+i*420_000, 0, 120_000), // 7-minute gaps: past 5m, inside the hour
			kvEvent("u2", "s3", "m", fri+i*60_000, 0, 50_000),
		)
	}
	return seedKV(t, evs...)
}

// THE SAME DECISION. With a segmenter that reproduces KVCacheSuggest's own row filter, every cell of
// KVCacheSuggestBy must equal KVCacheSuggest's cell for the same (user, hour): same winner, same
// dollars. This is what makes a segmented breakdown comparable with the uploaded campaign.
func TestKVCacheSuggestByMatchesKVCacheSuggestCellForCell(t *testing.T) {
	db := suggestByFixture(t)
	want, err := db.KVCacheSuggest(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet}, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.KVCacheSuggestBy(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet}, KVCacheSimConfig{}, workWeekOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != len(want.Cells) || len(got.Cells) == 0 {
		t.Fatalf("%d segmented cells, want %d (and more than 0)", len(got.Cells), len(want.Cells))
	}
	for _, g := range got.Cells {
		w := findSuggestCell(want, g.User, g.HourUTC)
		if w == nil {
			t.Fatalf("segmented cell %s/%d has no KVCacheSuggest counterpart", g.User, g.HourUTC)
		}
		if g.BestStrategy != w.BestStrategy || math.Abs(g.SavingUSD-w.SavingUSD) > 1e-9 ||
			math.Abs(g.BaselineUSD-w.BaselineUSD) > 1e-9 || g.Requests != w.Requests {
			t.Errorf("%s/%d: got %s $%.6f over %d, want %s $%.6f over %d", g.User, g.HourUTC,
				g.BestStrategy, g.SavingUSD, g.Requests, w.BestStrategy, w.SavingUSD, w.Requests)
		}
	}
}

// A segment is its own cell: Sunday and Monday rows of the same (user, hour) are decided separately.
func TestKVCacheSuggestBySplitsACellBySegment(t *testing.T) {
	db := suggestByFixture(t)
	byDay := func(r *kvcache.Request) (string, bool) {
		return time.UnixMilli(r.TS).UTC().Weekday().String(), true
	}
	got, err := db.KVCacheSuggestBy(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet}, KVCacheSimConfig{}, byDay)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	for _, c := range got.Cells {
		if c.User == "u1" && c.HourUTC == 9 {
			seen[c.Segment] += c.Requests
		}
	}
	if seen["Sunday"] != 8 || seen["Monday"] != 8 {
		t.Errorf("u1 hour 9 by weekday = %v, want Sunday 8 and Monday 8", seen)
	}
	// Friday is NOT filtered when the segmenter keeps it: the segmenter is the only filter.
	var fri int64
	for _, c := range got.Cells {
		if c.User == "u2" && c.Segment == "Friday" {
			fri += c.Requests
		}
	}
	if fri != 8 {
		t.Errorf("u2 Friday requests = %d, want 8: the segmenter, not the work week, decides", fri)
	}
}

func TestKVCacheSuggestByNeedsASegmenter(t *testing.T) {
	if _, err := suggestByFixture(t).KVCacheSuggestBy(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{}, nil); err == nil {
		t.Fatal("a nil segmenter was accepted")
	}
}
