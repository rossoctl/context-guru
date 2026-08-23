package dash

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

// The cost identities, as a pure function plus a proof that it FIRES.
//
// dash/kvcachesemantics_test.go asserts four of these over a real replay, and they were green and
// unproven: the fields they guard live in package kvcache, so the only way to watch them fail was
// to corrupt that package and revert — which is an edit to someone else's tree, and a race with
// whoever else is in it. Four green lines nobody has seen go red are four lines that might be
// checking nothing.
//
// So the identities are a function over a Result, and the negative cases are Results built by hand
// with one field deliberately wrong. No simulator is corrupted, no other package is touched, and
// the check is proven in both directions: TestTheCostIdentitiesHoldOnARealReplay says real replays
// satisfy them, TestEachCostIdentityFires says the function notices when they do not.
//
// Two definitions of an identity would be worse than none, so this is offered as the single one:
// the semantics test's inline assertions can call this and drop their own copies.

// costIdentityFailures returns one message per identity `arm` violates, given the `baseline` it was
// scored against and the Savings that compared them. Empty means every identity holds.
//
// Every identity here is arithmetic that must hold BY CONSTRUCTION — not a measurement, not a
// property of the traffic. A violation is a bug in the cost model or a field that has changed
// meaning while keeping its name, which is the one failure class no nominal wire check can see.
func costIdentityFailures(arm, baseline *kvcache.Result, s kvcache.Savings) []string {
	var out []string
	bad := func(what string, got, want float64) {
		if d := got - want; d > 1e-9 || d < -1e-9 {
			out = append(out, fmt.Sprintf("%s: %s = %.12f, want %.12f", arm.Strategy, what, got, want))
		}
	}
	// The decomposition IS the total. Five components and nothing else, so a sixth cost that
	// forgot to join the sum shows up here rather than as a quietly low total.
	bad("total_usd against the sum of its five parts", arm.TotalUSD,
		arm.FreshInputUSD+arm.CacheReadUSD+arm.CacheWriteUSD+arm.OutputUSD+arm.PingUSD)
	// The premium is a DIFFERENCE, and its sign is the whole meaning: negative means the cache paid
	// for itself. A sign flip here inverts every premium cell on the page.
	bad("cache_premium_usd", arm.CachePremium, arm.TotalUSD-arm.UncachedUSD)
	// Savings are baseline − strategy, unclamped, and the baseline's own is exactly zero.
	bad("absolute_usd", s.AbsoluteUSD, baseline.TotalUSD-arm.TotalUSD)
	bad("baseline_usd", s.BaselineUSD, baseline.TotalUSD)
	bad("strategy_usd", s.StrategyUSD, arm.TotalUSD)
	// Hits and misses partition the requests. Unpriced rows are in the split, so a reader who
	// assumed hit% + miss% = 100 needs this to be true rather than nearly true.
	if got, want := arm.Hits+arm.Misses, arm.Requests; got != want {
		out = append(out, fmt.Sprintf("%s: hits + misses = %d, want %d requests",
			arm.Strategy, got, want))
	}
	// And the two rates are recomputable from those counts. This is the check that catches a
	// percentage that became a FRACTION — a range assertion cannot, because 0.766 is a legal value
	// in [0,100], which is the negative result the semantics pass established.
	if arm.Requests > 0 {
		bad("hit_rate_pct against its own counts", arm.HitRate,
			100*float64(arm.Hits)/float64(arm.Hits+arm.Misses))
		bad("miss_rate_pct against its own counts", arm.MissRate,
			100*float64(arm.Misses)/float64(arm.Hits+arm.Misses))
	}
	return out
}

