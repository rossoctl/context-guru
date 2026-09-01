package dash

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// findHoldoutCell returns the cell for one (user, hour), or nil.
func findHoldoutCell(out *KVCacheHoldout, user string, hour int) *KVCacheHoldoutCell {
	for i := range out.Cells {
		if out.Cells[i].User == user && out.Cells[i].HourUTC == hour {
			return &out.Cells[i]
		}
	}
	return nil
}

// Two adjacent Sunday-Thursday weeks. 2023-01-01 is a Sunday, so week one runs
// 2023-01-01..2023-01-06 and week two 2023-01-08..2023-01-13 — the same fixed points
// kvcachesuggest_test.go builds from.
func holdoutWeeks() (train, test Window) {
	train = Window{
		Since: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Until: time.Date(2023, 1, 8, 0, 0, 0, 0, time.UTC).UnixMilli(),
	}
	test = Window{
		Since: time.Date(2023, 1, 8, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Until: time.Date(2023, 1, 15, 0, 0, 0, 0, time.UTC).UnixMilli(),
	}
	return train, test
}

// The whole point of the view: an overlapping train and test window is refused, because an
// arm scored on rows it was chosen on is an in-sample number reported as an out-of-sample
// one — a failure that is completely invisible in the output.
func TestKVCacheHoldoutRefusesOverlappingWindows(t *testing.T) {
	db := openTestDB(t)
	day := int64(86_400_000)
	cases := []struct {
		name        string
		train, test Window
	}{
		{"identical", Window{day, 5 * day}, Window{day, 5 * day}},
		{"test inside train", Window{day, 10 * day}, Window{2 * day, 3 * day}},
		{"train inside test", Window{2 * day, 3 * day}, Window{day, 10 * day}},
		{"partial overlap", Window{day, 5 * day}, Window{4 * day, 9 * day}},
		{"one instant of overlap", Window{day, 5 * day}, Window{5*day - 1, 9 * day}},
	}
	for _, c := range cases {
		if _, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
			staticPricer{ibmSonnet}, KVCacheSimConfig{}, c.train, c.test); err == nil {
			t.Errorf("%s: got no error, want a refusal", c.name)
		} else if !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%s: error %q does not say the windows overlap", c.name, err)
		}
	}

	// Until is exclusive, so two windows meeting at one instant are adjacent, not
	// overlapping — the most common split anyone will ask for.
	if _, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{},
		Window{day, 5 * day}, Window{5 * day, 9 * day}); err != nil {
		t.Errorf("adjacent windows were refused: %v", err)
	}
}

// An absent bound is an error, not the "0 means unbounded" every other filter in this
// package uses: an open-ended train window would swallow the test period and silently stop
// being a holdout at all.
func TestKVCacheHoldoutRequiresAllFourBounds(t *testing.T) {
	db := openTestDB(t)
	day := int64(86_400_000)
	good := Window{5 * day, 9 * day}
	cases := []struct {
		name        string
		train, test Window
	}{
		{"no train start", Window{0, 4 * day}, good},
		{"no train end", Window{day, 0}, good},
		{"no test start", Window{day, 4 * day}, Window{0, 9 * day}},
		{"no test end", Window{day, 4 * day}, Window{5 * day, 0}},
		{"train ends before it starts", Window{4 * day, day}, good},
		{"train is empty", Window{day, day}, good},
	}
	for _, c := range cases {
		if _, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
			staticPricer{ibmSonnet}, KVCacheSimConfig{}, c.train, c.test); err == nil {
			t.Errorf("%s: got no error, want a refusal", c.name)
		}
	}
}

