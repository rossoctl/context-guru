package kvcache

import (
	"context"
	"testing"
	"time"
)

// chain builds one conversation from a list of gaps, in milliseconds.
func chain(user, conv, model string, startTS int64, gapsMs ...int64) []*Request {
	rows := []*Request{req(1, user, conv, startTS, model)}
	ts := startTS
	for i, g := range gapsMs {
		ts += g
		rows = append(rows, req(int64(i+2), user, conv, ts, model))
	}
	Derive(rows)
	return rows
}

// alwaysDo is a strategy fixed to one action: the way to exercise one branch of the
// simulator without a real policy's thresholds getting in the way.
type alwaysDo struct{ a Action }

func (s alwaysDo) Name() string              { return "always-" + string(s.a) }
func (s alwaysDo) Decide(Observation) Action { return s.a }

// Every unit price in these tests, so an expected total can be read as arithmetic rather
// than as a magic constant.
const (
	pIn        = 3e-6
	pOut       = 15e-6
	pRead      = 0.3e-6
	pWrite5m   = 3.75e-6
	pWrite1h   = 6e-6
	prefix     = 100_000 // req()'s CachedContext
	freshTok   = 100     // req()'s InputTokens
	outTok     = 50      // req()'s OutputTokens
	overhead   = 1*pIn + 1*pOut
	costMiss   = freshTok*pIn + prefix*pWrite5m + outTok*pOut
	costMiss1h = freshTok*pIn + prefix*pWrite1h + outTok*pOut
	costHit    = freshTok*pIn + prefix*pRead + outTok*pOut
	costNone   = (freshTok+prefix)*pIn + outTok*pOut
	costPing   = prefix*pRead + overhead
	costLate   = prefix*pWrite5m + overhead
)

// The no-cache baseline is EXACTLY the uncached cost, which is what makes it a usable
// denominator: every prompt token billed once, at the fresh rate, and no cache premium at all.
func TestNoCacheBaselineIsExactlyTheUncachedCost(t *testing.T) {
	rows := chain("u", "s", "m", base, 60_000)
	r := Simulate(rows, NoCache{}, Config{Prices: testPrices()})

	near(t, "total", r.TotalUSD, 2*costNone)
	near(t, "uncached", r.UncachedUSD, 2*costNone)
	near(t, "cache premium", r.CachePremium, 0)
	if r.Hits != 0 || r.Misses != 2 {
		t.Errorf("hits/misses = %d/%d; nothing is ever cached, so nothing can hit", r.Hits, r.Misses)
	}
	if r.Expires != 2 || r.Writes5m != 0 || r.Writes1h != 0 || r.Pings != 0 {
		t.Errorf("no-cache wrote something: expires=%d w5m=%d w1h=%d pings=%d",
			r.Expires, r.Writes5m, r.Writes1h, r.Pings)
	}
	near(t, "cache read", r.CacheReadUSD, 0)
	near(t, "cache write", r.CacheWriteUSD, 0)
	if r.RetainedMs != 0 {
		t.Errorf("no-cache held an entry for %d ms", r.RetainedMs)
	}
}

