package dash

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// One test over a dataset shaped like the real one.
//
// Every other test in this package pins a specific behaviour on three or four rows. This one asks
// a different question: on traffic that LOOKS like production, are the figures the page puts on
// screen the right shape? It is the check that catches a page which is correct in every unit and
// nonsense in aggregate — a reuse probability of 4%, a median idle of half an hour, a ceiling
// that costs more than a real arm.
//
// The shape is taken from the live service's own snapshot (14,407 requests, 1,772 conversations,
// 2026-08-17 → 08-19 UTC), scaled down by ten so the suite stays quick:
//
//	1,772 conversations, of which 1,551 hold a single request
//	12,635 within-conversation gaps: p50 14.9 s, p90 106 s, 95.27% ≤ 5 min, 99.26% ≤ 1 h
//	median billed prefix (cache_read + cache_write) 124,845 tokens
//
// Nothing here asserts a figure copied from that snapshot. What is asserted is the RELATIONSHIPS
// that have to hold on any such distribution, which is what makes it a test rather than a golden
// file: a corpus where almost every conversation returns inside five minutes must report a high
// five-minute reuse probability, the one-hour tier must lose money on it, and the exact ceiling
// can never be beaten by a policy that cannot see the future.
func TestThePageIsTheRightShapeOnProductionLikeTraffic(t *testing.T) {
	db := seedKV(t, productionShaped()...)

	out, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := out.Cards
	t.Logf("requests=%d conversations=%d with_next=%d final=%d median_idle=%.0fms p90=%.0fms "+
		"reuse5m=%.2f%% reuse1h=%.2f%% hit=%.1f%% prefix_p50=%d cost=$%.2f",
		c.Requests, c.Conversations, c.WithNext, c.FinalRequests, c.MedianIdleMs, c.P90IdleMs,
		c.Within5mPct, c.Within1hPct, c.HitRatePct, c.CachedContextP50, c.CostUSD)

	// Every conversation contributes exactly one final request, and the idle denominator is
	// everything else. This is the invariant the whole page's honesty rests on.
	if c.FinalRequests != c.Conversations {
		t.Errorf("final requests %d != conversations %d; a conversation has exactly one last "+
			"request in a window", c.FinalRequests, c.Conversations)
	}
	if c.WithNext+c.FinalRequests != c.Requests {
		t.Errorf("with_next %d + final %d != requests %d", c.WithNext, c.FinalRequests, c.Requests)
	}
	// A corpus where almost everything returns in seconds must SAY so. A page reporting a low
	// five-minute reuse probability on this traffic is a page whose derivation is broken.
	if c.Within5mPct < 85 || c.Within5mPct > 99.5 {
		t.Errorf("five-minute reuse = %.2f%%; production-shaped traffic returns inside five "+
			"minutes almost always, and this figure decides every TTL on the page", c.Within5mPct)
	}
	if c.Within1hPct < c.Within5mPct {
		t.Errorf("one-hour reuse %.2f%% is below five-minute reuse %.2f%%; the horizons nest",
			c.Within1hPct, c.Within5mPct)
	}
	if c.MedianIdleMs <= 0 || c.MedianIdleMs > 120_000 {
		t.Errorf("median idle = %.0f ms; the shape has a p50 of about 15 s", c.MedianIdleMs)
	}
	if c.P90IdleMs < c.MedianIdleMs {
		t.Error("p90 idle is below the median")
	}
	// The single-request conversations are the majority and the page must say so, or it looks
	// like it discarded most of the traffic.
	if out.Coverage.SingleRequestConversations == 0 {
		t.Error("no single-request conversations were counted on a corpus built to be full of them")
	}

	// The histogram and the survival curve are two views of the SAME gaps and must agree at the
	// two horizons. Two panels on one page disagreeing about a percentage is the defect this
	// dashboard has shipped before.
	var inHistogram int64
	for _, b := range out.IdleBands {
		inHistogram += b.N
	}
	if inHistogram != c.WithNext {
		t.Errorf("the histogram holds %d gaps and the cards count %d", inHistogram, c.WithNext)
	}
	at := map[float64]SurvivalPoint{}
	for _, p := range out.Survival {
		at[p.Seconds] = p
	}
	for _, tc := range []struct {
		sec  float64
		want float64
	}{{kvcache.Horizon5m.Seconds(), c.Within5mPct}, {kvcache.Horizon1h.Seconds(), c.Within1hPct}} {
		got := at[tc.sec].ArrivedPct
		if diff := got - tc.want; diff > 0.001 || diff < -0.001 {
			t.Errorf("at %.0f s the survival curve says %.4f%% and the summary card says %.4f%%",
				tc.sec, got, tc.want)
		}
		if at[tc.sec].N != c.WithNext {
			t.Errorf("the survival denominator at %.0f s is %d, the cards' is %d", tc.sec,
				at[tc.sec].N, c.WithNext)
		}
	}
	// The grouped views partition the dataset.
	for _, g := range []struct {
		name   string
		groups []KVCacheGroup
	}{{"by_ttl", out.ByTTL}, {"by_bucket", out.ByBucket}, {"by_user", out.ByUser},
		{"by_model", out.ByModel}} {
		var n int64
		for _, row := range g.groups {
			n += row.Requests
		}
		if n != c.Requests {
			t.Errorf("%s holds %d requests, the dataset has %d — a grouped view that does not "+
				"partition drops rows from the page silently", g.name, n, c.Requests)
		}
	}

	// Now the comparison, over every default arm.
	sim, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cost := map[string]float64{}
	for _, r := range sim.Results {
		cost[r.Strategy] = r.TotalUSD
		t.Logf("%-24s $%9.2f  hit %5.1f%%  writes 5m/1h %5d/%-5d pings %5d  premium $%8.2f",
			r.Strategy, r.TotalUSD, r.HitRate, r.Writes5m, r.Writes1h, r.Pings,
			r.CachePremium)
	}
	for _, s := range sim.Savings {
		t.Logf("%-24s vs %s: $%9.2f (%.2f%%)", s.Strategy, s.Baseline, s.AbsoluteUSD, s.PercentUSD)
	}

	// The ceiling is a ceiling. It reads the true next-request time, so nothing that cannot may
	// beat it — and if anything does, either the DP or the replay is wrong and every savings
	// figure on the page is suspect.
	ceil, ok := cost[KVStrategyOptimal]
	if !ok {
		t.Fatal("the exact ceiling was not among the default arms")
	}
	for name, v := range cost {
		if name == KVStrategyOptimal {
			continue
		}
		if v < ceil-1e-9 {
			t.Errorf("%s costs $%.6f, BELOW the exact ceiling's $%.6f. A policy that cannot see "+
				"the future has beaten the cheapest plan that exists, so one of the two is wrong",
				name, v, ceil)
		}
	}
	// Caching pays for itself on this traffic: not caching a ~125k prefix that returns in seconds
	// is the most expensive thing anybody could do.
	if cost[KVStrategyNoCache] <= cost[KVStrategyFixed5m] {
		t.Errorf("no-cache ($%.2f) is not dearer than a 5-minute tier ($%.2f) on traffic that "+
			"returns in seconds", cost[KVStrategyNoCache], cost[KVStrategyFixed5m])
	}
	// And the one-hour tier LOSES on it, which is the finding the page exists to make visible:
	// it pays 2.0x input to protect prompts a 1.25x write already covered.
	if cost[KVStrategyFixed1h] <= cost[KVStrategyFixed5m] {
		t.Errorf("the 1h tier ($%.2f) is not dearer than the 5m tier ($%.2f) on short gaps; that "+
			"is the whole reason hit rate is not the objective",
			cost[KVStrategyFixed1h], cost[KVStrategyFixed5m])
	}
	// Every savings figure is exactly baseline − strategy, unclamped, and the baseline's own is
	// zero rather than omitted.
	base := cost[sim.Baseline]
	for _, s := range sim.Savings {
		if diff := s.AbsoluteUSD - (base - cost[s.Strategy]); diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: saving $%.9f != baseline $%.9f − strategy $%.9f", s.Strategy,
				s.AbsoluteUSD, base, cost[s.Strategy])
		}
	}
}

