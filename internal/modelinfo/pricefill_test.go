package modelinfo

import "testing"

// One function decides a cache price: a stated rate, then the operator's multiple, then the model.
func TestPriceCacheTiersFollowOneRule(t *testing.T) {
	const in = 10e-6
	near := func(a, b float64) bool { d := a - b; return d < 1e-15 && d > -1e-15 }
	for _, tc := range []struct {
		name         string
		p            Price
		model        string
		read, write5 float64
	}{
		{"stated wins", Price{Input: in, CacheRead: 7e-6, CacheWrite: 9e-6, ReadFrac: 0.3, WriteFrac: 2}, "claude-opus-5-5", 7e-6, 9e-6},
		{"operator multiple beats model", Price{Input: in, ReadFrac: 0.3, WriteFrac: 2}, "claude-opus-5-5", 3e-6, 20e-6},
		{"model decides when none is set", Price{Input: in}, "claude-opus-5-5", 0.5e-6, 12.5e-6},
		{"fable tier", Price{Input: in}, "claude-fable-5-1", 0.25e-6, 12.5e-6},
		{"non-Anthropic writes at 1.0x", Price{Input: in}, "gemini-3-flash-preview", 1e-6, 10e-6},
		{"empty model", Price{Input: in}, "", 1e-6, 10e-6},
	} {
		if got := tc.p.CacheReadFor(tc.model); !near(got, tc.read) {
			t.Errorf("%s: read = %v, want %v", tc.name, got, tc.read)
		}
		if got := tc.p.CacheWrite5mFor(tc.model); !near(got, tc.write5) {
			t.Errorf("%s: write 5m = %v, want %v", tc.name, got, tc.write5)
		}
	}
	if got, want := (Price{Input: in}).CacheWrite1hFor("x"), 2*in; got != want {
		t.Errorf("write 1h = %v, want %v", got, want)
	}
	if got, want := (Price{Input: in, Write1hFrac: 3}).CacheWrite1hFor("x"), 3*in; !near(got, want) {
		t.Errorf("write 1h with operator multiple = %v, want %v", got, want)
	}
	r := Price{Input: in, ReadFrac: 0.3}.Resolved("claude-opus-5-5")
	if r.ReadFrac != 0 || !near(r.CacheRead, 3e-6) {
		t.Errorf("Resolved = %+v", r)
	}
}
