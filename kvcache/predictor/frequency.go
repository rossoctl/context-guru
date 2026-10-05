package predictor

import (
	"sort"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// rollingWindow bounds how many of the MOST RECENT closed gaps the rolling statistics below
// look at.
//
// ponytail: a fixed constant rather than an adaptive or configurable window — ten gaps is
// enough for a median and an IQR to mean something without needing a whole conversation's
// history recomputed on every decision. Upgrade path: make it a StatsContext field if a real
// model ever wants a different width.
const rollingWindow = 10

// closedGaps is every gap this conversation has ALREADY CLOSED as of the current decision,
// oldest first, in milliseconds: the differences between consecutive rows in past, plus the
// one connecting past's last row to now (the same gap kvcache.Observation.SinceLastMs
// already carries). All of it is present-tense — past is the same capped, leak-free slice
// every Extractor in this package receives, and now is the decision instant itself.
//
// Empty on a conversation's first request: there is no gap yet, not a zero-length one.
func closedGaps(past []*kvcache.Request, now int64) []int64 {
	if len(past) == 0 {
		return nil
	}
	gaps := make([]int64, 0, len(past))
	for i := 1; i < len(past); i++ {
		gaps = append(gaps, past[i].TS-past[i-1].TS)
	}
	gaps = append(gaps, now-past[len(past)-1].TS)
	return gaps
}

// countSince is how many rows in past arrived within window before now — "requests in the
// previous N minutes", not counting the current one (it has not arrived yet from past's own
// point of view; it IS now).
func countSince(past []*kvcache.Request, now int64, window time.Duration) int {
	n := 0
	for _, r := range past {
		if time.Duration(now-r.TS)*time.Millisecond <= window {
			n++
		}
	}
	return n
}

// tail is the last n elements of gaps, or all of them if there are fewer than n.
func tail(gaps []int64, n int) []int64 {
	if len(gaps) <= n {
		return gaps
	}
	return gaps[len(gaps)-n:]
}

// sortedCopy returns gaps sorted ascending, without mutating the caller's slice — every
// caller below reuses the same closedGaps result for more than one statistic.
func sortedCopy(gaps []int64) []int64 {
	out := make([]int64, len(gaps))
	copy(out, gaps)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// quantile is the value at fraction q (0..1) of a slice ALREADY sorted ascending, by linear
// interpolation between the two nearest ranks — the same method dash/overview.go's own
// percentile helper uses, so a report comparing the two is comparing like with like.
func quantile(sorted []int64, q float64) float64 {
	if len(sorted) == 1 {
		return float64(sorted[0])
	}
	pos := q * float64(len(sorted)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(sorted) {
		return float64(sorted[lo])
	}
	frac := pos - float64(lo)
	return float64(sorted[lo])*(1-frac) + float64(sorted[hi])*frac
}

// ewmaGap is the exponentially-weighted moving average of gaps (oldest first), with the
// smoothing constant alpha applied newest-heaviest, seeded on the first gap so a
// two-observation conversation is not compared against zero.
//
// ponytail: alpha is a fixed 0.3, a heuristic ceiling rather than a fitted one — see this
// package's own doc comment on ClusteredHistory's minCell for the same kind of stated,
// unfitted constant.
func ewmaGap(gaps []int64) (float64, bool) {
	if len(gaps) == 0 {
		return 0, false
	}
	const alpha = 0.3
	e := float64(gaps[0])
	for _, g := range gaps[1:] {
		e = alpha*float64(g) + (1-alpha)*e
	}
	return e, true
}

// gapVariance is the population variance (divide by n, not n-1) of gaps — a description of
// the sample actually observed, not an estimate of a wider population.
func gapVariance(gaps []int64) (float64, bool) {
	if len(gaps) == 0 {
		return 0, false
	}
	var sum float64
	for _, g := range gaps {
		sum += float64(g)
	}
	mean := sum / float64(len(gaps))
	var ss float64
	for _, g := range gaps {
		d := float64(g) - mean
		ss += d * d
	}
	return ss / float64(len(gaps)), true
}