// The core of the whole page: a five-minute entry survives a gap inside its lifetime and is
// gone after one outside it.
func TestFixed5mHitsInsideTheLifetimeAndMissesOutside(t *testing.T) {
	rows := chain("u", "s", "m", base, 60_000, 400_000)
	r := Simulate(rows, Fixed5m(), Config{Prices: testPrices()})

	if r.Hits != 1 || r.Misses != 2 {
		t.Fatalf("hits/misses = %d/%d, want 1/2: the 60 s gap holds, the 400 s gap does not",
			r.Hits, r.Misses)
	}
	near(t, "hit rate", r.HitRate, 100.0/3)
	near(t, "miss rate", r.MissRate, 200.0/3)
	near(t, "total", r.TotalUSD, 2*costMiss+costHit)
	if r.Writes5m != 2 {
		t.Errorf("writes_5m = %d, want 2 — a hit that re-marks the same prefix writes nothing",
			r.Writes5m)
	}
	if r.Writes1h != 0 || r.Expires != 0 {
		t.Errorf("unexpected 1h writes (%d) or expiries (%d)", r.Writes1h, r.Expires)
	}
	if r.AvoidedRecomputations != 1 || r.AvoidedTokens != prefix {
		t.Errorf("avoided %d recomputations / %d tokens, want 1 / %d",
			r.AvoidedRecomputations, r.AvoidedTokens, prefix)
	}
	if r.Decisions[ActionWrite5m] != 3 {
		t.Errorf("the arm chose write_5m %d times, want 3 (every request)",
			r.Decisions[ActionWrite5m])
	}
	// The cache was held from the first request to 300 s past the second, and the window ends
	// at the third request: 60 s + 300 s of coverage.
	if want := int64(360_000); r.RetainedMs != want {
		t.Errorf("retained = %d ms, want %d", r.RetainedMs, want)
	}
	// The cache paid for itself here, and CachePremium says so with a NEGATIVE number.
	near(t, "cache premium", r.CachePremium, (2*costMiss+costHit)-3*costNone)
	if r.CachePremium >= 0 {
		t.Errorf("cache premium = %.6f; on this window caching is cheaper than not caching",
			r.CachePremium)
	}
}

// A one-hour tier holds through a gap that lapses at five minutes, and the comparison is
// what the page exists to show.
func TestOneHourTierHoldsAGapThatLapsesAtFiveMinutes(t *testing.T) {
	rows := chain("u", "s", "m", base, 1_800_000) // 30 minutes
	cfg := Config{Prices: testPrices()}
	five := Simulate(rows, Fixed5m(), cfg)
	hour := Simulate(rows, Fixed1h(), cfg)

	if five.Hits != 0 {
		t.Errorf("a 30-minute gap cannot hit a five-minute entry; got %d hits", five.Hits)
	}
	if hour.Hits != 1 {
		t.Errorf("a 30-minute gap is inside an hour; got %d hits", hour.Hits)
	}
	near(t, "fixed-5m total", five.TotalUSD, 2*costMiss)
	near(t, "fixed-1h total", hour.TotalUSD, costMiss1h+costHit)
	if hour.Writes1h != 1 || hour.Writes5m != 0 {
		t.Errorf("the 1h arm wrote %d at 1h and %d at 5m", hour.Writes1h, hour.Writes5m)
	}

	s := Compare(five, hour)
	near(t, "absolute savings", s.AbsoluteUSD, 2*costMiss-(costMiss1h+costHit))
	if !s.Known {
		t.Fatal("a non-zero baseline must yield a percentage")
	}
	near(t, "percentage savings", s.PercentUSD, s.AbsoluteUSD/(2*costMiss)*100)
	if s.HitDelta != 1 {
		t.Errorf("hit delta = %d, want 1", s.HitDelta)
	}
}

// A strategy that costs MORE reports negative savings. Not zero, not omitted.
//
// The 1-hour tier on a conversation that comes back in ten seconds pays 2.0x input to
// protect something a 1.25x write would have held anyway, and a page that clamped this to
// "0% saved" would be hiding the single most useful result the simulator produces.
func TestAStrategyThatCostsMoreReportsNegativeSavings(t *testing.T) {
	rows := chain("u", "s", "m", base, 10_000)
	cfg := Config{Prices: testPrices()}
	five := Simulate(rows, Fixed5m(), cfg)
	hour := Simulate(rows, Fixed1h(), cfg)

	s := Compare(five, hour)
	if s.AbsoluteUSD >= 0 {
		t.Fatalf("the 1h arm should cost MORE here; absolute savings = %.6f", s.AbsoluteUSD)
	}
	near(t, "absolute savings", s.AbsoluteUSD, (costMiss+costHit)-(costMiss1h+costHit))
	if s.PercentUSD >= 0 {
		t.Errorf("percentage savings = %.3f; a negative absolute must give a negative percentage",
			s.PercentUSD)
	}
	near(t, "percentage savings", s.PercentUSD, s.AbsoluteUSD/(costMiss+costHit)*100)
}

