package dash

import "testing"

// #1283: a request whose only mark is the top-level cache_control shows 1 in the top-level slot, and
// the placement sums add up to the breakpoint total of the requests.
func TestOverviewPlacementCountsTheTopLevelMark(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rootOnly := mkEvent(1000, "s1", "m", 100, 90)
	rootOnly.CacheBPRoot = 1
	mixed := mkEvent(2000, "s2", "m", 100, 90)
	mixed.CacheBPSystem, mixed.CacheBPBlocks, mixed.CacheBPRoot = 1, 2, 1
	if err := db.insertBatch([]*Event{rootOnly, mixed}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	b := o.Breakpoints
	if b.Root != 2 || b.System != 1 || b.Blocks != 2 || b.Requests != 2 {
		t.Errorf("placement = %+v, want root 2, system 1, blocks 2, requests 2", b)
	}
	want := int64(rootOnly.CacheBreakpoints() + mixed.CacheBreakpoints())
	if sum := b.System + b.Tools + b.Messages + b.Blocks + b.Root; sum != want || want != 5 {
		t.Errorf("placement sums add up to %d, want the total %d (5)", sum, want)
	}
	// The root-only request is its own modal placement, with the root slot in the string.
	solo, err := db.Overview(Filter{Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if solo.Breakpoints.Root != 1 || solo.Breakpoints.Modal != "system=0, tools=0, messages=0, blocks=0, top-level=1" {
		t.Errorf("root-only request: %+v", solo.Breakpoints)
	}
}
