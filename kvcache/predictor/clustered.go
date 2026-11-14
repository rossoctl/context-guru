package predictor

import (
	"sort"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// minCellClustered is the same floor kvcache.History's own minCell sets, and for the same
// reason: a five-minute reuse probability estimated from fewer than six closed gaps is not
// a probability, it is noise with a percentage sign on it.
const minCellClustered = 6

// clusterLevels are keysForCluster's five keys, most specific first, index for index.
var clusterLevels = []string{
	"user+model+cluster+bucket", "user+model+cluster", "user+cluster", "model+cluster",
	"cluster",
}

// ClusteredHistory is kvcache.History's fallback ladder with the request's own
// StopReasonCluster PINNED into every cell — never dropped.
//
// kvcache.History's ladder drops dimensions one at a time (user+model+bucket ->
// user+model -> user -> model -> global) until a cell clears minCell. Adding cluster as a
// SIXTH dimension to that same ladder would let a thin cell fall back to one with no
// cluster at all — and dropping cluster is exactly the failure this type exists to close:
// 92.5% of decision points are kvcache.ClusterStillWorking, so any cell reachable without a
// cluster constraint is swamped by that majority, drowning the minority signal a previous
// model needed and shipping an arm that measured actively harmful. Every rung here keeps
// cluster fixed and drops only user/model/bucket around it; the coarsest rung is "cluster"
// alone (the global answer WITHIN that cluster), never the unqualified global.
//
// Deliberately a separate type from kvcache.History rather than a change to it: History is
// exported and consumed by kvcache.Simulate and every shipped Strategy today, and widening
// its key would either break every existing call site's signature or silently change what
// its five existing levels mean. This is 1:1 the same shape — Observe on a gap that has
// just closed, ReuseWithin/MedianIdle with an explicit fallback level — over a key that
// happens to include one more field.
type ClusteredHistory struct {
	cells map[clusterKey]*clusterCell
}

type clusterKey struct {
	user, model string
	bucket      kvcache.Bucket
	cluster     kvcache.StopReasonCluster
}

type clusterCell struct {
	gaps   []time.Duration
	sorted bool
	within map[time.Duration]int
}

// NewClusteredHistory builds an empty accumulator.
func NewClusteredHistory() *ClusteredHistory {
	return &ClusteredHistory{cells: map[clusterKey]*clusterCell{}}
}

// keysForCluster are the cells one observation lands in, cluster always present, most
// specific first.
func keysForCluster(user, model string, b kvcache.Bucket, c kvcache.StopReasonCluster) []clusterKey {
	return []clusterKey{
		{user, model, b, c},
		{user, model, "", c},
		{user, "", "", c},
		{"", model, "", c},
		{"", "", "", c},
	}
}

// Observe records one CLOSED gap, under the StopReasonCluster of the request that opened
// it — the request just served BEFORE the idle span began, present tense, the same footing
// StopReason stands on elsewhere in this codebase. Callers must not call this for a gap
// that has not happened yet; Replay calls it in exactly one place, on the successor's
// arrival, mirroring kvcache.History.Observe.
func (h *ClusteredHistory) Observe(user, model string, b kvcache.Bucket, c kvcache.StopReasonCluster, gap time.Duration) {
	if h == nil {
		return
	}
	if gap < 0 {
		gap = 0
	}
	for _, k := range keysForCluster(user, model, b, c) {
		cell := h.cells[k]
		if cell == nil {
			cell = &clusterCell{within: map[time.Duration]int{}}
			h.cells[k] = cell
		}
		cell.gaps = append(cell.gaps, gap)
		cell.sorted = false
		if gap <= kvcache.Horizon5m {
			cell.within[kvcache.Horizon5m]++
		}
		if gap <= kvcache.Horizon1h {
			cell.within[kvcache.Horizon1h]++
		}
	}
}

// lookup finds the most specific cell with enough observations, and says which it was.
func (h *ClusteredHistory) lookup(user, model string, b kvcache.Bucket, c kvcache.StopReasonCluster) (*clusterCell, string) {
	if h == nil {
		return nil, kvcache.LevelNone
	}
	ks := keysForCluster(user, model, b, c)
	for i, k := range ks {
		if cell := h.cells[k]; cell != nil && len(cell.gaps) >= minCellClustered {
			return cell, clusterLevels[i]
		}
	}
	if cell := h.cells[ks[len(ks)-1]]; cell != nil && len(cell.gaps) > 0 {
		return cell, clusterLevels[len(clusterLevels)-1] // "cluster": the coarsest rung, still cluster-scoped
	}
	return nil, kvcache.LevelNone
}

// ReuseWithin is the share of this cluster-scoped cell's already-closed gaps that were no
// longer than d.
func (h *ClusteredHistory) ReuseWithin(user, model string, b kvcache.Bucket, c kvcache.StopReasonCluster, d time.Duration) (float64, int, string) {
	cell, level := h.lookup(user, model, b, c)
	if cell == nil || len(cell.gaps) == 0 {
		return 0, 0, kvcache.LevelNone
	}
	n := len(cell.gaps)
	if hits, ok := cell.within[d]; ok {
		return float64(hits) / float64(n), n, level
	}
	hits := 0
	for _, g := range cell.gaps {
		if g <= d {
			hits++
		}
	}
	return float64(hits) / float64(n), n, level
}

// MedianIdle is this cluster-scoped cell's median already-closed gap.
func (h *ClusteredHistory) MedianIdle(user, model string, b kvcache.Bucket, c kvcache.StopReasonCluster) (time.Duration, int, string) {
	cell, level := h.lookup(user, model, b, c)
	if cell == nil || len(cell.gaps) == 0 {
		return 0, 0, kvcache.LevelNone
	}
	if !cell.sorted {
		sort.Slice(cell.gaps, func(i, j int) bool { return cell.gaps[i] < cell.gaps[j] })
		cell.sorted = true
	}
	return cell.gaps[(len(cell.gaps)-1)/2], len(cell.gaps), level
}
