package campaign

import (
	"math"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// The adapt report's dollars: the closed form (RowFacts.Outcome) in expectation over ping rewrites, the miss of a rescued request, a cliff with a
// spread, the head a missed request reads anyway, and hits after long gaps from other sessions sharing the prefix. The builder's gate keeps its
// calibrated closed form; this is what a report shows as money.

const (
	// default cliff, rewrite and long-gap model parameters
	fidTc, fidTcSpread = 283.0, 4.0 // cliff and its uniform width, seconds
	FidPFirst          = 0.0089     // P(a first ping rewrites instead of refreshing)
	fidPLater          = 0.0257     // P(a later ping does); after a rewrite the keeper stops the span
	fidDeadGapS        = 1200.0     // gaps this long are beyond every shipped ping plan: their hits are other sessions' doing
)

// FidMiss is the default miss rate of a request that only a ping kept alive, by model family.
func FidMiss(model string) float64 {
	switch fam := modelinfo.Family(model); {
	case fam == modelinfo.FamilyOpus && modelinfo.IsTierVersion(model, modelinfo.FamilyOpus, 5, 5):
		return 0.009
	case fam == modelinfo.FamilySonnet:
		return 0.013
	case fam == modelinfo.FamilyHaiku:
		return 0.017
	}
	// Other opus ids and fable have no measured rate here: they get the default.
	return 0.028
}

// FidParams are the two quantities fitted on the caller's own history: h, the share of requests that still hit after a gap no ping covers
// (another session kept the shared prefix warm), and the head a request that misses reads anyway, per model.
type FidParams struct {
	H    float64
	Head map[string]float64
	N    int // requests the fit used
}

// FitFidelity fits h and the heads on the tenant's own requests.
func FitFidelity(reqs []*kvcache.Request, tenant string) FidParams {
	byID := make(map[int64]*kvcache.Request, len(reqs))
	for _, r := range reqs {
		byID[r.ID] = r
	}
	var hits, n int
	headSum, headN := map[string]float64{}, map[string]float64{}
	for _, r := range reqs {
		if r.User != tenant || r.KeepAlive || !r.HasNext {
			continue
		}
		q := byID[r.NextID]
		if q == nil || !q.CostKnown || q.CachedContext <= 0 || float64(r.NextTS-r.TS-int64(r.UpstreamMs))/1000 < fidDeadGapS {
			continue
		}
		n++
		if float64(q.CacheWrite) < 0.8*float64(q.CachedContext) {
			hits++
		} else {
			headSum[q.Model] += float64(q.CacheRead)
			headN[q.Model]++
		}
	}
	p := FidParams{Head: map[string]float64{}, N: n}
	if n > 0 {
		p.H = float64(hits) / float64(n)
	}
	for m, s := range headSum {
		p.Head[m] = s / headN[m]
	}
	return p
}

// Expected is plan (k pings x s apart) on the row in expectation over ping failures and the cliff: the consumer's saving, the pings' cost (a
// failed ping is billed as a rewrite), the expected number of pings and of rescues. Gateway dollars.
func (f *RowFacts) Expected(k int, x float64, sessionEnd bool, p *FidParams) (rescue, ping, pings, rescues float64) {
	stop, gap := math.Inf(1), math.Inf(1)
	if sessionEnd {
		stop = f.ToEnd
	}
	if f.Returned {
		gap = f.Gap
	}
	var s []float64 // start of each ping that would fire, relative to the response end
	for j := 1; j <= k; j++ {
		t := float64(j)*x - f.U + Late
		if t >= gap || t >= stop || t >= f.Span {
			break
		}
		s = append(s, t)
	}
	R := float64(f.Prefix)
	if f.NextPrefix > 0 {
		R = math.Min(R, float64(f.NextPrefix))
	}
	stake := math.Max(R-p.Head[f.Model], 0) * (f.PWrite - f.PRead)
	rewrite := float64(f.Prefix) * (f.PWrite - f.PRead)
	ms := FidMiss(f.Model)
	const nTc = 5
	survive := 1.0 // P(no failure among the pings so far)
	for j := 0; j <= len(s); j++ {
		// outcome "the first failure is ping j+1" (j < len(s)) or "no failure" (j == len(s)): pings 1..j succeed
		var pr float64
		sent := j
		touch := 0.0
		if j > 0 {
			touch = s[j-1] + PingDur
		}
		cost := float64(j) * f.Ping
		if j < len(s) {
			pf := fidPLater
			if j == 0 {
				pf = FidPFirst
			}
			pr = survive * pf
			survive *= 1 - pf
			sent++
			touch = s[j] + PingDur
			cost += f.Ping + rewrite // the failed ping rewrote the prefix; the entry is warm again, the span stops
		} else {
			pr = survive
		}
		if pr == 0 {
			continue
		}
		ping += pr * cost
		pings += pr * float64(sent)
		if !f.Returned || sent == 0 {
			continue
		}
		for i := 0; i < nTc; i++ {
			tc := fidTc + (float64(i)+0.5)/nTc*fidTcSpread - fidTcSpread/2
			if gap < tc {
				continue // a natural hit: the pings bought nothing
			}
			if gap-touch < tc {
				rescue += pr / nTc * stake * ((1 - ms) - p.H)
				rescues += pr / nTc * (1 - ms - p.H)
			}
		}
	}
	return rescue, ping, pings, rescues
}
