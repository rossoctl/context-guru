package predictor

import (
	"reflect"
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

func TestReplayIsDeterministic(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u1", "s1", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(2, "u1", "s1", base+30_000, "m", 100_000, "end_turn", "hit", 1, 0),
		req(3, "u2", "s2", base+15_000, "m", 50_000, "end_turn", "hit", 0, 2),
	})
	freg := DefaultFeatures()
	preds := NewPredictors()
	if err := preds.Register(Registered{ID: "a", Version: "v1", Validated: true, Predictor: stubPredictor{p: 0.5, ok: true}}); err != nil {
		t.Fatal(err)
	}

	a, err := Replay(rows, freg, preds)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Replay(rows, freg, preds)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two Replay calls over the same input produced different output")
	}
	if len(a) != 3 {
		t.Fatalf("len(a) = %d, want 3", len(a))
	}
}

// TestReplayLabelsCensoredRatherThanNeverReturned is requirement #5: the last request of a
// conversation must be marked Censored, never scored as "did not return".
func TestReplayLabelsCensoredRatherThanNeverReturned(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+30_000, "m", 100_000, "end_turn", "hit", 0, 0),
	})
	dps, err := Replay(rows, DefaultFeatures(), NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	if dps[0].Outcome.Censored {
		t.Error("the first request has a successor; it must not be Censored")
	}
	if !dps[1].Outcome.Censored {
		t.Error("the last request of the conversation has no successor; it must be Censored")
	}
	if dps[1].Outcome.Within5m || dps[1].Outcome.Within1h {
		t.Error("a Censored row's Within5m/Within1h are meaningless and must read false, " +
			"not be mistaken for an observed miss")
	}
}

// TestReplayAccumulatesGloballyNotPerConversation is the cross-conversation half of the
// no-leakage invariant: History must accumulate gaps in GLOBAL wall-clock order, so a
// decision in conversation A never sees a gap from conversation B that, in real time,
// closed after it.
func TestReplayAccumulatesGloballyNotPerConversation(t *testing.T) {
	// Conversation B's only gap closes at base+10_000, strictly BEFORE conversation A's
	// second request at base+20_000 — so A's second decision may legitimately see it once
	// it has actually closed, and A's FIRST decision (at base) must not.
	rows := derive([]*kvcache.Request{
		req(1, "a", "sA", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(2, "b", "sB", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(3, "b", "sB", base+10_000, "m", 100_000, "end_turn", "hit", 0, 0),
		req(4, "a", "sA", base+20_000, "m", 100_000, "end_turn", "hit", 0, 0),
	})
	dps, err := Replay(rows, DefaultFeatures(), NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]DecisionPoint{}
	for _, dp := range dps {
		byID[dp.RequestID] = dp
	}
	if n := byID[1].Features[FeatureReuseWithin5mN]; n.Present {
		t.Errorf("conversation A's FIRST decision already sees %v closed gaps; nothing had "+
			"closed anywhere yet", n)
	}
}

func TestAtDecisionPointMatchesReplay(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+30_000, "m", 100_000, "end_turn", "hit", 0, 0),
	})
	freg, preds := DefaultFeatures(), NewPredictors()
	all, err := Replay(rows, freg, preds)
	if err != nil {
		t.Fatal(err)
	}
	one, err := AtDecisionPoint(rows, 2, freg, preds)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(one, all[1]) {
		t.Fatalf("AtDecisionPoint(2) = %+v, want %+v", one, all[1])
	}
	if _, err := AtDecisionPoint(rows, 999, freg, preds); err == nil {
		t.Fatal("AtDecisionPoint for a request id not in the dataset must error")
	}
}

func TestReplayNilPredictorsIsFine(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100_000, "tool_use", "hit", 0, 0),
	})
	dps, err := Replay(rows, DefaultFeatures(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dps) != 1 || len(dps[0].Predictions) != 0 {
		t.Fatalf("Replay with nil Predictors = %+v, want one DecisionPoint with no Predictions", dps)
	}
}

func TestReplayEmptyInput(t *testing.T) {
	dps, err := Replay(nil, DefaultFeatures(), NewPredictors())
	if err != nil || dps != nil {
		t.Fatalf("Replay(nil, ...) = (%v, %v), want (nil, nil)", dps, err)
	}
}