// A percentage of nothing is UNDEFINED, not 0%.
func TestPercentageSavingsIsUndefinedOnAZeroBaseline(t *testing.T) {
	rows := chain("u", "s", "unpriced", base, 10_000)
	cfg := Config{Prices: testPrices()}
	base0 := Simulate(rows, NoCache{}, cfg)
	if base0.TotalUSD != 0 {
		t.Fatalf("an unpriced model must contribute no dollars; got %.9f", base0.TotalUSD)
	}
	s := Compare(base0, Simulate(rows, Fixed5m(), cfg))
	if s.Known {
		t.Error("a zero baseline yielded a percentage; a percentage of nothing is not 0%")
	}
	if s.PercentUSD != 0 {
		t.Errorf("percent = %v on an undefined comparison", s.PercentUSD)
	}
}

// An unpriced model's requests are COUNTED — in the totals, in the hit/miss split — and
// contribute nothing to any dollar figure. An unpriced request is not a free one.
func TestUnpricedRequestsAreCountedNotValued(t *testing.T) {
	rows := chain("u", "s", "unpriced", base, 60_000)
	r := Simulate(rows, Fixed5m(), Config{Prices: testPrices()})
	if r.Requests != 2 || r.Unpriced != 2 {
		t.Errorf("requests=%d unpriced=%d, want 2/2", r.Requests, r.Unpriced)
	}
	if r.Hits != 1 || r.Misses != 1 {
		t.Errorf("hits/misses = %d/%d; an unpriced row still hits or misses", r.Hits, r.Misses)
	}
	if r.TotalUSD != 0 || r.CacheWriteUSD != 0 || r.UncachedUSD != 0 {
		t.Errorf("an unpriced model produced dollars: total=%v write=%v uncached=%v",
			r.TotalUSD, r.CacheWriteUSD, r.UncachedUSD)
	}
	if len(r.ByModel) != 1 || r.ByModel[0].Unpriced != 2 {
		t.Errorf("the per-model group did not carry the unpriced count: %+v", r.ByModel)
	}
}

// Keep-alives hold an entry across a gap that would otherwise lapse, and they cost the READ
// rate.
func TestKeepAlivesHoldAnEntryAcrossALongGap(t *testing.T) {
	rows := chain("u", "s", "m", base, 600_000) // 10 minutes
	cfg := Config{Prices: testPrices(), PingIdle: 280 * time.Second, MaxPings: 2}
	r := Simulate(rows, alwaysDo{ActionPing5m}, cfg)

	if r.Pings != 2 {
		t.Fatalf("pings = %d, want 2 at 280 s into a 600 s gap with K=2", r.Pings)
	}
	if r.PingsThatRewrote != 0 {
		t.Errorf("%d pings arrived after the entry lapsed; at 280 s inside a 300 s lifetime none should",
			r.PingsThatRewrote)
	}
	if r.PingsOnOpenSpans != 0 {
		t.Errorf("pings_on_open_spans = %d; the window ends at the last request, so its open "+
			"span has no room for one", r.PingsOnOpenSpans)
	}
	if r.Hits != 1 {
		t.Errorf("the refreshed entry did not survive to the second request: %d hits", r.Hits)
	}
	near(t, "ping cost", r.PingUSD, 2*costPing)
	near(t, "total", r.TotalUSD, costMiss+2*costPing+costHit)

	// And it is cheaper than letting it lapse, which is the whole claim.
	plain := Simulate(rows, Fixed5m(), cfg)
	s := Compare(plain, r)
	if s.AbsoluteUSD <= 0 {
		t.Errorf("pinging cost more than lapsing here: %.6f", s.AbsoluteUSD)
	}
}

