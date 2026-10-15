package predictor

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// TestClusteredHistoryNeverDropsCluster reproduces, at bench scale, the exact failure this
// type exists to close: a majority "still working" population that closes far outside the
// five-minute window, alongside a much smaller "actually done" population that almost
// always closes inside it. A cell keyed WITHOUT cluster blends the two and reports a
// reuse rate the minority alone never had.
func TestClusteredHistoryNeverDropsCluster(t *testing.T) {
	h := NewClusteredHistory()
	plain := kvcache.NewHistory()

	// 20 "still working" gaps, none of which return inside 5 minutes.
	for i := 0; i < 20; i++ {
		h.Observe("u", "m", kvcache.BucketNight, kvcache.ClusterStillWorking, time.Hour)
		plain.Observe("u", "m", kvcache.BucketNight, time.Hour)
	}
	// 6 "actually done" gaps — exactly minCell — every one returning inside 5 minutes.
	for i := 0; i < 6; i++ {
		h.Observe("u", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, time.Minute)
		plain.Observe("u", "m", kvcache.BucketNight, time.Minute)
	}

	gotP, gotN, level := h.ReuseWithin("u", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, kvcache.Horizon5m)
	if gotN != 6 {
		t.Fatalf("ClusteredHistory n = %d, want 6 (only the actually_done gaps)", gotN)
	}
	if gotP != 1.0 {
		t.Fatalf("ClusteredHistory p = %.4f, want 1.0 — the minority signal must not be diluted "+
			"by the still_working majority (level=%s)", gotP, level)
	}

	plainP, plainN, _ := plain.ReuseWithin("u", "m", kvcache.BucketNight, kvcache.Horizon5m)
	if plainN != 26 {
		t.Fatalf("sanity: plain kvcache.History n = %d, want 26", plainN)
	}
	if plainP >= 0.5 {
		t.Fatalf("sanity check failed: the unclustered cell's own p = %.4f should already be "+
			"diluted well under the clustered answer of 1.0 for this test to demonstrate anything", plainP)
	}
}

// TestClusteredHistoryFallsBackWithoutDroppingCluster checks the ladder itself: a thin
// user-scoped cell falls back to a coarser level, and every level it can land on is still
// scoped to the SAME cluster — it never reaches the unqualified global answer.
func TestClusteredHistoryFallsBackWithoutDroppingCluster(t *testing.T) {
	h := NewClusteredHistory()
	// Plenty of model-level, actually_done evidence from OTHER users.
	for i := 0; i < 10; i++ {
		h.Observe("someone-else", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, time.Minute)
	}
	// This user has only 2 of their own — below minCellClustered.
	h.Observe("niche", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, time.Minute)
	h.Observe("niche", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, time.Minute)

	_, n, level := h.ReuseWithin("niche", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, kvcache.Horizon5m)
	if level == "user+model+cluster+bucket" || level == "user+model+cluster" {
		t.Fatalf("level = %q with only 2 of this user's own gaps; expected a fallback below minCell", level)
	}
	if level == kvcache.LevelNone {
		t.Fatal("fell all the way to LevelNone despite 12 gaps existing in this cluster")
	}
	if n < minCellClustered {
		t.Fatalf("n = %d at level %q; a level this ladder trusts must clear minCellClustered", n, level)
	}

	// And the cluster the lookup landed on must actually BE actually_done: a still_working
	// gap in the same user/model/bucket must never leak into this answer.
	h.Observe("niche", "m", kvcache.BucketNight, kvcache.ClusterStillWorking, 6*time.Hour)
	p2, n2, _ := h.ReuseWithin("niche", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, kvcache.Horizon5m)
	if n2 != n {
		t.Fatalf("adding a still_working gap changed the actually_done cell's n from %d to %d; "+
			"cluster boundary leaked", n, n2)
	}
	if p2 != 1.0 {
		t.Fatalf("actually_done reuse dropped to %.4f after adding an unrelated still_working gap", p2)
	}
}

func TestClusteredHistoryNilReceiverIsSafe(t *testing.T) {
	var h *ClusteredHistory
	h.Observe("u", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, time.Minute) // must not panic
	if p, n, level := h.ReuseWithin("u", "m", kvcache.BucketNight, kvcache.ClusterActuallyDone, kvcache.Horizon5m); n != 0 || p != 0 || level != kvcache.LevelNone {
		t.Fatalf("nil *ClusteredHistory.ReuseWithin = (%v, %v, %v), want (0, 0, LevelNone)", p, n, level)
	}
}
