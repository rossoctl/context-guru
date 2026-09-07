package kvcache

// Each test here asserts one PROPERTY the arm depends on, not one code path.
//
// The Python-parity guard lives in deploy/harbor/kv_ttl_reusemodel_drift_test.go and pins the
// numbers. These pin the shape: that the CDF is a distribution, that a horizon outside the knot
// grid clamps rather than extrapolates, that an unseen level falls to the catch-all rather than to
// the reference level, that the standardisation is actually applied, and that a model this build
// cannot score is refused instead of quietly scored against zeros.

import (
	"math"
	"testing"
	"time"
)

// tinyModel is a hand-written model with values chosen so the expected answers can be computed by
// hand rather than read off a previous run of the code under test.
func tinyModel() *ReuseModel {
	return &ReuseModel{
		Version: "test", TrainedOn: "hand-written",
		IntervalS: 280, LifeS: 300, MaxK: 3,
		Intercept: 0,
		Numeric: []ReuseTerm{
			// (log_prefix - 10) / 2, so a prefix whose log1p is 10 contributes exactly 0 and one
			// at 12 contributes exactly the coefficient.
			{Name: featLogPrefix, Coef: 1, Center: 10, Scale: 2},
		},
		Categorical: []ReuseCategory{
			{Name: featCategoricalSweep, Levels: []ReuseLevel{
				{Level: "1", Coef: 2}, {Level: "2", Coef: 2}, {Level: "3", Coef: 2},
				{Level: "4", Coef: 2}, {Level: "", Coef: 2},
			}},
			{Name: featCategoricalModel, Levels: []ReuseLevel{
				{Level: "known", Coef: 1}, {Level: "", Coef: -3},
			}},
		},
	}
}

func obs(prefix int64, model string) Observation {
	return Observation{
		User: "u", Model: model, Now: 1_700_000_000_000, CachedTokens: prefix,
		Turn: 1, TTL: TTL5m, Bucket: BucketAt(1_700_000_000_000),
	}
}

func TestReuseModelCDFIsADistribution(t *testing.T) {
	m := tinyModel()
	o := obs(60_000, "known")
	prev := -1.0
	// Every horizon from before the first knot to well past the last one.
	for sec := 0; sec <= 3000; sec += 10 {
		p, ok := m.ReuseProbability(o, time.Duration(sec)*time.Second)
		if !ok {
			t.Fatalf("declined at %ds", sec)
		}
		if p < 0 || p > 1 {
			t.Fatalf("F(%ds) = %g is not a probability", sec, p)
		}
		if p < prev-1e-12 {
			t.Fatalf("F(%ds) = %g fell below F(%ds) = %g; a cumulative distribution cannot "+
				"decrease, and BudgetPolicy divides by 1-F so a decrease is a negative "+
				"survivor share and a budget with no meaning", sec, p, sec-10, prev)
		}
		prev = p
	}
}

func TestReuseModelIsAlreadyConditionedOnReachingTheFirstSweep(t *testing.T) {
	// F(t_1) = 0 by construction: the CDF starts at the first sweep, because that is the
	// population the decision is faced by. Windows divides by 1-F(t_1), so a nonzero value here
	// would condition on the same thing twice.
	m := tinyModel()
	p, ok := m.ReuseProbability(obs(60_000, "known"), time.Duration(m.IntervalS)*time.Second)
	if !ok {
		t.Fatal("declined at the first sweep")
	}
	if p != 0 {
		t.Errorf("F(t_1) = %g, want exactly 0", p)
	}
}

func TestReuseModelClampsPastTheGridRatherThanExtrapolating(t *testing.T) {
	m := tinyModel()
	o := obs(60_000, "known")
	last := float64(m.MaxK+1)*m.IntervalS + m.LifeS
	end, _ := m.ReuseProbability(o, time.Duration(last)*time.Second)
	for _, sec := range []float64{last, last + 1, last * 10, 86_400} {
		p, ok := m.ReuseProbability(o, time.Duration(sec)*time.Second)
		if !ok {
			t.Fatalf("declined at %gs", sec)
		}
		if math.Abs(p-end) > 1e-12 {
			t.Errorf("F(%gs) = %g but the last knot is %g; extrapolating past the grid is how a "+
				"probability climbs past 1", sec, p, end)
		}
	}
}