// A keep-alive that arrives AFTER the entry lapsed is a cache WRITE, and it is priced and
// counted as one. Pricing it as a read is the arithmetic error that would hide a schedule
// whose interval exceeds the lifetime it is protecting.
func TestALateKeepAliveIsBilledAsAWriteAndCounted(t *testing.T) {
	rows := chain("u", "s", "m", base, 900_000)
	cfg := Config{Prices: testPrices(), PingIdle: 400 * time.Second, MaxPings: 1}
	r := Simulate(rows, alwaysDo{ActionPing5m}, cfg)

	if r.Pings != 1 || r.PingsThatRewrote != 1 {
		t.Fatalf("pings=%d rewrote=%d, want 1/1: a 400 s interval cannot protect a 300 s lifetime",
			r.Pings, r.PingsThatRewrote)
	}
	near(t, "the late ping's cost", r.PingUSD, costLate)
	if r.PingUSD < 10*costPing {
		t.Errorf("the late ping was priced at %.6f, near a read's %.6f — it re-created the "+
			"prefix and must be billed at the creation rate", r.PingUSD, costPing)
	}
	if r.Hits != 0 {
		t.Errorf("the re-created entry lapsed again before the 900 s mark; got %d hits", r.Hits)
	}
}

// A one-hour hold needs far fewer keep-alives than a five-minute one to cover the same span.
// That ping COUNT, not a different per-ping rate, is the cost difference between the two.
func TestAOneHourHoldNeedsFarFewerKeepAlives(t *testing.T) {
	rows := chain("u", "s", "m", base, 4*3_600_000) // four hours
	cfg := Config{Prices: testPrices(), MaxPings: 10}
	hour := Simulate(rows, alwaysDo{ActionPing1h}, cfg)
	five := Simulate(rows, alwaysDo{ActionPing5m}, cfg)

	if hour.Pings != 4 {
		t.Errorf("the 1h arm sent %d pings, want 4 at a 3360 s interval across four hours",
			hour.Pings)
	}
	if five.Pings != 10 {
		t.Errorf("the 5m arm sent %d pings, want its cap of 10", five.Pings)
	}
	if hour.Hits != 1 {
		t.Errorf("four refreshes of a one-hour entry should carry it four hours; %d hits", hour.Hits)
	}
	if five.Hits != 0 {
		t.Errorf("ten refreshes of a five-minute entry reach ~52 minutes, not four hours; %d hits",
			five.Hits)
	}
	// Per ping, the two cost the same: a refresh is a read either way.
	near(t, "one 1h refresh", hour.PingUSD/float64(hour.Pings), costPing)
	near(t, "one 5m refresh", five.PingUSD/float64(five.Pings), costPing)
}

// A `prefix_change` or `cold_start` miss is not something any TTL can rescue: the content
// moved, or there was no entry. Both force a miss whatever the strategy chose, and the count
// is reported so the ceiling on every arm is visible rather than implied.
func TestForcedMissesCannotBeRescuedByAnyTier(t *testing.T) {
	rows := chain("u", "s", "m", base, 10_000, 10_000)
	rows[0].MissReason = "cold_start"
	rows[1].MissReason = "prefix_change"
	for _, s := range []Strategy{Fixed5m(), Fixed1h(), alwaysDo{ActionPing1h}} {
		r := Simulate(rows, s, Config{Prices: testPrices()})
		if r.ForcedMisses != 2 {
			t.Errorf("%s: forced_misses = %d, want 2", s.Name(), r.ForcedMisses)
		}
		if r.Hits != 1 {
			t.Errorf("%s: hits = %d; only the third request (reason 'hit') can hit", s.Name(), r.Hits)
		}
		// And the forced miss pays a full write, not a read.
		if r.CacheReadUSD >= r.CacheWriteUSD {
			t.Errorf("%s: read %.6f vs write %.6f — a prefix change re-creates the prefix",
				s.Name(), r.CacheReadUSD, r.CacheWriteUSD)
		}
	}
}