// productionShaped builds a dataset with the live snapshot's own shape, deterministically.
//
// A fixed seed, so the same suite run twice produces the same figures: a shape test whose numbers
// move between runs is one nobody can act on, and this one logs its figures.
func productionShaped() []*Event {
	rng := rand.New(rand.NewSource(20260817))
	models := []string{"aws/claude-opus-5", "aws/claude-sonnet-5", "claude-opus-4-8"}
	users := []string{"tenant-a", "tenant-b", "tenant-c"}
	var out []*Event
	ts := kvBase
	for c := 0; c < 177; c++ {
		user := users[c%len(users)]
		model := models[c%len(models)]
		session := fmt.Sprintf("sess-%c-%d", rune('a'+c%26), c)
		// 155 of 177 conversations hold a single request, as 1,551 of 1,772 do on the snapshot.
		turns := 1
		if c%8 == 0 {
			turns = 8 + rng.Intn(50)
		}
		// Conversations start spread across the window, so the hour-of-day bands and the
		// time-of-day grouping have something in every one of them.
		ts += int64(rng.Intn(900_000))
		at := kvBase + int64(c)*int64(time.Hour/time.Millisecond)/6
		for i := 0; i < turns; i++ {
			// The prefix: around the snapshot's 124,845-token median.
			prefix := int64(90_000 + rng.Intn(70_000))
			read, write := prefix, int64(0)
			e := kvEvent(user, session, model, at, read, write)
			if i == 0 {
				// A conversation's first request creates the entry rather than reading one.
				e.CacheRead, e.CacheWrite = 0, prefix
				e.CacheMissReason = CacheColdStart
			}
			// A tenth of the corpus recorded no tier at all, as a pre-column snapshot does, so the
			// coverage panel and the observed-policy arm have something to report.
			if c%10 == 0 {
				e.CacheTTL = ""
			}
			e.UpstreamMs = float64(8_000 + rng.Intn(30_000))
			out = append(out, e)
			// The gap: 95% of them inside five minutes and a median around 15 s, with a tail.
			switch n := rng.Intn(100); {
			case n < 80:
				at += int64(3_000 + rng.Intn(40_000)) // seconds to under a minute
			case n < 95:
				at += int64(60_000 + rng.Intn(240_000)) // one to five minutes
			case n < 99:
				at += int64(300_001 + rng.Intn(3_300_000)) // five minutes to an hour
			default:
				at += int64(3_600_001 + rng.Intn(20_000_000)) // past an hour
			}
		}
	}
	return out
}
