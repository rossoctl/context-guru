package tooldefer

import "testing"

// TierOf follows accountant.HeadTier. A usage with 1h tokens and no cache-write total is bad data
// (the 1h part is inside the write total): its head is fresh input, as it is on the dashboard.
func TestTierOfFollowsTheAccountantHeadTier(t *testing.T) {
	for _, c := range []struct {
		read, write, write1h int64
		want                 Tier
	}{
		{1, 5, 5, TierRead}, {0, 5, 5, TierWrite1h}, {0, 5, 0, TierWrite}, {0, 0, 0, TierFresh}, {0, 0, 5, TierFresh},
	} {
		if got := TierOf(c.read, c.write, c.write1h); got != c.want {
			t.Errorf("TierOf(%d,%d,%d) = %d, want %d", c.read, c.write, c.write1h, got, c.want)
		}
	}
}