// The observed-policy arm replays the tier each request actually asked for, and reports how
// much of itself rested on a RECORDED tier rather than on an assumed default.
func TestObservedPolicyReplaysTheRecordedTierAndReportsCoverage(t *testing.T) {
	rows := chain("u", "s", "m", base, 10_000, 10_000)
	rows[0].TTL, rows[0].TTLSource = TTL1h, TTLSourceConfigured
	rows[1].TTL, rows[1].TTLSource = TTL5m, TTLSourceConfigured
	rows[2].TTL, rows[2].TTLSource = TTLNone, TTLSourceUnknown // a row written before the column existed

	obs := NewObserved(rows, TTL5m)
	r := Simulate(rows, obs, Config{Prices: testPrices()})

	if r.Decisions[ActionWrite1h] != 1 || r.Decisions[ActionWrite5m] != 2 {
		t.Errorf("decisions = %v; want one 1h (recorded) and two 5m (one recorded, one assumed)",
			r.Decisions)
	}
	if r.ObservedCoverage == nil {
		t.Fatal("the observed arm must report its own coverage")
	}
	if r.ObservedCoverage.Recorded != 2 || r.ObservedCoverage.Assumed != 1 {
		t.Errorf("coverage = %+v, want 2 recorded / 1 assumed", *r.ObservedCoverage)
	}
	// Every other arm reports no coverage at all rather than a fabricated 100%.
	if Simulate(rows, Fixed5m(), Config{Prices: testPrices()}).ObservedCoverage != nil {
		t.Error("a fixed arm reported an observed-policy coverage it cannot have")
	}
}

// Whether a plain cache HIT refreshes the lifetime is a provider property, and changing it
// changes the result — which is why it is a configurable field and not an assumption.
func TestHitRefreshesTTLIsConfigurableAndChangesTheOutcome(t *testing.T) {
	rows := chain("u", "s", "m", base, 200_000, 250_000)
	refreshing := Simulate(rows, Fixed5m(), Config{Prices: testPrices()})
	sem := DefaultSemantics()
	sem.HitRefreshesTTL = false
	frozen := Simulate(rows, Fixed5m(), Config{Prices: testPrices(), Semantics: sem})

	if refreshing.Hits != 2 {
		t.Errorf("with refreshing hits, both gaps hold: got %d hits", refreshing.Hits)
	}
	if frozen.Hits != 1 {
		t.Errorf("without refreshing hits, the entry dies 300 s after the FIRST request: got %d hits",
			frozen.Hits)
	}
	if frozen.TotalUSD <= refreshing.TotalUSD {
		t.Errorf("the non-refreshing provider must cost more: %.6f vs %.6f",
			frozen.TotalUSD, refreshing.TotalUSD)
	}
}

// Retention is the UNION of the intervals an entry was alive, clipped to the window. A sum of
// lifetimes would double-count every overlapping refresh.
func TestRetainedTimeIsAUnionNotASumOfLifetimes(t *testing.T) {
	rows := chain("u", "s", "m", base, 60_000)
	r := Simulate(rows, Fixed5m(), Config{Prices: testPrices(), WindowEnd: base + 400_000})
	// Two five-minute lifetimes were started, 60 s apart: their union is 360 s, their sum 600 s.
	if want := int64(360_000); r.RetainedMs != want {
		t.Errorf("retained = %d ms, want %d (the union); the sum of lifetimes would be 600000",
			r.RetainedMs, want)
	}
}

// An open idle span — a conversation whose last request is inside the window — has a length
// nobody knows. Its pings are bounded by the window's end and counted APART, because they
// rest on an assumption the closed spans do not need.
func TestPingsOnAnOpenSpanAreBoundedAndCountedApart(t *testing.T) {
	rows := chain("u", "s", "m", base, 10_000)
	cfg := Config{Prices: testPrices(), PingIdle: 280 * time.Second, MaxPings: 2,
		WindowEnd: base + 10_000 + 900_000}
	r := Simulate(rows, alwaysDo{ActionPing5m}, cfg)
	if r.Pings != 2 || r.PingsOnOpenSpans != 2 {
		t.Errorf("pings=%d open=%d; the only span with room for a ping is the open one",
			r.Pings, r.PingsOnOpenSpans)
	}
	// With no window end, an open span costs nothing — the conservative direction, and the
	// one that cannot inflate a saving.
	bare := Simulate(rows, alwaysDo{ActionPing5m}, Config{Prices: testPrices(),
		PingIdle: 280 * time.Second, MaxPings: 2})
	if bare.Pings != 0 {
		t.Errorf("with the window ending at the last request, an open span attracts %d pings",
			bare.Pings)
	}
}