// The join that makes this a holdout: the arm chosen on the train window is the arm scored
// on the test window, looked up in the test window's own candidate list — not re-chosen
// there. A cell whose best arm differs between the two windows must still report the
// TRAIN arm's test-window number, because that is the arm a campaign would have enforced.
func TestKVCacheHoldoutScoresTheTrainChosenArmOnTestRows(t *testing.T) {
	train, test := holdoutWeeks()
	// Sunday and Monday of each week, hour 10, one user, enough requests to clear the
	// floor on both sides.
	var evs []*Event
	for week, base := range []time.Time{
		time.Date(2023, 1, 1, 10, 0, 0, 0, time.UTC),
		time.Date(2023, 1, 8, 10, 0, 0, 0, time.UTC),
	} {
		for i := 0; i < 6; i++ {
			ts := base.Add(time.Duration(i) * 3 * time.Minute).UnixMilli()
			evs = append(evs, kvEvent("u1", "s-week-"+string(rune('a'+week)), "m", ts, 100_000, 0))
		}
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}
	cell := findHoldoutCell(out, "u1", 10)
	if cell == nil {
		t.Fatalf("no cell for u1 hour 10: %+v", out.Cells)
	}
	if cell.TrainRequests != 6 || cell.TestRequests != 6 {
		t.Errorf("cell = %d train / %d test requests, want 6 each", cell.TrainRequests, cell.TestRequests)
	}
	if cell.Arm == "" {
		t.Fatal("the cell names no chosen arm")
	}
	// The chosen arm's own pair must be the one promoted onto the cell, and it must be
	// flagged exactly once in Arms.
	chosen := 0
	for _, a := range cell.Arms {
		if !a.Chosen {
			continue
		}
		chosen++
		if a.Strategy != cell.Arm {
			t.Errorf("Arms flags %q as chosen but the cell's arm is %q", a.Strategy, cell.Arm)
		}
		if a.TestSavingUSD != cell.TestSavingUSD || a.TestKnown != cell.TestKnown {
			t.Errorf("the cell's test figure (%v/%v) does not match the chosen arm's (%v/%v)",
				cell.TestSavingUSD, cell.TestKnown, a.TestSavingUSD, a.TestKnown)
		}
	}
	if chosen != 1 {
		t.Errorf("%d arms flagged Chosen, want exactly 1", chosen)
	}
	// Both windows produced a priced cell, so this pair is comparable and counted.
	if out.ComparableCells != 1 {
		t.Errorf("ComparableCells = %d, want 1", out.ComparableCells)
	}
}

// A retention ratio against a train total of exactly zero is undefined, not 0% — the same
// rule kvcache.Savings.Known already encodes for percentages. A cell can genuinely have a
// zero train saving (nothing beat the baseline), so this is reachable, not defensive.
func TestKVCacheHoldoutRetentionIsUnknownAgainstAZeroTrainTotal(t *testing.T) {
	train, test := holdoutWeeks()
	// Requests with no cache activity at all: every arm ties the baseline, so the chosen
	// arm's saving is exactly 0 on both sides.
	var evs []*Event
	for week, base := range []time.Time{
		time.Date(2023, 1, 2, 11, 0, 0, 0, time.UTC),
		time.Date(2023, 1, 9, 11, 0, 0, 0, time.UTC),
	} {
		for i := 0; i < 6; i++ {
			evs = append(evs, kvEvent("flat", "s-"+string(rune('a'+week)), "m",
				base.Add(time.Duration(i)*3*time.Minute).UnixMilli(), 0, 0))
		}
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}
	if out.TotalTrainSavingUSD != 0 {
		t.Skipf("this fixture no longer produces a zero train total (%v); the rule it pins "+
			"still holds, but this is no longer the case that exercises it",
			out.TotalTrainSavingUSD)
	}
	if out.RetentionKnown || out.RetentionPct != 0 {
		t.Errorf("retention = %v/%v, want unknown: a ratio against a zero train total is "+
			"undefined, not 0%%", out.RetentionPct, out.RetentionKnown)
	}
}

// A cell with train traffic but NO test traffic must report its train numbers, say plainly
// why there is no test figure, and be excluded from the totals — never counted as a $0.00
// test result, which would read as "this arm saved nothing" rather than "nothing was
// measured".
func TestKVCacheHoldoutReportsAMissingTestCellRatherThanAZero(t *testing.T) {
	train, test := holdoutWeeks()
	// Six requests in the train week only.
	var evs []*Event
	base := time.Date(2023, 1, 2, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		evs = append(evs, kvEvent("lonely", "s1", "m",
			base.Add(time.Duration(i)*3*time.Minute).UnixMilli(), 100_000, 0))
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}
	cell := findHoldoutCell(out, "lonely", 14)
	if cell == nil {
		t.Fatalf("no cell for the train-only user: %+v", out.Cells)
	}
	if cell.TrainRequests != 6 {
		t.Errorf("TrainRequests = %d, want 6", cell.TrainRequests)
	}
	if cell.TestRequests != 0 {
		t.Errorf("TestRequests = %d, want 0", cell.TestRequests)
	}
	if cell.TestKnown {
		t.Error("TestKnown is true for a cell with no test traffic at all")
	}
	if cell.TestNote == "" {
		t.Error("a cell with no test figure must say why, not just leave it blank")
	}
	if out.ComparableCells != 0 {
		t.Errorf("ComparableCells = %d, want 0: nothing here is comparable", out.ComparableCells)
	}
	if out.RetentionKnown || out.RetentionPct != 0 {
		t.Errorf("retention = %v/%v, want unknown and 0: with no comparable cells there is no "+
			"honest ratio", out.RetentionPct, out.RetentionKnown)
	}
	if out.TotalTestSavingUSD != 0 || out.TotalTrainSavingUSD != 0 {
		t.Errorf("totals = %v train / %v test, want 0 each: an uncomparable cell contributes "+
			"to neither", out.TotalTrainSavingUSD, out.TotalTestSavingUSD)
	}
	// The cell is still REPORTED, with its train numbers — excluded from the totals is not
	// the same as hidden.
	if out.TotalCells != 1 {
		t.Errorf("TotalCells = %d, want 1: the cell must still be reported", out.TotalCells)
	}
}

