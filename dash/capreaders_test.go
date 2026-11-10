package dash

import (
	"fmt"
	"math"
	"testing"
)

// The readers that sum stored rows must land on the headline's figure once the cap's correction is
// added back (#586). The fixture's second and third turn are past the cap, so the correction is real.

func TestSavingsTotalsCappedAgreesWithTheHeadline(t *testing.T) {
	api, until := replayCapAPI(t)
	var head struct {
		Baseline float64 `json:"baseline_cost_usd"`
		Net      float64 `json:"net_saved_usd"`
		Total    float64 `json:"total_saved_usd"`
		Corr     float64 `json:"replay_cap_correction_usd"`
	}
	api.get(t, fmt.Sprintf("/api/stats?since=0&until=%d", until), &head)
	if head.Corr == 0 {
		t.Fatal("fixture has no credit past the cap — the test proves nothing")
	}
	db := api.a.rec.DB()
	f := Filter{TenantAll: true, Session: "s"}
	plain, err := db.SavingsTotals(f)
	if err != nil {
		t.Fatal(err)
	}
	capped, err := db.SavingsTotalsCapped(f, api.a.windows, api.a.pricer)
	if err != nil {
		t.Fatal(err)
	}
	if capped.CapNotApplied {
		t.Fatal("cap reported not applied with a pricer and a resolver")
	}
	nearEq(t, "cap_correction_usd", capped.CapCorrectionUSD, head.Corr)
	nearEq(t, "baseline_cost_usd", capped.BaselineCostUSD, head.Baseline)
	nearEq(t, "net_saved_usd", capped.NetSavedUSD, head.Net)
	nearEq(t, "total_saved_usd", capped.TotalSavedUSD, head.Total)
	if !(capped.TotalSavedUSD < plain.TotalSavedUSD) {
		t.Errorf("capped total %v is not below the uncapped %v", capped.TotalSavedUSD, plain.TotalSavedUSD)
	}
	if plain.CapNotApplied || plain.CapCorrectionUSD != 0 {
		t.Error("plain SavingsTotals must stay uncapped and unlabelled")
	}
}

func TestSavingsTotalsCappedSaysWhenItCannotCap(t *testing.T) {
	api, _ := replayCapAPI(t)
	db := api.a.rec.DB()
	f := Filter{TenantAll: true, Session: "s"}
	plain, _ := db.SavingsTotals(f)
	for name, c := range map[string]struct{ noPricer, noWindows bool }{
		"no pricer": {noPricer: true}, "no windows": {noWindows: true}} {
		w, p := api.a.windows, api.a.pricer
		if c.noPricer {
			p = nil
		}
		if c.noWindows {
			w = nil
		}
		got, err := db.SavingsTotalsCapped(f, w, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.CapNotApplied || got.CapCorrectionUSD != 0 || math.Abs(got.TotalSavedUSD-plain.TotalSavedUSD) > 1e-12 {
			t.Errorf("%s: want the stored figure, labelled; got not_applied=%v corr=%v total=%v",
				name, got.CapNotApplied, got.CapCorrectionUSD, got.TotalSavedUSD)
		}
	}
}

func TestSeriesAndBreakdownCarryTheCapCorrection(t *testing.T) {
	api, until := replayCapAPI(t)
	var head struct {
		Corr float64 `json:"replay_cap_correction_usd"`
	}
	api.get(t, fmt.Sprintf("/api/stats?since=0&until=%d", until), &head)
	if head.Corr == 0 {
		t.Fatal("fixture has no credit past the cap")
	}
	for _, path := range []string{
		fmt.Sprintf("/api/series?since=0&until=%d", until),
		fmt.Sprintf("/api/breakdown?dim=model&since=0&until=%d", until),
	} {
		var b struct {
			Corr       float64 `json:"cap_correction_usd"`
			NotApplied bool    `json:"cap_not_applied"`
		}
		api.get(t, path, &b)
		nearEq(t, path+" cap_correction_usd", b.Corr, head.Corr)
		if b.NotApplied {
			t.Errorf("%s: cap_not_applied with a working cap", path)
		}
	}
	api.a.pricer = nil
	var b struct {
		Corr       float64 `json:"cap_correction_usd"`
		NotApplied bool    `json:"cap_not_applied"`
	}
	// /api/series is a cached route: a different bucket is a different key, so this reads fresh.
	api.get(t, fmt.Sprintf("/api/series?since=0&until=%d&bucket=61000", until), &b)
	if !b.NotApplied || b.Corr != 0 {
		t.Errorf("series without a pricer: not_applied=%v corr=%v, want true and 0", b.NotApplied, b.Corr)
	}
}