func TestPingsPerSpan(t *testing.T) {
	const idle = 280 * time.Second
	for _, tc := range []struct {
		gap  time.Duration
		idle time.Duration
		max  int
		want int
	}{
		{0, idle, 2, 0},
		{idle, idle, 2, 0}, // a gap no longer than the interval attracts none
		{idle + time.Millisecond, idle, 2, 1},
		{2 * idle, idle, 2, 2},
		{600 * time.Second, idle, 2, 2},
		{10000 * time.Second, idle, 2, 2}, // the cap binds
		{10000 * time.Second, idle, 0, 0}, // K=0 sends nothing
		{10000 * time.Second, 0, 2, 0},    // no interval sends nothing
		{4 * time.Hour, 3360 * time.Second, 10, 4},
	} {
		if got := PingsPerSpan(tc.gap, tc.idle, tc.max); got != tc.want {
			t.Errorf("PingsPerSpan(%v, %v, %d) = %d, want %d", tc.gap, tc.idle, tc.max, got, tc.want)
		}
	}
}

// The historical-probability arm uses the account's OWN closed gaps, and its prefix gate
// stops it caching something too small to repay a write.
func TestHistoricalProbabilityActsOnItsOwnHistoryAndItsPrefixGate(t *testing.T) {
	// Eight ten-second gaps: this user always comes back inside five minutes.
	rows := chain("u", "s", "m", base, 10_000, 10_000, 10_000, 10_000, 10_000, 10_000, 10_000)
	r := Simulate(rows, HistoricalProbability{}, Config{Prices: testPrices()})
	if r.Decisions[ActionWrite5m] == 0 {
		t.Errorf("an account that always returns in 10 s was never given a 5m write: %v", r.Decisions)
	}
	if r.Decisions[ActionWrite1h] != 0 || r.Decisions[ActionPing1h] != 0 {
		t.Errorf("a 10-second working pattern does not need an hour of protection: %v", r.Decisions)
	}

	// The same traffic with a prefix under the gate is never cached at all.
	small := chain("u", "s", "m", base, 10_000, 10_000, 10_000, 10_000, 10_000, 10_000, 10_000)
	for _, q := range small {
		q.CachedContext = 500
	}
	g := Simulate(small, HistoricalProbability{}, Config{Prices: testPrices()})
	if g.Decisions[ActionExpire] != int64(len(small)) {
		t.Errorf("a 500-token prefix was cached: %v", g.Decisions)
	}
	// Every arm reports which fallback level its statistics could answer at, so a reader can
	// see how much of it was actually personalised.
	if len(r.StatsLevels) == 0 {
		t.Error("no statistics coverage was reported")
	}
	if r.StatsLevels[LevelNone] != 1 {
		t.Errorf("stats levels = %v; exactly the first decision has no closed gap behind it",
			r.StatsLevels)
	}
}

// A long-idle account is given the LONG hold, and which form of it — a 1h write or a 5m
// write plus refreshes — is decided by the rates rather than in advance.
func TestHistoricalProbabilityChoosesTheCheaperLongHold(t *testing.T) {
	// Gaps of about twenty minutes: outside five minutes, inside an hour.
	rows := chain("u", "s", "m", base, 1_200_000, 1_200_000, 1_200_000, 1_200_000, 1_200_000,
		1_200_000, 1_200_000)
	r := Simulate(rows, HistoricalProbability{}, Config{Prices: testPrices()})
	long := r.Decisions[ActionWrite1h] + r.Decisions[ActionPing5m]
	if long == 0 {
		t.Fatalf("a twenty-minute working pattern got no long hold: %v", r.Decisions)
	}
	// At these rates two refreshes (2 x $0.030018) are cheaper than the 1h premium
	// (100k x (6e-6 - 3.75e-6) = $0.225), so the ping path wins — computed, not assumed.
	if r.Decisions[ActionPing5m] == 0 {
		t.Errorf("the cheaper long hold was not chosen: %v", r.Decisions)
	}
	// Raise the 1h multiplier's competitor by making refreshes expensive and it flips.
	dear := NewPriceList(context.Background(), []string{"m"},
		stubPricer{"m": {Input: 3e-6, Output: 15e-6, CacheRead: 3e-6, CacheWrite: 3.75e-6}},
		Multipliers{}, nil)
	q := Simulate(rows, HistoricalProbability{}, Config{Prices: dear})
	if q.Decisions[ActionWrite1h] == 0 {
		t.Errorf("with refreshes at the input rate the 1h write must win: %v", q.Decisions)
	}
}