// A cell thin on EITHER side is excluded from the totals: a train-side breach means the arm
// was chosen from noise, a test-side one means it was scored against noise. Both are still
// reported, flagged, so the exclusion is visible rather than silent.
func TestKVCacheHoldoutExcludesThinCellsFromTheTotals(t *testing.T) {
	train, test := holdoutWeeks()
	var evs []*Event
	// Train: 6 requests (clears the floor of 5). Test: 2 (does not).
	trainBase := time.Date(2023, 1, 2, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		evs = append(evs, kvEvent("thin", "s1", "m",
			trainBase.Add(time.Duration(i)*3*time.Minute).UnixMilli(), 100_000, 0))
	}
	testBase := time.Date(2023, 1, 9, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		evs = append(evs, kvEvent("thin", "s2", "m",
			testBase.Add(time.Duration(i)*3*time.Minute).UnixMilli(), 100_000, 0))
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}
	cell := findHoldoutCell(out, "thin", 9)
	if cell == nil {
		t.Fatalf("no cell for the thin user: %+v", out.Cells)
	}
	if cell.TrainInsufficientData {
		t.Error("TrainInsufficientData is true for a 6-request train side (floor is 5)")
	}
	if !cell.TestInsufficientData {
		t.Error("TestInsufficientData is false for a 2-request test side")
	}
	if out.ComparableCells != 0 {
		t.Errorf("ComparableCells = %d, want 0: the test side is below the floor",
			out.ComparableCells)
	}
	if out.TotalCells != 1 {
		t.Errorf("TotalCells = %d, want 1: a thin cell is excluded from the totals, not hidden",
			out.TotalCells)
	}
}

// The route: a malformed window is the CALLER's mistake and must answer 400, not 500 —
// a 5xx here would fire an alert every time someone typed a bad date. A well-formed pair
// answers 200 on an empty database, the state every new deployment is in.
func TestKVCacheHoldoutRouteRejectsABadWindowAsA400(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	day := int64(86_400_000)
	ms := func(n int64) string { return strconv.FormatInt(n*day, 10) }

	for _, c := range []struct{ name, qs string }{
		{"overlapping windows", "?train_since=" + ms(1) + "&train_until=" + ms(5) +
			"&test_since=" + ms(4) + "&test_until=" + ms(9)},
		{"no bounds at all", ""},
		{"only a train window", "?train_since=" + ms(1) + "&train_until=" + ms(5)},
	} {
		w, _ := get(t, a, "/api/kvcache/suggest/holdout"+c.qs, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400: %s", c.name, w.Code, w.Body.String())
		}
	}

	w, body := get(t, a, "/api/kvcache/suggest/holdout?train_since="+ms(1)+"&train_until="+ms(4)+
		"&test_since="+ms(5)+"&test_until="+ms(9), "")
	if w.Code != http.StatusOK {
		t.Fatalf("valid windows -> %d, want 200: %s", w.Code, w.Body.String())
	}
	if body == nil {
		t.Error("the route served no JSON object")
	}
}

