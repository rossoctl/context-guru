package harbor

// The Go keep-alive budget and the Python one must be the same arithmetic.
//
// kvcache.BudgetPolicy is what a replay is scored by; kv_ttl_keepalive_policy.py is where the
// model is fitted. Two implementations of one money question is exactly the drift this project
// has been bitten by, so the Python is a port, this is the guard, and when they disagree Go is
// right. (Neither side is on a dashboard page: BudgetPolicy is not in Registry() and, like
// Custom's Predictor, it is reached only by an in-process caller — "a predictor is code, not a
// query parameter", as dash/kvcachesim.go puts it. That is a reason for the guard rather than
// against it: an arm nothing renders is an arm nobody would notice drifting.)
//
// It compares the DECISION and both intermediate vectors. A budget that agrees for the wrong
// reason — a hazard scaled wrong and a survival scaled inversely wrong — is a guard that has
// already stopped working, so the per-window h and s are pinned too.
//
// Nothing is mocked. The Python side runs its real `--fixture` entry point, which is
// stdlib-only by construction, so this runs in a plain CI container.

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// keepAlivePythonBin is pythonBin parameterised by module, which is the only thing the two
// ever differed in — the skip message has to name the module that could not be imported, and
// this one needs no scientific stack at all where kv_ttl_cost_model may. Two spellings of one
// interpreter probe is how they come to disagree the day one of them gains a caveat.
func keepAlivePythonBin(t *testing.T) string {
	t.Helper()
	return pythonBinFor(t, "kv_ttl_keepalive_policy")
}

// The fixture wire format. Field names are the Python's own.
type budgetCase struct {
	CachedTokens int64        `json:"cached_tokens"`
	InputRate    float64      `json:"input_rate"`
	CDF          [][2]float64 `json:"cdf"`
	MinPrefix    int64        `json:"min_prefix,omitempty"`
}

type budgetFixture struct {
	IntervalS float64      `json:"interval_s"`
	LifeS     float64      `json:"life_s"`
	MaxK      int          `json:"max_k"`
	Cases     []budgetCase `json:"cases"`
}

type budgetWindows struct {
	H []float64 `json:"h"`
	S []float64 `json:"s"`
}

type budgetPyResult struct {
	BreakEven float64         `json:"break_even"`
	Budgets   []int           `json:"budgets"`
	Windows   []budgetWindows `json:"windows"`
}

// cdfPredictor is the same hand-written cumulative distribution the fixture carries, as a
// kvcache.Predictor, so both sides are answering from identical numbers rather than from two
// fits that would differ for uninteresting reasons.
type cdfPredictor struct{ points [][2]float64 }

func (c cdfPredictor) ReuseProbability(_ kvcache.Observation, horizon time.Duration) (float64, bool) {
	// Sorted, because _run_fixture sorts its points and a step function read in a different
	// order is a different distribution. The fixture below happens to be written in order; a
	// guard that depends on that is a guard that breaks on the next row somebody adds.
	pts := append([][2]float64(nil), c.points...)
	sort.Slice(pts, func(i, j int) bool { return pts[i][0] < pts[j][0] })
	sec := horizon.Seconds()
	p := 0.0
	for _, pt := range pts {
		if sec >= pt[0] {
			p = pt[1]
		}
	}
	return p, true
}