// stubPredictor is a learned model's stand-in: it answers the one question the seam asks.
type stubPredictor struct{ p5, p1h float64 }

func (s stubPredictor) ReuseProbability(_ Observation, horizon time.Duration) (float64, bool) {
	if horizon <= Horizon5m {
		return s.p5, true
	}
	return s.p1h, true
}

// A predictor plugs in without the simulator or the results changing shape. This is the
// property that makes the interface worth having.
func TestACustomPredictorPlugsInWithoutChangingTheSimulator(t *testing.T) {
	rows := chain("u", "s", "m", base, 1_800_000)
	cfg := Config{Prices: testPrices()}

	confident := Simulate(rows, Custom{Label: "learned", Predictor: stubPredictor{p5: 0.95}}, cfg)
	if confident.Strategy != "learned" {
		t.Errorf("strategy name = %q, want the supplied label", confident.Strategy)
	}
	if confident.Decisions[ActionWrite5m] != 2 {
		t.Errorf("a 95%% five-minute prediction must give a 5m write: %v", confident.Decisions)
	}
	// A predictor that says the conversation is over lets the entry expire.
	done := Simulate(rows, Custom{Predictor: stubPredictor{p5: 0.01, p1h: 0.02}}, cfg)
	if done.Decisions[ActionExpire] != 2 {
		t.Errorf("a 1%% prediction must let the cache expire: %v", done.Decisions)
	}
	near(t, "an expiring arm costs the uncached price", done.TotalUSD, 2*costNone)

	// A long-horizon prediction takes the long hold, and AlwaysPing forces the ping path so
	// an operator can measure it specifically.
	pinged := Simulate(rows, Custom{Predictor: stubPredictor{p5: 0.1, p1h: 0.9}, AlwaysPing: true},
		Config{Prices: testPrices(), PingIdle: 280 * time.Second, MaxPings: 8})
	if pinged.Decisions[ActionPing5m] == 0 || pinged.Pings == 0 {
		t.Errorf("AlwaysPing did not take the ping path: %v (%d pings)",
			pinged.Decisions, pinged.Pings)
	}

	// And the last seam: a whole decision function.
	fn := Simulate(rows, Custom{Label: "callback",
		Decider: func(Observation) Action { return ActionWrite1h }}, cfg)
	if fn.Decisions[ActionWrite1h] != 2 {
		t.Errorf("the supplied decider was not used: %v", fn.Decisions)
	}
}

// Every arm is grouped by user and by model, and the groups reconcile with the totals.
func TestResultsAreGroupedByUserAndModelAndReconcile(t *testing.T) {
	var rows []*Request
	rows = append(rows, chain("alice", "a1", "m", base, 60_000)...)
	for _, r := range chain("bob", "b1", "m", base+5_000, 60_000) {
		r.ID += 100
		rows = append(rows, r)
	}
	Derive(rows)
	r := Simulate(rows, Fixed5m(), Config{Prices: testPrices()})

	if len(r.ByUser) != 2 {
		t.Fatalf("by_user has %d groups, want 2", len(r.ByUser))
	}
	var sum float64
	var reqs, hits int64
	for _, g := range r.ByUser {
		sum += g.TotalUSD
		reqs += g.Requests
		hits += g.Hits
	}
	near(t, "the per-user groups sum to the total", sum, r.TotalUSD)
	if reqs != r.Requests || hits != r.Hits {
		t.Errorf("groups hold %d requests / %d hits, totals say %d / %d",
			reqs, hits, r.Requests, r.Hits)
	}
	if r.Conversations != 2 {
		t.Errorf("conversations = %d, want 2", r.Conversations)
	}
	// Both users are on one model, so there is one model group holding everything.
	if len(r.ByModel) != 1 || r.ByModel[0].Requests != r.Requests {
		t.Errorf("by_model = %+v", r.ByModel)
	}
}