// The analysis read is CAPPED at kvCacheMaxRows and the cap keeps the NEWEST rows in the
// window (see KVCacheDataset). So a window bigger than the cap is silently reduced to its
// own recent tail, and the single-window page shouts about it: KVCacheSuggestions carries
// Scanned/Total/Truncated and dash/ui/kvcache.js renders a warning banner telling the
// reader the analysis covers only the rows that were read.
//
// A holdout needs that louder, not quieter. The two windows are read and capped
// INDEPENDENTLY, so a clipped train window means the arm was chosen on a slice of the
// period the reader asked for, and RetentionPct can divide two totals whose populations
// cover very different fractions of their windows — while every per-cell request count
// reports only the post-cap rows, so nothing in the payload reveals it. That is the one
// silent data loss that undermines every other honesty flag in this file.
func TestKVCacheHoldoutReportsATruncatedWindow(t *testing.T) {
	restore := kvCacheMaxRows
	t.Cleanup(func() { kvCacheMaxRows = restore })
	kvCacheMaxRows = 4

	train, test := holdoutWeeks()
	// 8 train-window requests and 6 test-window ones, all one (user, hour) cell: with the
	// cap at 4, BOTH windows truncate and the train side loses half its rows.
	var evs []*Event
	for i := 0; i < 8; i++ {
		ts := time.Date(2023, 1, 1, 10, 0, 0, 0, time.UTC).
			Add(time.Duration(i) * 3 * time.Minute).UnixMilli()
		evs = append(evs, kvEvent("u1", "s-train", "m", ts, 100_000, 0))
	}
	for i := 0; i < 6; i++ {
		ts := time.Date(2023, 1, 8, 10, 0, 0, 0, time.UTC).
			Add(time.Duration(i) * 3 * time.Minute).UnixMilli()
		evs = append(evs, kvEvent("u1", "s-test", "m", ts, 100_000, 0))
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}

	if !out.TrainTruncated || !out.TestTruncated {
		t.Errorf("TrainTruncated=%v TestTruncated=%v, want both true: a reader cannot be told "+
			"how much of each window was actually read if the payload does not carry it",
			out.TrainTruncated, out.TestTruncated)
	}
	if out.TrainScanned != 4 || out.TrainTotal != 8 {
		t.Errorf("train read %d of %d, want 4 of 8", out.TrainScanned, out.TrainTotal)
	}
	if out.TestScanned != 4 || out.TestTotal != 6 {
		t.Errorf("test read %d of %d, want 4 of 6", out.TestScanned, out.TestTotal)
	}
	// Not just machine-readable: the notes are what the page shows a manager, and a
	// retention figure computed over a clipped window has to say so in words too.
	var said bool
	for _, n := range out.Notes {
		if strings.Contains(n, "capped") || strings.Contains(n, "truncat") {
			said = true
		}
	}
	if !said {
		t.Errorf("no note mentions the cap; notes = %q", out.Notes)
	}
}

// The other half of the same rule: an UNtruncated holdout must not carry a scary banner.
// Truncated stays false and the notes stay quiet, so the flag means something when it is
// set.
func TestKVCacheHoldoutReportsAnUntruncatedWindowAsWhole(t *testing.T) {
	train, test := holdoutWeeks()
	var evs []*Event
	for week, base := range []time.Time{
		time.Date(2023, 1, 1, 10, 0, 0, 0, time.UTC),
		time.Date(2023, 1, 8, 10, 0, 0, 0, time.UTC),
	} {
		for i := 0; i < 6; i++ {
			ts := base.Add(time.Duration(i) * 3 * time.Minute).UnixMilli()
			evs = append(evs, kvEvent("u1", "s-w"+strconv.Itoa(week), "m", ts, 100_000, 0))
		}
	}
	db := seedKV(t, evs...)

	out, err := db.KVCacheSuggestHoldout(allTenants(), KVCacheOptions{},
		staticPricer{ibmSonnet}, KVCacheSimConfig{}, train, test)
	if err != nil {
		t.Fatal(err)
	}
	if out.TrainTruncated || out.TestTruncated {
		t.Errorf("TrainTruncated=%v TestTruncated=%v, want both false on a whole window",
			out.TrainTruncated, out.TestTruncated)
	}
	if out.TrainScanned != 6 || out.TrainTotal != 6 || out.TestScanned != 6 || out.TestTotal != 6 {
		t.Errorf("train %d/%d test %d/%d, want 6/6 each",
			out.TrainScanned, out.TrainTotal, out.TestScanned, out.TestTotal)
	}
	for _, n := range out.Notes {
		if strings.Contains(n, "capped") || strings.Contains(n, "truncat") {
			t.Errorf("an untruncated holdout carries a cap note: %q", n)
		}
	}
}
