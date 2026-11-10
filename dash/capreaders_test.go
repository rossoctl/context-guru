package dash

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

// The readers that sum stored rows must land on the headline's figure once the cap's correction is
// added back (#586). The fixture's second and third turn are past the cap, so the correction is real.
// It has replay credit only, so replay_cap_correction_usd IS the whole correction here; the summarize
// half is covered by TestSavingsTotalsCappedCountsTheSummarizeLedger.

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

// The summarize half: a session whose summarize credit runs past its span. The fixture of
// replayCapAPI has no summarizer row, so this one covers the other ledger, and the sum of both.
func TestSavingsTotalsCappedCountsTheSummarizeLedger(t *testing.T) {
	db := openTestDB(t)
	const stored1, storedReplay = 1000 * 1.25e-6, 1000 * 1e-7
	err := db.insertBatch([]*Event{
		summarizeEvent(DayMs+1, 300_000, "s", 1_000, 50_000, CacheHit, 1000, 1000, stored1, nil),
		summarizeEvent(DayMs+2, 310_000, "s", 10_000, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+3, 320_000, "s", 100_000, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+4, 330_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+5, 340_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := Filter{TenantAll: true, Session: "s"}
	sl, err := db.SummarizeLedgerFor(f, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if sl.DeltaUSD() >= 0 {
		t.Fatal("fixture has no summarize credit past the span — the test proves nothing")
	}
	rl, err := db.ReplayLedgerFor(f, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if rl.DeltaUSD() != 0 {
		t.Fatalf("a summarize-only fixture moved the replay ledger by %v", rl.DeltaUSD())
	}
	plain, _ := db.SavingsTotals(f)
	capped, err := db.SavingsTotalsCapped(f, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	// The headline applies the same two deltas to the same three totals.
	o := &Overview{BaselineCostUSD: plain.BaselineCostUSD, NetSavedUSD: plain.NetSavedUSD,
		TotalSavedUSD: plain.TotalSavedUSD, TotalReducedUSD: plain.TotalSavedUSD}
	o.ApplySummarizeLedger(sl)
	o.ApplyReplayLedger(rl)
	nearEq(t, "cap_correction_usd", capped.CapCorrectionUSD, sl.DeltaUSD()+rl.DeltaUSD())
	nearEq(t, "baseline_cost_usd", capped.BaselineCostUSD, o.BaselineCostUSD)
	nearEq(t, "net_saved_usd", capped.NetSavedUSD, o.NetSavedUSD)
	nearEq(t, "total_saved_usd", capped.TotalSavedUSD, o.TotalSavedUSD)
	if capped.CapNotApplied {
		t.Error("cap reported not applied with both ledgers running")
	}
}

// A ledger that fails leaves the other's correction in place and sets the label, as the headline
// does: the readers must not fall back to the stored figure while the headline carries part of the cap.
// The fixture holds both kinds of capped credit, so each failure leaves a non-zero part.
func TestCapCorrectionKeepsTheLedgerThatRan(t *testing.T) {
	db := openTestDB(t)
	const stored1, storedReplay = 1000 * 1.25e-6, 1000 * 1e-7
	evs := []*Event{
		summarizeEvent(DayMs+1, 300_000, "s", 1_000, 50_000, CacheHit, 1000, 1000, stored1, nil),
		summarizeEvent(DayMs+2, 310_000, "s", 10_000, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+3, 320_000, "s", 100_000, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+4, 330_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay, nil),
		summarizeEvent(DayMs+5, 340_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay, nil),
	}
	// An offloader on the same session, credited every turn, so the replay ledger has a correction.
	for i, e := range evs {
		// Billed 500k plus 500k removed reaches the 1M ceiling of testWindows from the second turn on.
		e.Components = append(e.Components, CompRow{Component: "extract_llm", Kind: "offload", Acted: true,
			Mutated: true, SavedGross: 500_000, SavedUnique: 0, SavedUSD: 1})
		e.TokensBefore, e.TokensAfter = 800_000+i*10_000, 300_000+i*10_000
		e.FreshInput, e.CacheRead, e.CacheWrite = 10, 500_000, 0
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	f := Filter{TenantAll: true, Session: "s"}
	sl, err := db.SummarizeLedgerFor(f, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := db.ReplayLedgerFor(f, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if sl.DeltaUSD() == 0 || rl.DeltaUSD() == 0 {
		t.Fatalf("fixture: want both ledgers non-zero, got summarize %v replay %v", sl.DeltaUSD(), rl.DeltaUSD())
	}
	boom := func(Filter) (*ReplayLedger, error) { return nil, errors.New("timed out") }

	both := db.CapCorrection(f, testWindows{}, handPrice)
	nearEq(t, "both", both.CorrectionUSD, sl.DeltaUSD()+rl.DeltaUSD())
	if both.NotApplied {
		t.Error("both ledgers ran and the result says not applied")
	}
	// The replay scan fails: the summarize part stays.
	noReplay := db.capCorrection(f, testWindows{}, handPrice, boom)
	nearEq(t, "summarize part kept", noReplay.CorrectionUSD, sl.DeltaUSD())
	if !noReplay.NotApplied {
		t.Error("a failed replay scan was not labelled")
	}
	// No resolver: nothing runs.
	none := db.CapCorrection(f, nil, handPrice)
	if !none.NotApplied || none.CorrectionUSD != 0 {
		t.Errorf("no resolver: got %+v, want not_applied and 0", none)
	}
}

// A replay failure reaches series and breakdown through the same hook the headline uses.
func TestSeriesAndBreakdownSayWhenTheReplayLedgerFailed(t *testing.T) {
	api, until := replayCapAPI(t)
	api.a.replayLedgerFn = func(*DB, Filter) (*ReplayLedger, error) { return nil, errors.New("timed out") }
	for _, path := range []string{
		fmt.Sprintf("/api/series?since=0&until=%d&bucket=62000", until),
		fmt.Sprintf("/api/breakdown?dim=agent&since=0&until=%d", until),
	} {
		var b struct {
			NotApplied bool `json:"cap_not_applied"`
		}
		api.get(t, path, &b)
		if !b.NotApplied {
			t.Errorf("%s: cap_not_applied is false with the replay ledger failing", path)
		}
	}
}
