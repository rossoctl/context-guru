package kvcache

import (
	"testing"
	"time"
)

// stubStats is a Stats a test can move by hand, standing in for History's own mutation
// across a replay: the test changes its fields between Decide calls exactly the way an
// account's own closed gaps change History between decisions.
type stubStats struct {
	p     float64
	n     int
	level string
}

func (s *stubStats) ReuseWithin(user, model string, b Bucket, d time.Duration) (float64, int, string) {
	return s.p, s.n, s.level
}

func (s *stubStats) MedianIdle(user, model string, b Bucket) (time.Duration, int, string) {
	return 0, s.n, s.level
}

// stickyPrice is "m"'s rates from testPrices(), whose numbers are used throughout: breakeven
// = (Write1h-Write5m)/(Write5m-CacheRead) = (6e-6-3.75e-6)/(3.75e-6-0.3e-6) ~= 65.2%, the same
// figure cacheinject.go's own TTL doc derives from the multipliers directly.
func stickyPrice(t *testing.T) Pricing {
	t.Helper()
	p := testPrices().For("m")
	if !p.Known {
		t.Fatal("fixture price for \"m\" is not known")
	}
	return p
}

// (a) The decision made at turn 1 is held for every later turn, even when the account's own
// statistics swing hard in the other direction mid-replay — which is the whole point of a
// STICKY arm: a live History would genuinely move like this as more of the window replays,
// and the arm must not care.
func TestStickySessionCommitsOnceAndHoldsItThroughAStatsSwing(t *testing.T) {
	price := stickyPrice(t)
	stats := &stubStats{p: 0.90, n: 10, level: LevelUser} // well above the ~65.2% break-even
	s := NewStickySession1h()

	mk := func(turn int, id int64) Observation {
		return Observation{User: "u", Conversation: "c1", Model: "m", RequestID: id,
			Bucket: BucketMorning, Turn: turn, Pricing: price, Stats: stats}
	}
	if got := s.Decide(mk(1, 1)); got != ActionWrite1h {
		t.Fatalf("turn 1 with p=%.2f (above break-even) decided %v, want write_1h", stats.p, got)
	}

	// The account's own history now says the opposite: if this were re-decided, it would
	// choose 5m. It must not be re-decided.
	stats.p, stats.n, stats.level = 0.0, 50, LevelUser
	if got := s.Decide(mk(2, 2)); got != ActionWrite1h {
		t.Errorf("turn 2 decided %v after the stats swung to p=%.2f; want the turn-1 "+
			"commitment (write_1h) held regardless", got, stats.p)
	}
	if got := s.Decide(mk(3, 3)); got != ActionWrite1h {
		t.Errorf("turn 3 decided %v; want write_1h still — a sticky arm never re-decides", got)
	}
}

// (b) A different conversation for the same user gets its own decision, made independently
// at ITS first request against whatever the account's stats say at that moment — and one
// conversation's commitment does not leak into another's.
func TestStickySessionDecidesPerConversationNotPerUser(t *testing.T) {
	price := stickyPrice(t)
	stats := &stubStats{p: 0.90, n: 10, level: LevelUser}
	s := NewStickySession1h()

	a1 := Observation{User: "u", Conversation: "conv-a", Model: "m", RequestID: 1,
		Bucket: BucketMorning, Turn: 1, Pricing: price, Stats: stats}
	if got := s.Decide(a1); got != ActionWrite1h {
		t.Fatalf("conv-a turn 1 decided %v, want write_1h (p=%.2f clears break-even)", got, stats.p)
	}

	// By the time conv-b starts, the account's own history has moved on.
	stats.p, stats.n, stats.level = 0.10, 20, LevelUser
	b1 := Observation{User: "u", Conversation: "conv-b", Model: "m", RequestID: 2,
		Bucket: BucketMorning, Turn: 1, Pricing: price, Stats: stats}
	if got := s.Decide(b1); got != ActionWrite5m {
		t.Fatalf("conv-b turn 1 decided %v, want write_5m (p=%.2f is below break-even)", got, stats.p)
	}

	// conv-a's own commitment is unaffected by conv-b's decision or by the stats move.
	a2 := a1
	a2.Turn, a2.RequestID = 2, 3
	if got := s.Decide(a2); got != ActionWrite1h {
		t.Errorf("conv-a turn 2 decided %v; want its own turn-1 commitment (write_1h) held",
			got)
	}
}