// budgetDriftFixture is the case table, and each row is chosen to exercise a branch the two
// implementations could plausibly diverge on rather than to look varied.
func budgetDriftFixture() budgetFixture {
	return budgetFixture{
		IntervalS: 280, LifeS: 300, MaxK: 8,
		Cases: []budgetCase{
			// Nothing to buy: back almost immediately.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0.99}, {86400, 1}}},
			// One rich window, far above the break-even.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.90}, {86400, 1}}},
			// Just BELOW the break-even, where an off-by-a-hair in either rescue or ping cost
			// changes the decision. This is the row that catches a rate mistake.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.0869}, {86400, 1}}},
			// Just ABOVE it.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.0871}, {86400, 1}}},
			// A lean first window redeemed by a rich second: pins the option value.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{580, 0.02}, {860, 0.60}, {3600, 1}}},
			// A worthless tail: pins where the schedule ends.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.30}, {86400, 0.30}}},
			// A rich window five sweeps out: pins holding through a dead stretch.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, .3}, {860, .3}, {1140, .3}, {1420, .3},
					{1700, .95}, {86400, 1}}},
			// Most already returned: pins the divide by the survivor share. Without it both
			// sides would read 5pp where the conditional truth is 50%.
			{CachedTokens: 124_845, InputRate: 3.0e-6,
				CDF: [][2]float64{{280, 0.90}, {580, 0.90}, {860, 0.95}, {3600, 1}}},
			// A small prefix, where a ping's FIXED overhead is a real share of its cost and the
			// two sides could disagree by dropping the ping_input/ping_output term.
			{CachedTokens: 900, InputRate: 3.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.20}, {86400, 1}}},
			// MinPrefix: a decision, not an absence of one.
			{CachedTokens: 5_000, InputRate: 3.0e-6, MinPrefix: 10_000,
				CDF: [][2]float64{{300, 0}, {580, 0.90}, {86400, 1}}},
			// A different rate scale entirely. The break-even must not move, because the prefix
			// and the per-token rate both cancel — this is the row that proves that claim.
			{CachedTokens: 124_845, InputRate: 15.0e-6,
				CDF: [][2]float64{{300, 0}, {580, 0.0871}, {86400, 1}}},
		},
	}
}

func scoreBudgetWithPort(t *testing.T, py string, f budgetFixture) budgetPyResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.json")
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "kv_ttl_keepalive_policy.py", "--fixture", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the port failed on the fixture: %v\nstdout: %s\nstderr: %s",
			err, out, stderr.String())
	}
	var got budgetPyResult
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("cannot read the port's output: %v\n%s", err, out)
	}
	return got
}

// goBudget is kvcache's own answer for one fixture case.
func goBudget(c budgetCase, f budgetFixture) (int, bool, []float64, []float64) {
	o := kvcache.Observation{
		User: "acct-1", Conversation: "conv-1", Model: "m", Now: 1_786_967_311_185,
		CachedTokens: c.CachedTokens, TTL: kvcache.TTL5m,
		Pricing: kvcache.Pricing{
			Model: "m", Input: c.InputRate, Output: c.InputRate * 5,
			CacheRead:       c.InputRate * kvcache.DefaultCacheReadMultiple,
			Write5m:         c.InputRate * kvcache.DefaultWrite5mMultiple,
			Write1h:         c.InputRate * kvcache.DefaultWrite1hMultiple,
			PingInputTokens: 1, PingOutputTokens: 1, Known: true,
		},
	}
	pol := kvcache.BudgetPolicy{
		Predictor: cdfPredictor{points: c.CDF},
		Interval:  time.Duration(f.IntervalS) * time.Second,
		MaxK:      f.MaxK,
		MinPrefix: c.MinPrefix,
	}
	k, ok := pol.PingBudget(o)
	h, s, _ := pol.Windows(o)
	return k, ok, h, s
}

