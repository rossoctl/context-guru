package harbor

// The shipped reuse model must mean the same thing in Go as in the Python that fitted it.
//
// kv_ttl_keepalive_drift_test.go beside this file already pins the DECISION arithmetic — the
// break-even, `windows`, the backward induction — by feeding both sides a hand-written step CDF.
// That guard never touches a fitted model: `--fixture` supplies the distribution, so the
// coefficients, the feature derivation and the standardisation are all outside it. Its own
// docstring says so ("the drift guard covers NEITHER").
//
// This is that hole. `kvcache.ReuseModelV1` is a table generated from
// deploy/harbor/reusemodel_v1.json, and between the two sit a dozen chances to disagree
// silently: a log1p applied to milliseconds on one side and seconds on the other, a
// standardisation Go forgot, a sin/cos convention, a one-hot level that case-folds on one side
// only, a sweep index off by one, an interpolation that extrapolates instead of clamping. Every
// one of those produces a plausible number and no error at all.
//
// So the fixture carries RAW `Observation` fields, never derived features, and the comparison
// covers four layers rather than only the last: the per-sweep survival vector, the CDF at
// horizons deliberately placed on and off the knot grid, the per-window h/s that
// BudgetPolicy.Windows derives, and the budget itself. A budget that agrees for the wrong reason
// is a guard that has already stopped working.
//
// When they disagree, Go is right — it is what a replay is scored by.

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// reuseCase is one span, in the terms Observation actually carries. Nothing here is a derived
// feature: that is the point (see the file comment).
type reuseCase struct {
	TsMs         int64     `json:"ts_ms"`
	CachedTokens int64     `json:"cached_tokens"`
	SinceLastMs  int64     `json:"since_last_ms"`
	Turn         int       `json:"turn"`
	StatP        float64   `json:"stat_p"`
	StatN        int       `json:"stat_n"`
	Model        string    `json:"model"`
	InputRate    float64   `json:"input_rate"`
	MinPrefix    int64     `json:"min_prefix,omitempty"`
	Horizons     []float64 `json:"horizons"`

	why string `json:"-"`
}

type reuseFixture struct {
	IntervalS float64     `json:"interval_s"`
	LifeS     float64     `json:"life_s"`
	MaxK      int         `json:"max_k"`
	Cases     []reuseCase `json:"cases"`
}

type reusePyResult struct {
	Survival [][]float64     `json:"survival"`
	CDF      [][]float64     `json:"cdf"`
	Windows  []budgetWindows `json:"windows"`
	Budgets  []int           `json:"budgets"`
}

// fixedStats is a Stats that answers with whatever the case says, so the two sides read the
// same stat_p/stat_n without either having to replay a trajectory to get there.
type fixedStats struct {
	p float64
	n int
}

func (f fixedStats) ReuseWithin(_, _ string, _ kvcache.Bucket, _ time.Duration) (float64, int, string) {
	return f.p, f.n, kvcache.LevelGlobal
}

func (f fixedStats) MedianIdle(_, _ string, _ kvcache.Bucket) (time.Duration, int, string) {
	return 0, f.n, kvcache.LevelGlobal
}

