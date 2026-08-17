package dash

import "testing"

// TestBreakdownCacheBreakpointsUnknownForObserve: the `cache_breakpoints` dimension is a
// COUNT the pipeline makes. In observe mode the enforced path never runs the pipeline, so
// the count is never made and the four cache_bp_* columns read zero — which is not the
// same statement as "this request arrived with no breakpoints". Keying both as "0" merges
// them into one bar, and on a proxy running wholly in observe mode that bar is the entire
// chart, reporting "none of your traffic sets a breakpoint" about traffic nobody counted.
//
// Unset must key as "", which GroupRow.Key documents the UI as labelling unset.
func TestBreakdownCacheBreakpointsUnknownForObserve(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A genuine zero, a genuine three, and an observe row whose request DID arrive with
	// three breakpoints — the pipeline just never counted them, so the row carries zeros.
	zero := mkEvent(DayMs+1, "s-zero", "m", 100, 80)
	three := mkEvent(DayMs+2, "s-three", "m", 100, 80)
	three.CacheBPSystem, three.CacheBPMessages = 1, 2
	obs := mkEvent(DayMs+3, "s-obs", "m", 100, 80)
	obs.Mode = ModeObserve
	if err := db.insertBatch([]*Event{zero, three, obs}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Breakdown(Filter{TenantAll: true}, "cache_breakpoints")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, g := range rows {
		got[g.Key] = g.Requests
	}
	want := map[string]int64{"": 1, "0": 1, "3": 1}
	if len(got) != len(want) {
		t.Fatalf("cache_breakpoints breakdown = %v, want %v — an observe row cannot be "+
			"reported as a counted zero", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("cache_breakpoints[%q] = %d, want %d (full: %v)", k, got[k], n, got)
		}
	}
}