// (c) The decision cannot see the future: it is a function of Pricing and Stats alone, and
// two otherwise-identical first requests must decide identically however their OTHER
// fields — timestamp, cached size, current tier, the idle gap, the turn count — differ. A
// strategy that let any of those leak into the decision would be reading facts about the
// request itself (or, worse, values a caller could set from what turns out to happen next)
// rather than the leak-free statistics Stats already guarantees are past-only.
func TestStickySessionDecisionDependsOnlyOnPricingAndStats(t *testing.T) {
	price := stickyPrice(t)
	stats := &stubStats{p: 0.90, n: 10, level: LevelUser}
	s := NewStickySession1h()

	a := Observation{User: "u", Conversation: "conv-a", Model: "m", RequestID: 1,
		Bucket: BucketMorning, Turn: 1, Now: 1_000, SinceLastMs: 0,
		CachedTokens: 1_000, TTL: TTLNone, ExpiresAt: 0, Pricing: price, Stats: stats}
	b := Observation{User: "u", Conversation: "conv-b", Model: "m", RequestID: 2,
		Bucket: BucketMorning, Turn: 1, Now: 999_999_999, SinceLastMs: 3_600_000,
		CachedTokens: 9_999_999, TTL: TTL1h, ExpiresAt: 1, Pricing: price, Stats: stats}

	gotA, gotB := s.Decide(a), s.Decide(b)
	if gotA != gotB {
		t.Errorf("two fresh conversations with identical Pricing/Stats decided differently "+
			"(%v vs %v) despite differing only in fields the decision must not use", gotA, gotB)
	}
	if gotA != ActionWrite1h {
		t.Errorf("got %v, want write_1h: p=%.2f clears the break-even", gotA, stats.p)
	}
}

// The fallback ladder: no Stats at all, or Stats with nothing at any level (LevelNone), or
// an unpriced model, must all default to the 5-minute tier rather than guess a probability
// from nothing.
func TestStickySessionFallsBackTo5mWithNoUsableStatsOrPricing(t *testing.T) {
	price := stickyPrice(t)
	base := Observation{User: "u", Model: "m", Turn: 1, Bucket: BucketMorning, Pricing: price}

	noStats := base
	noStats.Conversation, noStats.Stats = "no-stats", nil
	if got := NewStickySession1h().Decide(noStats); got != ActionWrite5m {
		t.Errorf("nil Stats decided %v, want write_5m", got)
	}

	levelNone := base
	levelNone.Conversation = "level-none"
	levelNone.Stats = &stubStats{p: 0.99, n: 0, level: LevelNone}
	if got := NewStickySession1h().Decide(levelNone); got != ActionWrite5m {
		t.Errorf("LevelNone stats (n=0) decided %v, want write_5m even though p=0.99", got)
	}

	unpriced := base
	unpriced.Conversation, unpriced.Stats = "unpriced", &stubStats{p: 0.99, n: 10, level: LevelUser}
	unpriced.Pricing = Pricing{Model: "unpriced"} // Known=false
	if got := NewStickySession1h().Decide(unpriced); got != ActionWrite5m {
		t.Errorf("unpriced model decided %v, want write_5m", got)
	}
}

// The arm is buildable from the registry and shaped the way every other Strategy is.
func TestStickySessionIsRegisteredAndDescribesItself(t *testing.T) {
	s, err := NewStrategy(StrategyStickySession1h, nil, Config{})
	if err != nil {
		t.Fatalf("NewStrategy(%q): %v", StrategyStickySession1h, err)
	}
	if s.Name() != StrategyStickySession1h {
		t.Errorf("Name() = %q, want %q", s.Name(), StrategyStickySession1h)
	}
	d, ok := s.(Describer)
	if !ok || d.Describe() == "" {
		t.Error("the arm has no description; the page renders it beside the number")
	}
	var found bool
	for _, spec := range Registry() {
		if spec.Name == StrategyStickySession1h {
			found = true
		}
	}
	if !found {
		t.Error("sticky-session-1h is built but not in Registry()")
	}
}

// Sanity: the break-even formula genuinely gates the decision, not just the fallback ladder.
func TestStickySessionBreakEvenGatesTheCommitment(t *testing.T) {
	price := stickyPrice(t)
	breakeven := sticky1hBreakeven(price)
	if breakeven <= 0 || breakeven >= 1 {
		t.Fatalf("fixture break-even = %.4f, want a value in (0,1) for this test to mean anything",
			breakeven)
	}
	below := Observation{User: "u", Conversation: "below", Model: "m", Turn: 1,
		Bucket: BucketMorning, Pricing: price, Stats: &stubStats{p: breakeven - 0.05, n: 10, level: LevelUser}}
	above := Observation{User: "u", Conversation: "above", Model: "m", Turn: 1,
		Bucket: BucketMorning, Pricing: price, Stats: &stubStats{p: breakeven + 0.05, n: 10, level: LevelUser}}
	if got := NewStickySession1h().Decide(below); got != ActionWrite5m {
		t.Errorf("p just below break-even decided %v, want write_5m", got)
	}
	if got := NewStickySession1h().Decide(above); got != ActionWrite1h {
		t.Errorf("p just above break-even decided %v, want write_1h", got)
	}
}