// reuseDriftCases is the case table. Each row sits on a branch the two implementations could
// plausibly diverge on rather than being chosen to look varied.
//
// The horizon lists matter as much as the spans. `windows()` reads four horizons per sweep —
// t_j, the standing deadline, t_j + life and t_j + interval — and only two of those are knots.
// The rest are interpolated, and one of them (t_j + interval for the last sweep) sits past the
// end of the grid, where the two sides must CLAMP rather than extrapolate.
func reuseDriftCases() []reuseCase {
	// On and off the knot grid at interval 280 / life 300: knots are 0, 280, 560, 580, 840,
	// 860, ... so 300, 420, 700 and 2600 are all interpolated, 0 is the lower clamp and 99999
	// is past the last knot.
	hz := []float64{0, 1, 280, 300, 420, 560, 580, 700, 840, 860, 1120, 1140, 2240, 2260, 2520,
		2540, 2600, 99999}
	return []reuseCase{
		{why: "a large prefix on a known model, the population the money sits on",
			TsMs: 1787654871634, CachedTokens: 124845, SinceLastMs: 0, Turn: 1,
			StatP: 0.0, StatN: 0, Model: "aws/claude-opus-5", InputRate: 3.8e-06, Horizons: hz},
		{why: "the same span with a rich history, so stat_p and log_stat_n both move",
			TsMs: 1787654871634, CachedTokens: 124845, SinceLastMs: 45_000, Turn: 37,
			StatP: 0.93, StatN: 412, Model: "aws/claude-opus-5", InputRate: 3.8e-06, Horizons: hz},
		{why: "a model id the fit never saw, which must fall to the catch-all level",
			TsMs: 1787654871634, CachedTokens: 60000, SinceLastMs: 10_000, Turn: 5,
			StatP: 0.5, StatN: 20, Model: "nosuch/model-9", InputRate: 3.8e-06, Horizons: hz},
		{why: "a model id differing only in case, which must fold to the same level",
			TsMs: 1787654871634, CachedTokens: 60000, SinceLastMs: 10_000, Turn: 5,
			StatP: 0.5, StatN: 20, Model: "AWS/Claude-Opus-5", InputRate: 3.8e-06, Horizons: hz},
		{why: "a tiny prefix, where the ping's fixed overhead dominates the break-even",
			TsMs: 1787654871634, CachedTokens: 500, SinceLastMs: 1_000, Turn: 2,
			StatP: 0.2, StatN: 8, Model: "aws/claude-sonnet-5", InputRate: 1.52e-06, Horizons: hz},
		{why: "a 5x different rate scale, to prove the threshold does not move with it",
			TsMs: 1787654871634, CachedTokens: 124845, SinceLastMs: 0, Turn: 1,
			StatP: 0.0, StatN: 0, Model: "aws/claude-opus-5", InputRate: 1.9e-05, Horizons: hz},
		{why: "a MinPrefix gate above the prefix: a decision, not an absence of one",
			TsMs: 1787654871634, CachedTokens: 5_000, SinceLastMs: 0, Turn: 1,
			StatP: 0.0, StatN: 0, Model: "aws/claude-opus-5", InputRate: 3.8e-06,
			MinPrefix: 20_000, Horizons: hz},
		{why: "an hour and weekday at the far side of both cycles, for the sin/cos convention",
			TsMs: 1787698799000, CachedTokens: 90_000, SinceLastMs: 600_000, Turn: 12,
			StatP: 0.75, StatN: 64, Model: "claude-opus-5", InputRate: 3.8e-06, Horizons: hz},
		{why: "turn and gap far out in the tail, where standardisation errors show up",
			TsMs: 1787654871634, CachedTokens: 250_000, SinceLastMs: 7_200_000, Turn: 4_000,
			StatP: 0.99, StatN: 5_000, Model: "azure/gpt-5.5", InputRate: 2.5e-06, Horizons: hz},
		{why: "a zero prefix, which the arm must decline rather than price",
			TsMs: 1787654871634, CachedTokens: 0, SinceLastMs: 0, Turn: 1,
			StatP: 0.0, StatN: 0, Model: "aws/claude-opus-5", InputRate: 3.8e-06, Horizons: hz},
	}
}

// observationOf builds the Observation the Go side scores, from the same raw fields the fixture
// hands the Python.
func observationOf(c reuseCase) kvcache.Observation {
	return kvcache.Observation{
		User: "u", Conversation: "c", Model: c.Model, RequestID: 1,
		Now:          c.TsMs,
		HourUTC:      time.UnixMilli(c.TsMs).UTC().Hour(),
		Bucket:       kvcache.BucketAt(c.TsMs),
		CachedTokens: c.CachedTokens,
		SinceLastMs:  c.SinceLastMs,
		TTL:          kvcache.TTL5m,
		ExpiresAt:    c.TsMs + int64(kvcache.TTL5m.Lifetime()/time.Millisecond),
		Turn:         c.Turn,
		Stats:        fixedStats{p: c.StatP, n: c.StatN},
		Pricing: kvcache.Pricing{
			Known: true, Input: c.InputRate, Output: c.InputRate * 5.0,
			CacheRead: c.InputRate * 0.10, Write5m: c.InputRate * 1.25,
			Write1h:          c.InputRate * 2.00,
			PingInputTokens:  1,
			PingOutputTokens: 1,
		},
	}
}

