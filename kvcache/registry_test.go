package kvcache

import (
	"context"
	"math"
	"testing"
	"time"
)

// dataset builds a trajectory set with gaps spread across both horizons, so that different
// arms genuinely win on different rows: short gaps where a 5-minute write is enough, gaps in
// the band only a keep-alive or a 1-hour tier reaches, gaps past an hour that nothing
// rescues, and trajectories that simply stop.
func dataset(t *testing.T) ([]*Request, Config) {
	t.Helper()
	const start = int64(1_786_967_311_185)
	gaps := []int64{5_000, 45_000, 299_999, 300_001, 600_000, 900_000, 2_400_000,
		3_599_999, 3_600_001, 20_000_000}
	var reqs []*Request
	var id int64
	for c, gap := range gaps {
		conv := "conv-" + string(rune('a'+c))
		ts := start + int64(c)*1_000
		// Prefixes big enough that the write premium is the dominant term, which is the regime
		// the production corpus is in (median billed prefix 124,845 tokens).
		for turn := 0; turn < 4; turn++ {
			id++
			reason := "hit"
			if turn == 0 {
				reason = "cold_start"
			}
			reqs = append(reqs, &Request{
				ID: id, User: "acct-1", ConversationID: conv, TS: ts,
				HourUTC: time.UnixMilli(ts).UTC().Hour(), Bucket: BucketAt(ts),
				Model: "m", InputTokens: 120, OutputTokens: 45,
				CachedContext: 120_000 + int64(turn)*20_000, MissReason: reason,
				TTL: TTL5m, TTLSource: TTLSourceConfigured,
			})
			ts += gap
		}
	}
	Derive(reqs)
	var end int64
	for _, r := range reqs {
		if r.TS > end {
			end = r.TS
		}
	}
	in, out, cr, w5, w1 := 3.8e-6, 19e-6, 0.38e-6, 4.75e-6, 7.6e-6
	pin, pout := int64(1), int64(1)
	prices := NewPriceList(context.Background(), []string{"m"}, nil, Multipliers{},
		map[string]Override{"m": {Input: &in, Output: &out, CacheRead: &cr, Write5m: &w5,
			Write1h: &w1, PingInputTokens: &pin, PingOutputTokens: &pout}})
	return reqs, Config{Prices: prices, WindowEnd: end}
}

// The ceiling has to BE a ceiling. Every arm in the registry is replayed and none of them may
// come out cheaper than the exact optimum.
//
// This is the assertion that caught the original implementation, which was a greedy per-row
// rule and scored BELOW a plain keep-alive — impossible for a bound, and invisible in any
// single number. It is a property, not an example: a future arm added to the registry is
// covered the day it lands.
func TestOptimalIsALowerBoundOnEveryOtherArm(t *testing.T) {
	reqs, cfg := dataset(t)
	best := Simulate(reqs, NewOptimal(reqs, cfg), cfg)
	if best.TotalUSD <= 0 {
		t.Fatalf("the optimum priced this dataset at %.6f; the fixture is not exercising cost",
			best.TotalUSD)
	}
	var cheaper int
	for _, spec := range Registry() {
		if spec.Name == StrategyOptimal || spec.Name == StrategyReplay {
			continue
		}
		s, err := NewStrategy(spec.Name, reqs, cfg)
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		got := Simulate(reqs, s, cfg)
		// A cent of slack for float summation order, not a licence to be beaten.
		if got.TotalUSD < best.TotalUSD-0.01 {
			t.Errorf("%s costs $%.6f, BELOW the exact optimum's $%.6f. The ceiling is not a "+
				"ceiling, so every saving quoted against it is wrong.",
				spec.Name, got.TotalUSD, best.TotalUSD)
		}
		if got.TotalUSD > best.TotalUSD+0.01 {
			cheaper++
		}
		// And the comparison must be signed, never clamped: an arm dearer than the bound has a
		// negative saving against it.
		if sv := Compare(got, best); sv.AbsoluteUSD < 0 {
			t.Errorf("%s: the optimum reports a NEGATIVE saving (%.6f) against a dearer arm",
				spec.Name, sv.AbsoluteUSD)
		}
	}
	if cheaper == 0 {
		t.Error("no arm was dearer than the optimum, so this dataset cannot tell a real bound " +
			"from a coincidence")
	}
	// A random plan must not beat it either — the DP is over the same five actions, so a
	// hand-picked sequence is a direct challenge to its optimality.
	plans := map[int64]Action{}
	for i, r := range reqs {
		plans[r.ID] = Actions[i%len(Actions)]
	}
	if got := Simulate(reqs, NewReplay("rr", plans, ActionExpire), cfg); got.TotalUSD < best.TotalUSD-0.01 {
		t.Errorf("a round-robin plan costs $%.6f, below the optimum's $%.6f",
			got.TotalUSD, best.TotalUSD)
	}
}

