package predictor

import (
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

func TestRegisterRefusesImpossibleFeatureWithExtractor(t *testing.T) {
	f := NewFeatures()
	err := f.Register(Feature{
		ID: "bad", Availability: Impossible,
		Extract: func(kvcache.Observation, []*kvcache.Request, StatsContext) Value { return Value{} },
	})
	if err == nil {
		t.Fatal("an Impossible feature with an Extractor must be refused — it is a policy " +
			"input pretending to be a documented ceiling")
	}
}

func TestRegisterRefusesFeatureWithoutExtractorUnlessImpossible(t *testing.T) {
	f := NewFeatures()
	if err := f.Register(Feature{ID: "bad", Availability: Live}); err == nil {
		t.Fatal("a Live feature with no Extractor must be refused")
	}
}

func TestRegisterRefusesDuplicateID(t *testing.T) {
	f := NewFeatures()
	feat := Feature{
		ID: "dup", Availability: Live,
		Extract: func(kvcache.Observation, []*kvcache.Request, StatsContext) Value {
			return Value{Present: true}
		},
	}
	if err := f.Register(feat); err != nil {
		t.Fatalf("first registration should succeed: %v", err)
	}
	if err := f.Register(feat); err == nil {
		t.Fatal("registering the same ID twice must be refused")
	}
}

func TestRegisterRefusesBlankID(t *testing.T) {
	f := NewFeatures()
	if err := f.Register(Feature{Availability: Live,
		Extract: func(kvcache.Observation, []*kvcache.Request, StatsContext) Value { return Value{} },
	}); err == nil {
		t.Fatal("a blank ID must be refused")
	}
}

// TestExtractPastSliceIsCappedAtTheDecisionPoint proves the structural half of the leakage
// guarantee directly: past is handed over with len == cap == i, so NOTHING an Extractor
// does — including reslicing to widen it — can reach group[i] or later.
func TestExtractPastSliceIsCappedAtTheDecisionPoint(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+1000, "m", 200, "tool_use", "hit", 0, 0),
		req(3, "u", "s", base+2000, "m", 300, "end_turn", "hit", 0, 0),
	})
	// rows is chronological already (three requests, same conversation); group == rows.
	var gotLen, gotCap int
	f := NewFeatures()
	if err := f.Register(Feature{
		ID: "spy", Availability: Live,
		Extract: func(_ kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			gotLen, gotCap = len(past), cap(past)
			return Value{Present: true}
		},
	}); err != nil {
		t.Fatal(err)
	}
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	f.Extract(rows, 2, stats)
	if gotLen != 2 {
		t.Fatalf("past len = %d, want 2 (rows[0], rows[1])", gotLen)
	}
	if gotCap != gotLen {
		t.Fatalf("past cap = %d, len = %d — capacity beyond len means past[:cap(past)] "+
			"could reach the decision row or beyond", gotCap, gotLen)
	}
}

func TestImpossibleFeaturesHaveNoExtractorAndAlwaysAbsent(t *testing.T) {
	f := DefaultFeatures()
	for _, id := range []string{FeatureNextTS, FeatureIdleMs} {
		feat, ok := f.Get(id)
		if !ok {
			t.Fatalf("feature %q not registered", id)
		}
		if feat.Availability != Impossible {
			t.Errorf("feature %q: Availability = %q, want %q", id, feat.Availability, Impossible)
		}
		if feat.Extract != nil {
			t.Errorf("feature %q carries an Extractor despite being Impossible", id)
		}
	}
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100, "tool_use", "hit", 0, 0),
	})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := f.Extract(rows, 0, stats)
	for _, id := range []string{FeatureNextTS, FeatureIdleMs} {
		if feats[id].Present {
			t.Errorf("feature %q reported Present=true; an Impossible feature must never resolve", id)
		}
	}
}

func TestDefaultFeaturesBuildsWithoutError(t *testing.T) {
	f := DefaultFeatures()
	list := f.List()
	if len(list) == 0 {
		t.Fatal("DefaultFeatures registered nothing")
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("List() is not sorted by ID: %q then %q", list[i-1].ID, list[i].ID)
		}
	}
}