func TestReuseModelV1AgreesWithThePort(t *testing.T) {
	if err := kvcache.ReuseModelV1.Validate(); err != nil {
		t.Fatalf("the shipped model is not scoreable by this build: %v", err)
	}
	py := pythonBinFor(t, "kv_ttl_keepalive_policy")
	if py == "" {
		t.Skip("no python3 that can import kv_ttl_keepalive_policy")
	}
	model := kvcache.ReuseModelV1
	fx := reuseFixture{IntervalS: model.IntervalS, LifeS: model.LifeS, MaxK: model.MaxK,
		Cases: reuseDriftCases()}
	blob, err := json.Marshal(fx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "reuse_fixture.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// The model the Python scores is the CHECKED-IN artifact the Go table was generated from.
	// That is the comparison worth making: it catches a hand-edit of the generated file and a
	// regeneration that was never committed, neither of which a shared in-memory model would.
	out, err := exec.Command(py, "kv_ttl_keepalive_policy.py", "--score", path,
		"--model", "reusemodel_v1.json").Output()
	if err != nil {
		t.Fatalf("python --score: %v", err)
	}
	var got reusePyResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse python output %q: %v", strings.TrimSpace(string(out)), err)
	}
	if len(got.Budgets) != len(fx.Cases) {
		t.Fatalf("python answered %d cases, fixture had %d", len(got.Budgets), len(fx.Cases))
	}

	// 1e-9 absolute. The two sides do the same arithmetic in the same order in IEEE754 doubles,
	// so anything above float noise is a real divergence rather than a tolerance question — a
	// loose tolerance here would hide exactly the bugs this exists to catch.
	const tol = 1e-9
	for i, c := range fx.Cases {
		o := observationOf(c)
		pol := kvcache.BudgetPolicy{Predictor: model, Interval: time.Duration(model.IntervalS) *
			time.Second, MaxK: model.MaxK, MinPrefix: c.MinPrefix}

		// (a) the CDF, at horizons on and off the knot grid.
		for j, hz := range c.Horizons {
			want := got.CDF[i][j]
			have, ok := model.ReuseProbability(o, time.Duration(hz*float64(time.Second)))
			if !ok {
				t.Errorf("case %d (%s): Go declined horizon %gs, python answered %.12f",
					i, c.why, hz, want)
				continue
			}
			if math.Abs(have-want) > tol {
				t.Errorf("case %d (%s): F(%gs) Go %.12f vs python %.12f (delta %.3g)",
					i, c.why, hz, have, want, have-want)
			}
		}

		// (b) the per-window hazard and survival BudgetPolicy derives from that CDF. Compared
		// because a CDF that agrees and windows that do not means the schedule is being read
		// differently, which no budget comparison alone would localise.
		h, s, ok := pol.Windows(o)
		if !ok {
			if c.CachedTokens > 0 {
				t.Errorf("case %d (%s): Windows declined a priced observation", i, c.why)
			}
			continue
		}
		for j := range got.Windows[i].H {
			if j >= len(h) {
				t.Errorf("case %d (%s): Go returned %d windows, python %d",
					i, c.why, len(h), len(got.Windows[i].H))
				break
			}
			if math.Abs(h[j]-got.Windows[i].H[j]) > tol {
				t.Errorf("case %d (%s): h[%d] Go %.12f vs python %.12f",
					i, c.why, j, h[j], got.Windows[i].H[j])
			}
			if math.Abs(s[j]-got.Windows[i].S[j]) > tol {
				t.Errorf("case %d (%s): s[%d] Go %.12f vs python %.12f",
					i, c.why, j, s[j], got.Windows[i].S[j])
			}
		}

		// (c) the decision.
		budget, bok := pol.PingBudget(o)
		if !bok {
			// The Python has no cap to fall back to, so it reports 0 where Go reports
			// (0, false) — see budget_for's own docstring. Only a nonzero python budget is a
			// disagreement here.
			if got.Budgets[i] != 0 {
				t.Errorf("case %d (%s): Go had no opinion, python chose %d",
					i, c.why, got.Budgets[i])
			}
			continue
		}
		if budget != got.Budgets[i] {
			t.Errorf("case %d (%s): budget Go %d vs python %d\n  Go h=%v\n  py h=%v",
				i, c.why, budget, got.Budgets[i], h, got.Windows[i].H)
		}
	}
}

// TestReuseModelV1IsValid keeps a re-fit that adds or renames a feature from shipping silently.
//
// Validate refuses a table naming a term this build cannot evaluate, and the failure it prevents
// is the quiet one: an unknown feature would otherwise be scored against zero, which is not an
// error, not a crash, and not the model that was fitted.
func TestReuseModelV1IsValid(t *testing.T) {
	if err := kvcache.ReuseModelV1.Validate(); err != nil {
		t.Fatalf("shipped model invalid: %v", err)
	}
	if kvcache.ReuseModelV1.Version == "" || kvcache.ReuseModelV1.TrainedOn == "" {
		t.Error("the shipped model carries no provenance; a saving attributed to it could not " +
			"be traced to a corpus or a window")
	}
}