// The two implementations of the keep-alive budget must agree on every case, and on the two
// vectors the budget is computed from.
func TestKeepAliveBudgetAgreesWithThePort(t *testing.T) {
	py := keepAlivePythonBin(t)
	if py == "" {
		t.Skip("no usable Python; a missing interpreter is an absent guard, not a failure")
	}
	f := budgetDriftFixture()
	got := scoreBudgetWithPort(t, py, f)
	if len(got.Budgets) != len(f.Cases) {
		t.Fatalf("port returned %d budgets for %d cases", len(got.Budgets), len(f.Cases))
	}

	// The break-even is the one number the whole arm rests on, and it must be the rates' own.
	const wantBreakEven = kvcache.DefaultCacheReadMultiple /
		(kvcache.DefaultWrite5mMultiple - kvcache.DefaultCacheReadMultiple)
	if math.Abs(got.BreakEven-wantBreakEven) > 1e-9 {
		t.Errorf("break-even: port %.10f, Go's rates give %.10f", got.BreakEven, wantBreakEven)
	}

	for i, c := range f.Cases {
		k, ok, h, s := goBudget(c, f)
		if !ok && c.MinPrefix == 0 {
			t.Errorf("case %d: Go had no opinion on a fully priced observation", i)
			continue
		}
		if k != got.Budgets[i] {
			t.Errorf("case %d (prefix %d, cdf %v): Go budget %d, port %d",
				i, c.CachedTokens, c.CDF, k, got.Budgets[i])
		}
		if c.MinPrefix > 0 {
			continue // short-circuited before the windows are computed on both sides
		}
		// Length first: indexing a shorter vector would PANIC the guard rather than fail it,
		// and a panic in a drift test reads as a broken test rather than as drift.
		if len(got.Windows) <= i || len(got.Windows[i].H) != len(h) ||
			len(got.Windows[i].S) != len(s) {
			t.Errorf("case %d: Go returned %d windows, port returned %v — the two are not even "+
				"describing the same schedule", i, len(h), got.Windows)
			continue
		}
		for j := range h {
			if math.Abs(h[j]-got.Windows[i].H[j]) > 1e-9 {
				t.Errorf("case %d window %d hazard: Go %.10f, port %.10f",
					i, j+1, h[j], got.Windows[i].H[j])
			}
			if math.Abs(s[j]-got.Windows[i].S[j]) > 1e-9 {
				t.Errorf("case %d window %d survival: Go %.10f, port %.10f",
					i, j+1, s[j], got.Windows[i].S[j])
			}
		}
	}
}

// MaxK <= 0 means the default on BOTH sides.
//
// The fixture above always sends 8, so nothing in it would notice the two disagreeing here —
// and they did: Go's maxK() falls back to DefaultBudgetMaxK where the port read a
// non-positive max_k as "no windows", answering 0 to every case. A default is exactly the
// kind of thing a port drifts on, because it is the value nobody passes.
func TestKeepAliveBudgetDefaultsMaxKTheSameWayOnBothSides(t *testing.T) {
	py := keepAlivePythonBin(t)
	if py == "" {
		t.Skip("no usable Python")
	}
	explicit := budgetDriftFixture()
	explicit.MaxK = kvcache.DefaultBudgetMaxK
	implicit := budgetDriftFixture()
	implicit.MaxK = 0

	want := scoreBudgetWithPort(t, py, explicit)
	got := scoreBudgetWithPort(t, py, implicit)
	for i := range explicit.Cases {
		if got.Budgets[i] != want.Budgets[i] {
			t.Errorf("case %d: port answered %d with max_k=0 and %d with max_k=%d",
				i, got.Budgets[i], want.Budgets[i], kvcache.DefaultBudgetMaxK)
		}
		// And Go, whose MaxK=0 is the same fallback.
		k, ok, _, _ := goBudget(explicit.Cases[i], implicit)
		if !ok && explicit.Cases[i].MinPrefix == 0 {
			t.Errorf("case %d: Go had no opinion with MaxK=0", i)
			continue
		}
		if k != got.Budgets[i] {
			t.Errorf("case %d with MaxK=0: Go %d, port %d", i, k, got.Budgets[i])
		}
	}
}

// The Python module's own claims have to hold, checked by the module itself, so that the
// fixture agreeing does not mask both sides being wrong in the same way.
func TestThePortsSelfTestPasses(t *testing.T) {
	py := keepAlivePythonBin(t)
	if py == "" {
		t.Skip("no usable Python")
	}
	out, err := exec.Command(py, "kv_ttl_keepalive_policy.py", "--self-test").CombinedOutput()
	if err != nil {
		t.Fatalf("the port's self-test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "self-test OK") {
		t.Errorf("unexpected self-test output: %s", out)
	}
}