// Real replays satisfy every identity. Without this the negative test below could pass over a
// function that is simply wrong about what the fields mean.
func TestTheCostIdentitiesHoldOnARealReplay(t *testing.T) {
	db := seedKV(t, productionShaped()...)
	sim, err := db.KVCacheSimulate(allTenants(), KVCacheOptions{}, staticPricer{ibmSonnet},
		KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*kvcache.Result{}
	for _, r := range sim.Results {
		byName[r.Strategy] = r
	}
	base := byName[sim.Baseline]
	if base == nil {
		t.Fatalf("the baseline %q was not among the replayed arms", sim.Baseline)
	}
	if len(sim.Savings) == 0 {
		t.Fatal("no savings were computed; the identities below would be vacuous")
	}
	for _, s := range sim.Savings {
		arm := byName[s.Strategy]
		if arm == nil {
			t.Fatalf("a saving for %q with no result", s.Strategy)
		}
		for _, f := range costIdentityFailures(arm, base, s) {
			t.Error(f)
		}
	}
}

// And each identity FIRES when it is broken. One case per identity, each corrupting exactly one
// field of an otherwise consistent pair, so a check that silently stopped looking cannot pass.
func TestEachCostIdentityFires(t *testing.T) {
	// A consistent arm and baseline, built by hand: 100 fresh + 20 read + 300 write + 50 output +
	// 30 pings = 500, against an uncached equivalent of 900.
	consistent := func() (*kvcache.Result, *kvcache.Result, kvcache.Savings) {
		arm := &kvcache.Result{
			Strategy: "arm", Requests: 10, Hits: 6, Misses: 4, HitRate: 60, MissRate: 40,
			FreshInputUSD: 100, CacheReadUSD: 20, CacheWriteUSD: 300, OutputUSD: 50, PingUSD: 30,
			TotalUSD: 500, UncachedUSD: 900, CachePremium: -400,
		}
		base := &kvcache.Result{Strategy: "base", TotalUSD: 800}
		return arm, base, kvcache.Savings{Strategy: "arm", Baseline: "base",
			BaselineUSD: 800, StrategyUSD: 500, AbsoluteUSD: 300}
	}
	if arm, base, s := consistent(); len(costIdentityFailures(arm, base, s)) != 0 {
		t.Fatalf("the consistent fixture already violates an identity: %v",
			costIdentityFailures(arm, base, s))
	}

	for _, tc := range []struct {
		name    string
		corrupt func(*kvcache.Result, *kvcache.Result, *kvcache.Savings)
		want    string
	}{
		{"a sixth cost that forgot to join the total",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.TotalUSD = 470 },
			"total_usd against the sum of its five parts"},
		{"the premium's sign inverted",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.CachePremium = 400 },
			"cache_premium_usd"},
		{"a saving clamped at zero",
			func(_, _ *kvcache.Result, s *kvcache.Savings) { s.AbsoluteUSD = 0 },
			"absolute_usd"},
		{"the baseline total drifting from the baseline arm",
			func(_, b *kvcache.Result, _ *kvcache.Savings) { b.TotalUSD = 810 },
			"baseline_usd"},
		{"the strategy total drifting from the arm",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.TotalUSD = 500.5 },
			"strategy_usd"},
		{"a request in neither the hits nor the misses",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.Requests = 11 },
			"hits + misses"},
		// The one a range assertion cannot see: 0.6 is a perfectly legal value in [0,100].
		{"a hit rate expressed as a fraction rather than a share",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.HitRate = 0.6 },
			"hit_rate_pct against its own counts"},
		{"a miss rate expressed as a fraction",
			func(a, _ *kvcache.Result, _ *kvcache.Savings) { a.MissRate = 0.4 },
			"miss_rate_pct against its own counts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arm, base, s := consistent()
			tc.corrupt(arm, base, &s)
			got := costIdentityFailures(arm, base, s)
			if len(got) == 0 {
				t.Fatalf("the identity did not fire; %s is unguarded", tc.want)
			}
			var found bool
			for _, g := range got {
				if strings.Contains(g, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("fired, but not on %q: %v", tc.want, got)
			}
		})
	}
}