// The registry is a wire contract: the names are in a query parameter, a JSON key and the
// offline evaluator's own arm table.
func TestRegistryIsBuildableAndUnique(t *testing.T) {
	reqs, cfg := dataset(t)
	seen := map[string]bool{}
	var baselines, unreachable int
	for _, spec := range Registry() {
		if seen[spec.Name] {
			t.Errorf("duplicate arm name %q", spec.Name)
		}
		seen[spec.Name] = true
		if spec.Description == "" {
			t.Errorf("%s has no description; the page renders it beside the number", spec.Name)
		}
		if spec.Baseline {
			baselines++
		}
		if spec.Unreachable {
			unreachable++
		}
		s, err := NewStrategy(spec.Name, reqs, cfg)
		if spec.Name == StrategyReplay {
			if err == nil {
				t.Error("replay must refuse to be built without its action list")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		if s.Name() != spec.Name {
			t.Errorf("registry calls it %q, the strategy calls itself %q", spec.Name, s.Name())
		}
	}
	if baselines != 1 {
		t.Errorf("%d arms are marked Baseline; exactly one is the honest denominator", baselines)
	}
	if unreachable != 1 {
		t.Errorf("%d arms are marked Unreachable; only the exact optimum reads the future",
			unreachable)
	}
	if _, err := NewStrategy("no-such-arm", reqs, cfg); err == nil {
		t.Error("an unknown arm name must be an error, not a silent default")
	}
	if len(StrategyNames()) != len(registry) {
		t.Error("StrategyNames disagrees with the registry")
	}
}

// Replay is the seam an offline predictor is scored through, so it must score exactly what it
// was handed and REPORT what it was not.
func TestReplayScoresWhatItWasGivenAndCountsWhatItWasNot(t *testing.T) {
	reqs, cfg := dataset(t)
	// Half the rows named, the rest left out.
	plan := map[int64]Action{}
	for i, r := range reqs {
		if i%2 == 0 {
			plan[r.ID] = ActionWrite1h
		}
	}
	r := NewReplay("offline-model", plan, ActionWrite5m)
	got := Simulate(reqs, r, cfg)
	if got.Strategy != "offline-model" {
		t.Errorf("results grouped under %q, want the supplied label", got.Strategy)
	}
	if want := int64(len(reqs) - len(plan)); r.Unanswered() != want {
		t.Errorf("Unanswered = %d, want %d — a list that covers half the window must say so",
			r.Unanswered(), want)
	}
	if got.Decisions[ActionWrite1h] != int64(len(plan)) {
		t.Errorf("the plan named %d 1-hour writes, the replay made %d",
			len(plan), got.Decisions[ActionWrite1h])
	}
	if got.Decisions[ActionWrite5m] != int64(len(reqs)-len(plan)) {
		t.Errorf("the fallback was not applied to the rows the plan omitted: %v", got.Decisions)
	}
	// A full plan must reproduce the arm it was copied from, exactly. This is what makes the
	// seam trustworthy: scoring a policy through Replay is the same as running it.
	fixed := Simulate(reqs, Fixed1h(), cfg)
	all := map[int64]Action{}
	for _, q := range reqs {
		all[q.ID] = ActionWrite1h
	}
	via := Simulate(reqs, NewReplay("as-fixed-1h", all, ActionExpire), cfg)
	if math.Abs(via.TotalUSD-fixed.TotalUSD) > 1e-9 {
		t.Errorf("replaying fixed-1h's own actions cost $%.9f, the arm itself $%.9f",
			via.TotalUSD, fixed.TotalUSD)
	}
	if via.Hits != fixed.Hits || via.Pings != fixed.Pings || via.Writes1h != fixed.Writes1h {
		t.Errorf("the replay diverged on counts: %d/%d hits, %d/%d pings, %d/%d 1h writes",
			via.Hits, fixed.Hits, via.Pings, fixed.Pings, via.Writes1h, fixed.Writes1h)
	}
}

// The keep-alive arms must actually send keep-alives, and the 1-hour one must need far fewer
// of them for the same traffic — the ping-count half of the 5m-versus-1h trade.
func TestKeepAliveArmsPingAndTheHourlyOnePingsLess(t *testing.T) {
	reqs, cfg := dataset(t)
	five := Simulate(reqs, KeepAlive5m(), cfg)
	hour := Simulate(reqs, KeepAlive1h(), cfg)
	if five.Pings == 0 || hour.Pings == 0 {
		t.Fatalf("a keep-alive arm sent no pings (5m=%d, 1h=%d)", five.Pings, hour.Pings)
	}
	if hour.Pings >= five.Pings {
		t.Errorf("the 1-hour arm sent %d pings and the 5-minute arm %d; an hourly entry needs "+
			"refreshing far less often", hour.Pings, five.Pings)
	}
	// The hourly arm's coverage CONTAINS the five-minute arm's, so it cannot hit less often.
	if hour.HitRate < five.HitRate-1e-9 {
		t.Errorf("the 1-hour arm hit %.2f%% against the 5-minute arm's %.2f%%; an hour of "+
			"coverage contains five minutes of it", hour.HitRate, five.HitRate)
	}
	// The other half of the trade is the RATE, per written token — deliberately not asserted
	// through CacheWriteUSD, which is rate x volume: the hourly arm misses less, so it writes
	// FEWER tokens, and its total write bill can legitimately come out lower. Asserting on the
	// total was this test's own first bug, and it is the same confusion as reading a hit rate
	// as a cost.
	p := cfg.Prices.For("m")
	if p.Write1h <= p.Write5m {
		t.Errorf("write_1h %.9f is not dearer per token than write_5m %.9f", p.Write1h, p.Write5m)
	}
	t.Logf("the trade on this dataset: 5m $%.2f (%d pings, %.1f%% hit) against "+
		"1h $%.2f (%d pings, %.1f%% hit)", five.TotalUSD, five.Pings, five.HitRate,
		hour.TotalUSD, hour.Pings, hour.HitRate)
	if five.Writes5m == 0 || hour.Writes1h == 0 {
		t.Errorf("the arms did not write at their own tiers (5m=%d, 1h=%d)",
			five.Writes5m, hour.Writes1h)
	}
}
