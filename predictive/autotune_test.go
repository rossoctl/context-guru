package predictive

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"testing"
)

// fleetRows: n users x m rows on which K3 is worth 1 and every other plan 0, plus noise; one row a minute.
func fleetRows(rng *rand.Rand, users, m int, t0 int64) []AutoRow {
	var out []AutoRow
	for u := 0; u < users; u++ {
		for i := 0; i < m; i++ {
			g := make([]float64, 7)
			for k := range g {
				g[k] = rng.NormFloat64()
			}
			g[0], g[3] = 0, g[3]+1
			out = append(out, AutoRow{Scope: fmt.Sprintf("u%d", u), Session: fmt.Sprintf("u%d-s%d", u, i/20), EndMs: t0 + int64(i)*60e3, Gain: g})
		}
	}
	return out
}

func TestAutoTuneShrinksThinUsersAndFollowsStrongOnes(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	t0 := int64(1_786_000_000_000)
	rows := fleetRows(rng, 20, 2000, t0)
	// a thin user whose few rows favour K1 by chance-sized margins, and a heavy user for whom K1 is consistently better all the time
	for i := 0; i < 8; i++ {
		rows = append(rows, AutoRow{Scope: "thin", Session: "thin", EndMs: t0 + int64(i)*60e3, Gain: []float64{0, 2, 0, 1, 0, 0, 0}})
	}
	for i := 0; i < 2000; i++ {
		g := []float64{0, 3 + rng.NormFloat64(), 0, 1 + rng.NormFloat64(), 0, 0, 0}
		rows = append(rows, AutoRow{Scope: "heavy", Session: fmt.Sprintf("h%d", i/20), EndMs: t0 + int64(i)*60e3, Gain: g})
	}
	now := t0 + 2000*60e3 + 3600e3
	f := AutoTune(rows, now, AutoGrid{})
	if f.FleetPlan != 3 {
		t.Fatalf("fleet plan = K%d, want K3", f.FleetPlan)
	}
	if got := f.PlanFor("thin"); got != 3 {
		t.Errorf("8 rows moved a user off the fleet plan: K%d (kappa %v)", got, f.Kappa)
	}
	if got := f.PlanFor("heavy"); got != 1 {
		t.Errorf("2000 consistent rows did not move the heavy user: K%d (kappa %v)", got, f.Kappa)
	}
	if got := f.PlanFor("never-seen"); got != 3 {
		t.Errorf("an unseen user got K%d, want the fleet plan", got)
	}
}

func TestAutoTuneKeepsTheFleetPlanWhenUsersDoNotDiffer(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	t0 := int64(1_786_000_000_000)
	f := AutoTune(fleetRows(rng, 20, 1000, t0), t0+1000*60e3+3600e3, AutoGrid{})
	if !math.IsInf(f.Kappa, 1) {
		t.Errorf("homogeneous users chose kappa %v; the safest setting must win unless another is strictly better", f.Kappa)
	}
	for sc, k := range f.Plan {
		if k != f.FleetPlan {
			t.Errorf("%s got K%d, fleet K%d", sc, k, f.FleetPlan)
		}
	}
}

// The reference Python tuner (scripts/campaign/synth_fixtures.py) and this one make the same choices on the same rows: 20k synthetic rows
// of 19 scopes, exported with the Python answer.
func TestAutoTuneMatchesTheResearchImplementation(t *testing.T) {
	fh, err := os.Open("testdata/autotune_fixture.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		NowMs     int64          `json:"now_ms"`
		Scope     []string       `json:"scope"`
		EndMs     []int64        `json:"end_ms"`
		Gain      [][]float64    `json:"gain"`
		HalfLife  *float64       `json:"want_half_life_days"`
		Kappa     *float64       `json:"want_kappa"`
		Plan      map[string]int `json:"want_plan"`
		FleetPlan int            `json:"want_fleet"`
	}
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	rows := make([]AutoRow, len(fx.Scope))
	for i := range rows {
		rows[i] = AutoRow{Scope: fx.Scope[i], EndMs: fx.EndMs[i], Gain: fx.Gain[i]}
	}
	f := AutoTune(rows, fx.NowMs, AutoGrid{})
	inf := func(p *float64) float64 {
		if p == nil {
			return math.Inf(1)
		}
		return *p
	}
	if f.HalfLifeDays != inf(fx.HalfLife) || f.Kappa != inf(fx.Kappa) || f.FleetPlan != fx.FleetPlan {
		t.Fatalf("tuned (half-life %v, kappa %v, fleet K%d), research (%v, %v, K%d)", f.HalfLifeDays, f.Kappa, f.FleetPlan, inf(fx.HalfLife), inf(fx.Kappa), fx.FleetPlan)
	}
	for sc, k := range fx.Plan {
		if f.PlanFor(sc) != k {
			t.Errorf("%s: K%d, research K%d", sc, f.PlanFor(sc), k)
		}
	}
}