func TestReuseModelAnUnseenLevelFallsToTheCatchAllNotToZero(t *testing.T) {
	// The distinction is the whole reason ReuseLevel has an empty-string entry. Zero is not
	// "unknown", it is "exactly the reference level" — a much stronger claim about a model id the
	// fit never saw. tinyModel gives the catch-all -3 against the known level's +1, so the two
	// are far apart and a fallback to 0 would sit between them and be caught either way.
	m := tinyModel()
	known, _ := m.ReuseProbability(obs(60_000, "known"), 560*time.Second)
	unseen, _ := m.ReuseProbability(obs(60_000, "never-trained-on"), 560*time.Second)
	zeroish := &ReuseModel{Version: "z", TrainedOn: "x", IntervalS: 280, LifeS: 300, MaxK: 3,
		Numeric: m.Numeric, Categorical: []ReuseCategory{m.Categorical[0],
			{Name: featCategoricalModel, Levels: []ReuseLevel{{Level: "known", Coef: 1}}}}}
	fellToZero, _ := zeroish.ReuseProbability(obs(60_000, "never-trained-on"), 560*time.Second)
	if math.Abs(unseen-known) < 1e-9 {
		t.Error("an unseen model id scored identically to a trained one; the level is not being read")
	}
	if math.Abs(unseen-fellToZero) < 1e-9 {
		t.Error("the catch-all coefficient had no effect: an unseen level is being scored as 0, " +
			"which claims it behaves exactly like the reference level")
	}
}

func TestReuseModelCaseFoldsTheModelLevel(t *testing.T) {
	// The corpus carries `Azure/gpt-4o` beside `azure/gpt-5.5`, because the id is whatever the
	// client sent. Two levels for one model halve its evidence, so both sides fold.
	m := tinyModel()
	lower, _ := m.ReuseProbability(obs(60_000, "known"), 560*time.Second)
	upper, _ := m.ReuseProbability(obs(60_000, "KNOWN"), 560*time.Second)
	if math.Abs(lower-upper) > 1e-12 {
		t.Errorf("%q scored %g and %q scored %g; the level is not case-folded",
			"known", lower, "KNOWN", upper)
	}
}

func TestReuseModelAppliesTheStandardisation(t *testing.T) {
	// A coefficient means nothing without the Center/Scale the fit used. Same coefficient, same
	// features, different standardisation must give a different answer — otherwise Go is ignoring
	// two thirds of every ReuseTerm and the Python's coefficients are being applied to raw
	// values.
	a := tinyModel()
	b := tinyModel()
	b.Numeric[0].Scale = 20 // ten times wider, so the same feature contributes a tenth as much
	pa, _ := a.ReuseProbability(obs(1_000_000, "known"), 560*time.Second)
	pb, _ := b.ReuseProbability(obs(1_000_000, "known"), 560*time.Second)
	if math.Abs(pa-pb) < 1e-6 {
		t.Errorf("Scale had no effect (%g vs %g); the standardisation is not being applied", pa, pb)
	}
}

func TestReuseModelHasNoOpinionWithoutATable(t *testing.T) {
	// ok=false is reserved for "there is no model". Everything else a decision needs refused —
	// unknown rates, no prefix, an unpriceable schedule — is BudgetPolicy's own business and is
	// checked there; duplicating it here would be the same policy in two places.
	var nilModel *ReuseModel
	if _, ok := nilModel.ReuseProbability(obs(60_000, "known"), time.Minute); ok {
		t.Error("a nil model claimed an opinion")
	}
	empty := &ReuseModel{Version: "e", IntervalS: 280, LifeS: 300, MaxK: 3}
	if _, ok := empty.ReuseProbability(obs(60_000, "known"), time.Minute); ok {
		t.Error("a model with no terms claimed an opinion")
	}
}