// The latency differential is measured from the window's own rows, and it is ABSENT rather
// than estimated when either population is too small to mean anything.
func TestLatencyDifferentialNeedsBothPopulations(t *testing.T) {
	rows := chain("u", "s", "m", base, 10_000)
	rows[0].UpstreamMs, rows[0].Hit = 20_000, false
	rows[1].UpstreamMs, rows[1].Hit = 1_000, true
	if l := MeasureLatency(rows); l.Known {
		t.Errorf("two rows produced a latency differential: %+v", l)
	}

	var many []*Request
	for i := int64(0); i < 60; i++ {
		r := req(i+1, "u", "s", base+i*10_000, "m")
		r.Hit = i%2 == 0
		r.MissReason = "hit"
		if !r.Hit {
			r.MissReason = "ttl_expiry"
		}
		r.UpstreamMs = 1_000
		if !r.Hit {
			r.UpstreamMs = 3_000
		}
		many = append(many, r)
	}
	Derive(many)
	l := MeasureLatency(many)
	if !l.Known {
		t.Fatalf("30 of each population is enough: %+v", l)
	}
	near(t, "per-miss latency", l.PerMissMs, 2_000)
	if l.HitN != 30 || l.MissN != 30 {
		t.Errorf("populations = %d hits / %d misses", l.HitN, l.MissN)
	}
	// A row with no recorded upstream time is ABSENT from both means, not a zero in one.
	many[0].UpstreamMs = 0
	if l2 := MeasureLatency(many); l2.HitN != 29 {
		t.Errorf("an unrecorded latency was counted: %d hits", l2.HitN)
	}

	// And the comparison carries it through only when it is known.
	cfg := Config{Prices: testPrices()}
	s := Compare(Simulate(many, NoCache{}, cfg), Simulate(many, Fixed1h(), cfg))
	if !s.LatencyKnown || s.LatencyAvoidedMs <= 0 {
		t.Errorf("a measurable differential and a positive hit delta must give an avoided "+
			"latency: %+v", s)
	}
}

// Simulate sorts defensively and is deterministic: the same window gives the same figures
// twice, whatever order the caller hands the rows in.
func TestSimulateIsDeterministicAndSortsDefensively(t *testing.T) {
	rows := chain("u", "s", "m", base, 60_000, 400_000, 30_000)
	shuffled := []*Request{rows[2], rows[0], rows[3], rows[1]}
	cfg := Config{Prices: testPrices()}
	a := Simulate(rows, Fixed5m(), cfg)
	b := Simulate(shuffled, Fixed5m(), cfg)
	if a.TotalUSD != b.TotalUSD || a.Hits != b.Hits || a.RetainedMs != b.RetainedMs {
		t.Errorf("an out-of-order slice replayed differently:\n %+v\n %+v", a, b)
	}
	c := Simulate(rows, Fixed5m(), cfg)
	if a.TotalUSD != c.TotalUSD || a.Pings != c.Pings {
		t.Error("two replays of the same window disagreed")
	}
}

// Every strategy describes itself, because the results table shows the sentence beside the
// name and a reader cannot compare arms they cannot tell apart.
func TestEveryShippedStrategyDescribesItself(t *testing.T) {
	for _, s := range []Strategy{NoCache{}, Fixed5m(), Fixed1h(),
		NewObserved(nil, TTL5m), HistoricalProbability{}, Custom{}} {
		d, ok := s.(Describer)
		if !ok {
			t.Errorf("%s has no description", s.Name())
			continue
		}
		if len(d.Describe()) < 20 {
			t.Errorf("%s describes itself as %q", s.Name(), d.Describe())
		}
	}
	// And the names are stable keys, so a result can be grouped by them.
	if (NoCache{}).Name() != "no-cache" || Fixed5m().Name() != "fixed-5m" ||
		Fixed1h().Name() != "fixed-1h" {
		t.Error("a strategy name moved; the dashboard groups results by these keys")
	}
}
