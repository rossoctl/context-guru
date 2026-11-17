package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Issue #1080: the what-if screens price a re-sent block with estimateRepeatRate, which the legacy
// Event.repeatRate calls. The three cases are the rule's: a read keeps the read rate, a write that
// covers the prompt earns the write rate, anything else is the fresh rate.
func TestEstimateRepeatRate(t *testing.T) {
	p := modelinfo.Price{Input: 3e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	for _, tc := range []struct {
		name               string
		fresh, read, write int64
		want               float64
	}{
		{"a hit", 100, 5000, 0, p.CacheRead},
		{"a partial hit keeps the read rate", 100, 5000, 2000, p.CacheRead},
		{"a write that covers the prompt", 100, 0, 5000, p.CacheWrite},
		{"a write smaller than the fresh part", 100000, 0, 2000, p.Input},
		{"nothing cached", 100, 0, 0, p.Input},
	} {
		if got := estimateRepeatRate(p, tc.fresh, tc.read, tc.write); got != tc.want {
			t.Errorf("%s: rate %v, want %v", tc.name, got, tc.want)
		}
		e := Event{FreshInput: tc.fresh, CacheRead: tc.read, CacheWrite: tc.write}
		if got := e.repeatRate(p); got != tc.want {
			t.Errorf("%s: Event.repeatRate %v, want the estimate's %v", tc.name, got, tc.want)
		}
	}
}