func TestReuseModelValidateRefusesWhatThisBuildCannotScore(t *testing.T) {
	// The failure this prevents is silent: an unknown feature would be scored against zero,
	// which is not an error and not a crash — it is a different model producing different
	// budgets with nothing to notice.
	for _, tc := range []struct {
		name string
		m    *ReuseModel
	}{
		{"nil", nil},
		{"no terms", &ReuseModel{Version: "x", IntervalS: 280, LifeS: 300}},
		{"unknown numeric", &ReuseModel{Version: "x", IntervalS: 280, LifeS: 300, MaxK: 3,
			Numeric: []ReuseTerm{{Name: "sweep_k", Coef: 1, Scale: 1}}}},
		{"unknown categorical", &ReuseModel{Version: "x", IntervalS: 280, LifeS: 300, MaxK: 3,
			Categorical: []ReuseCategory{{Name: "cache_ttl",
				Levels: []ReuseLevel{{Level: "ephemeral_5m", Coef: 1}}}}}},
		{"no schedule", &ReuseModel{Version: "x", MaxK: 3,
			Numeric: []ReuseTerm{{Name: featLogPrefix, Coef: 1, Scale: 1}}}},
	} {
		if err := tc.m.Validate(); err == nil {
			t.Errorf("%s: Validate accepted a model this build cannot score exactly", tc.name)
		}
	}
	if err := tinyModel().Validate(); err != nil {
		t.Errorf("a well-formed model was refused: %v", err)
	}
}

func TestReuseModelTheLastSweepsSurvivalCannotChangeABudget(t *testing.T) {
	// s_(MaxK+1) is composed into the grid but never read by Windows, which looks no further than
	// t_MaxK + life. This is asserted rather than assumed because the Python's `sweep` one-hot has
	// no fitted level for MaxK+1 — it falls to the catch-all — so if that value DID reach a
	// decision, the arm's answer would depend on an unfitted coefficient.
	base := tinyModel()
	moved := tinyModel()
	for i, l := range moved.Categorical[0].Levels {
		if l.Level == "" {
			moved.Categorical[0].Levels[i].Coef = -12 // a wildly different unfitted catch-all
		}
	}
	o := obs(60_000, "known")
	o.Pricing = Pricing{Known: true, Input: 3.8e-06, Output: 1.9e-05, CacheRead: 3.8e-07,
		Write5m: 4.75e-06, Write1h: 7.6e-06, PingInputTokens: 1, PingOutputTokens: 1}
	iv := time.Duration(base.IntervalS) * time.Second
	pa := BudgetPolicy{Predictor: base, Interval: iv, MaxK: base.MaxK}
	pb := BudgetPolicy{Predictor: moved, Interval: iv, MaxK: moved.MaxK}
	ka, oka := pa.PingBudget(o)
	kb, okb := pb.PingBudget(o)
	if oka != okb || ka != kb {
		t.Errorf("moving the unfitted sweep-%d coefficient changed the budget from (%d,%v) to "+
			"(%d,%v); Windows is reading past t_MaxK + life", base.MaxK+1, ka, oka, kb, okb)
	}
}

func TestReuseModelV1IsTheArmTheRegistryBuilds(t *testing.T) {
	// NewKeepAliveBudget is what makes `keepalive-budget` resolvable from a name, which is the
	// whole reason the arm could join the registry. If it stopped reading the compiled-in model
	// the name would still resolve — to an arm with a nil Predictor, which never pings at all
	// (Decide returns write_5m), so the page would show a silently different policy under the
	// right label.
	s, err := NewStrategy(StrategyKeepAliveBudget, nil, Config{})
	if err != nil {
		t.Fatalf("the registry cannot build its own arm: %v", err)
	}
	b, isBudget := s.(BudgetPolicy)
	if !isBudget {
		t.Fatalf("%s resolved to %T, not a BudgetPolicy", StrategyKeepAliveBudget, s)
	}
	if b.Predictor == nil {
		t.Fatal("the arm has no Predictor: it resolves, but it can never ping")
	}
	if b.Predictor != Predictor(ReuseModelV1) {
		t.Errorf("the arm reads %T rather than the compiled-in ReuseModelV1", b.Predictor)
	}
	if _, isBudgeter := s.(PingBudgeter); !isBudgeter {
		t.Error("the arm does not implement PingBudgeter, so Simulate will run it on the flat " +
			"Config.MaxPings path instead of its own per-conversation budget")
	}
}
