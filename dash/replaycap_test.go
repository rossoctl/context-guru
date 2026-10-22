package dash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixedWindow(w int) windowFn { return func(string) (int, bool) { return w, true } }

// row builds a request: billed is the provider's count, removed the tokens a component took out
// (tokens_before - tokens_after), and usd that component's stored credit on it.
func capTestRow(ts int64, before, removed, billed int64, comp string, usd float64) capRow {
	r := capRow{Tenant: "t", Session: "s", Model: "m1", TS: ts,
		TokensBefore: before, TokensAfter: before - removed, Billed: billed}
	if usd != 0 {
		r.Credits = []compCredit{{Component: comp, Gross: removed, Unique: 0, USD: usd}}
	}
	return r
}

// The credit ends on the turn the counterfactual transcript (billed + removed) reaches the
// ceiling, although the real transcript is far below it: the removals must not move the cap.
func TestReplayCapEndsAtTheCounterfactualCompaction(t *testing.T) {
	// m1 has an unmeasured tokenizer factor, so f = 1. Window 1,000,000 at ceiling 1.00.
	rows := []capRow{
		capTestRow(1, 100_000, 50_000, 400_000, "extract_llm", 1),  // cf 450k: counted
		capTestRow(2, 110_000, 50_000, 450_000, "extract_llm", 1),  // cf 500k: counted
		capTestRow(3, 120_000, 500_000, 480_000, "extract_llm", 1), // cf 980k: counted
		capTestRow(4, 130_000, 500_000, 500_000, "extract_llm", 1), // cf 1,000k: the world without us compacted
		capTestRow(5, 140_000, 500_000, 510_000, "extract_llm", 1), // after
	}
	l := walkReplayCap(rows, fixedWindow(1_000_000))
	c := l.ByComponent["extract_llm"]
	nearEq(t, "counted", c.CountedUSD, 3)
	nearEq(t, "past cap", c.PastCapUSD, 2)
	nearEq(t, "past cf", c.PastCounterfactual, 2)
	nearEq(t, "past real", c.PastClientCompact, 0)
	nearEq(t, "delta", l.DeltaUSD(), -2)
	if l.ClosedCounterfactual != 1 || l.ClosedClientCompaction != 0 {
		t.Errorf("closed cf=%d real=%d, want 1/0", l.ClosedCounterfactual, l.ClosedClientCompaction)
	}
}

// A real client compaction: tokens_before falls after the agent's own compaction request went by.
func TestReplayCapEndsAtARealClientCompaction(t *testing.T) {
	compaction := capTestRow(3, 130_000, 0, 130_000, "", 0)
	compaction.Bypassed = true
	rows := []capRow{
		capTestRow(1, 100_000, 10_000, 100_000, "searchfold", 1),
		capTestRow(2, 120_000, 10_000, 120_000, "searchfold", 1),
		compaction,
		capTestRow(4, 30_000, 10_000, 30_000, "searchfold", 1), // the client compacted
		capTestRow(5, 40_000, 10_000, 40_000, "searchfold", 1),
	}
	l := walkReplayCap(rows, fixedWindow(1_000_000))
	c := l.ByComponent["searchfold"]
	nearEq(t, "counted", c.CountedUSD, 2)
	nearEq(t, "past real", c.PastClientCompact, 2)
	nearEq(t, "past cf", c.PastCounterfactual, 0)
	if l.ClosedClientCompaction != 1 {
		t.Errorf("closed by client compaction = %d, want 1", l.ClosedClientCompaction)
	}
}

// A subagent shares the session and the model, and its smaller transcript interleaves with the
// main one. That drop is not a client compaction: no compaction request went by.
func TestReplayCapDoesNotReadASubagentAsACompaction(t *testing.T) {
	rows := []capRow{
		capTestRow(1, 100_000, 10_000, 100_000, "extract_llm", 1),
		capTestRow(2, 120_000, 10_000, 120_000, "extract_llm", 1),
		capTestRow(3, 5_000, 1_000, 5_000, "extract_llm", 1), // subagent turn
		capTestRow(4, 130_000, 10_000, 130_000, "extract_llm", 1),
	}
	l := walkReplayCap(rows, fixedWindow(1_000_000))
	nearEq(t, "counted", l.ByComponent["extract_llm"].CountedUSD, 4)
	if l.ClosedClientCompaction != 0 {
		t.Errorf("a subagent turn read as a client compaction")
	}
}

// The compaction request can be on another model than the turn it shrinks (a summarizer fork),
// so the evidence is looked up across the session.
func TestReplayCapFindsTheCompactionRequestOnAnotherModel(t *testing.T) {
	other := capTestRow(3, 130_000, 0, 130_000, "", 0)
	other.Model, other.Bypassed = "m2", true
	rows := []capRow{
		capTestRow(1, 100_000, 10_000, 100_000, "extract_llm", 1),
		capTestRow(2, 120_000, 10_000, 120_000, "extract_llm", 1),
		capTestRow(4, 30_000, 10_000, 30_000, "extract_llm", 1),
	}
	// Ordered by model: m1's rows, then m2's.
	l := walkReplayCap(append(rows, other), fixedWindow(1_000_000))
	nearEq(t, "past real", l.ByComponent["extract_llm"].PastClientCompact, 1)
}

// Every capped component follows the same rule; summarize keeps its own ledger.
func TestReplayCapAppliesToEveryComponentOnTheTurn(t *testing.T) {
	r := capTestRow(1, 100_000, 600_000, 500_000, "extract_llm", 0) // cf 1.1M: closed
	r.Credits = []compCredit{
		{Component: "extract_llm", Gross: 1, USD: 1}, {Component: "dedup", Gross: 1, USD: 2},
	}
	l := walkReplayCap([]capRow{r}, fixedWindow(1_000_000))
	nearEq(t, "extract_llm", l.ByComponent["extract_llm"].PastCapUSD, 1)
	nearEq(t, "dedup", l.ByComponent["dedup"].PastCapUSD, 2)
	nearEq(t, "total", l.DeltaUSD(), -3)
}

// The first-removal / replay split follows the credit through the cap.
func TestReplayCapSplitsFirstRemovalAndReplay(t *testing.T) {
	r1 := capTestRow(1, 100_000, 10_000, 100_000, "extract_llm", 0)
	r1.Credits = []compCredit{{Component: "extract_llm", Gross: 10, USD: 4, FirstUSD: 3}}
	r2 := capTestRow(2, 110_000, 600_000, 500_000, "extract_llm", 0) // cf 1.1M
	r2.Credits = []compCredit{{Component: "extract_llm", Gross: 10, USD: 5, FirstUSD: 2}}
	c := walkReplayCap([]capRow{r1, r2}, fixedWindow(1_000_000)).ByComponent["extract_llm"]
	nearEq(t, "first", c.FirstRemovalUSD, 3)
	nearEq(t, "replay", c.ReplayUSD, 1)
	nearEq(t, "past first", c.PastCapFirstUSD, 2)
	nearEq(t, "stored", c.StoredUSD, 9)
}

// No exact window: the credit is capped against a conservative assumed window and labelled so,
// not left uncapped.
func TestReplayCapAssumesAWindowWhenNoneIsExact(t *testing.T) {
	unknown := func(string) (int, bool) { return 0, false }
	rows := []capRow{
		capTestRow(1, 50_000, 10_000, 50_000, "extract_llm", 1),    // cf 60k: under the assumed 200k
		capTestRow(2, 300_000, 100_000, 150_000, "extract_llm", 1), // cf 250k: past it
	}
	l := walkReplayCap(rows, unknown)
	c := l.ByComponent["extract_llm"]
	nearEq(t, "counted", c.CountedUSD, 1)
	nearEq(t, "past cap", c.PastCapUSD, 1)
	nearEq(t, "assumed", c.WindowAssumedUSD, 2)
	// A larger guessed window is lowered to the assumed one; a smaller one is kept.
	big := func(string) (int, bool) { return 1_000_000, false }
	nearEq(t, "inexact 1M", walkReplayCap(rows, big).ByComponent["extract_llm"].PastCapUSD, 1)
	small := func(string) (int, bool) { return 50_000, false }
	nearEq(t, "inexact 50k", walkReplayCap(rows, small).ByComponent["extract_llm"].PastCapUSD, 2)
	row := &ComponentRow{Component: "extract_llm", SavedUSD: 2}
	applyReplayLedgerToRow(row, l)
	if !row.ReplayWindowAssumed {
		t.Error("row is not labelled window assumed")
	}
}

// The tokenizer factor scales the added-back tokens into the ceiling's ruler.
func TestReplayCapScalesRemovedTokensByTheBilledFactor(t *testing.T) {
	mk := func(model string) capRow {
		r := capTestRow(1, 100_000, 400_000, 700_000, "extract_llm", 1)
		r.Model = model
		return r
	}
	// 700k + 400k = 1.1M closes at f=1 (m1); the same row on a haiku (f=1.3686) closes too, and
	// 700k + 400k*1.3686 is farther past the ceiling than 1.1M.
	if d := walkReplayCap([]capRow{mk("m1")}, fixedWindow(1_000_000)).DeltaUSD(); d != -1 {
		t.Errorf("m1 delta = %v, want -1", d)
	}
	r := capTestRow(2, 100_000, 250_000, 700_000, "extract_llm", 1)
	r.Model = "claude-haiku-4-5"
	// 700k + 250k = 950k is below the ceiling at f=1 but 700k + 250k*1.3686 = 1.042M is above it.
	if d := walkReplayCap([]capRow{r}, fixedWindow(1_000_000)).DeltaUSD(); d != -1 {
		t.Errorf("haiku delta = %v, want -1 (factor applied)", d)
	}
}

// Through SQL: stored component rows are read, capped, and move the headline and the row.
func TestReplayLedgerThroughSQLMovesTheHeadlineAndTheRow(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, before, after int, fresh, read int64, usd float64) *Event {
		e := mkEvent(ts, "s", "m1", before, after)
		e.TenantID = "t1"
		e.FreshInput, e.CacheRead = fresh, read
		e.CacheMissReason = CacheHit
		e.TokenAccounting = AccountingComplete
		e.Components = []CompRow{{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: before - after, SavedUnique: 0, SavedUSD: usd}}
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(DayMs+1, 300_000, 200_000, 1_000, 300_000, 0.5),
		mk(DayMs+2, 310_000, 200_000, 1_000, 310_000, 0.5),
		mk(DayMs+3, 320_000, 100_000, 1_000, 620_000, 0.5), // cf = 621k + 220k: still open
		mk(DayMs+4, 330_000, 100_000, 1_000, 900_000, 0.5), // cf = 901k + 230k: closed
		mk(DayMs+5, 340_000, 100_000, 1_000, 900_000, 0.5),
	}); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	c := l.ByComponent["extract_llm"]
	if c == nil {
		t.Fatalf("no extract_llm in %+v", l)
	}
	if c.Rows != 5 || c.PastRows != 2 {
		t.Errorf("rows = %d past = %d, want 5 and 2", c.Rows, c.PastRows)
	}
	o := &Overview{BaselineCostUSD: 10, NetSavedUSD: 4, TotalSavedUSD: 5, TotalReducedUSD: 5}
	o.ApplyReplayLedger(l)
	nearEq(t, "baseline", o.BaselineCostUSD, 10+l.DeltaUSD())
	nearEq(t, "correction", o.ReplayCapCorrectionUSD, l.DeltaUSD())
	nearEq(t, "net saved", o.NetSavedUSD, 4+l.DeltaUSD())
	nearEq(t, "total saved", o.TotalSavedUSD, 5+l.DeltaUSD())
	nearEq(t, "total reduced", o.TotalReducedUSD, 5+l.DeltaUSD())
	if l.DeltaUSD() >= 0 {
		t.Fatal("the fixture does not cap anything")
	}
	row := &ComponentRow{Component: "extract_llm", SavedUSD: c.StoredUSD, NetUSD: c.StoredUSD,
		NetUSDWithEstimate: c.StoredUSD, SavedUSDReplay: c.StoredUSD, SavedUSDDecomposed: c.StoredUSD}
	applyReplayLedgerToRow(row, l)
	nearEq(t, "row saved", row.SavedUSD, c.CountedUSD)
	nearEq(t, "row net", row.NetUSD, c.CountedUSD)
	nearEq(t, "row net with estimate", row.NetUSDWithEstimate, c.CountedUSD)
	nearEq(t, "row replay", row.SavedUSDReplay, c.CountedUSD)
	nearEq(t, "row past cap", row.ReplayCreditPastCapUSD, c.PastCapUSD)
	// summarize's own component is left to its ledger.
	for _, name := range SummarizerComponents {
		other := &ComponentRow{Component: name, SavedUSD: 1}
		applyReplayLedgerToRow(other, l)
		if other.SavedUSD != 1 {
			t.Errorf("%s row moved by the replay ledger: %v", name, other.SavedUSD)
		}
	}
}

// The three read paths agree: the headline (full and session-scoped lean), /api/components and the
// episode view all read the one ledger.
func TestReplayCapReachesEveryReadPath(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, before, after int, fresh, read int64) *Event {
		e := mkEvent(ts, "s", "aws/claude-sonnet-5", before, after)
		e.TenantID = "t1"
		e.FreshInput, e.CacheRead, e.CacheWrite = fresh, read, 0
		e.TokenAccounting = AccountingComplete
		e.Components = []CompRow{{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: before - after, SavedUnique: 0, SavedUSD: 1}}
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(DayMs+1, 300_000, 200_000, 1_000, 300_000), // cf 300k + 1.69*100k: open
		// 700k - 100k removed = 600k * 1.6857 = 1.01M on top of 450k: the world without us compacted.
		mk(DayMs+2, 700_000, 100_000, 1_000, 449_000),
		mk(DayMs+3, 710_000, 100_000, 1_000, 450_000),
	}); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{ibmSonnet})
	api.SetWindows(testWindows{})
	mux := http.NewServeMux()
	api.Mount(mux)
	get := func(path string, v any) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	type stats struct {
		Correction float64 `json:"replay_cap_correction_usd"`
		TotalSaved float64 `json:"total_saved_usd"`
	}
	var full, lean stats
	get("/api/stats?session=s", &full)
	get("/api/stats?lean=1&session=s", &lean)
	nearEq(t, "full correction", full.Correction, -2)
	nearEq(t, "lean correction", lean.Correction, -2)
	nearEq(t, "full and lean agree", full.TotalSaved, lean.TotalSaved)

	var comps struct {
		Components []ComponentRow `json:"components"`
	}
	get("/api/components?session=s", &comps)
	var row *ComponentRow
	for i := range comps.Components {
		if comps.Components[i].Component == "extract_llm" {
			row = &comps.Components[i]
		}
	}
	if row == nil {
		t.Fatal("no extract_llm row")
	}
	nearEq(t, "row saved", row.SavedUSD, 1)
	nearEq(t, "row past cap", row.ReplayCreditPastCapUSD, 2)

	var eps CompactionEpisodes
	get("/api/components/compaction-episodes?session=s", &eps)
	if eps.ReplayLedger == nil {
		t.Fatal("the episode view carries no replay ledger")
	}
	nearEq(t, "episode view past cap", eps.ReplayLedger.ByComponent["extract_llm"].PastCapUSD, 2)
}

// An interleaved subagent turn right after a compaction request is not a compaction: the main
// thread's next turn is as large as before. A real compaction stays small.
func TestReplayCapRequiresTheDropToPersist(t *testing.T) {
	compaction := capTestRow(3, 130_000, 0, 130_000, "", 0)
	compaction.Bypassed = true
	mk := func(afterDrop int64) []capRow {
		return []capRow{
			capTestRow(1, 100_000, 10_000, 100_000, "searchfold", 1),
			capTestRow(2, 120_000, 10_000, 120_000, "searchfold", 1),
			compaction,
			capTestRow(4, 30_000, 10_000, 30_000, "searchfold", 1), // the drop
			capTestRow(5, afterDrop, 10_000, afterDrop, "searchfold", 1),
		}
	}
	if l := walkReplayCap(mk(135_000), fixedWindow(1_000_000)); l.ClosedClientCompaction != 0 {
		t.Errorf("a drop the main thread recovered from read as a compaction")
	}
	if l := walkReplayCap(mk(40_000), fixedWindow(1_000_000)); l.ClosedClientCompaction != 1 {
		t.Errorf("a persistent drop after the compaction request was missed")
	}
}

// request_components has no UNIQUE: two rows for one (request, component) count once, at the larger.
func TestReplayLedgerKeepsTheLargerOfDuplicateRows(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(DayMs+1, "s", "m1", 300_000, 200_000)
	e.TenantID = "t1"
	e.TokenAccounting = AccountingComplete
	e.Components = []CompRow{
		{Component: "extract_llm", Acted: true, SavedGross: 100_000, SavedUSD: 0.25},
		{Component: "extract_llm", Acted: true, SavedGross: 100_000, SavedUSD: 0.75},
		{Component: "extract_llm", Acted: true, SavedGross: 100_000, SavedUSD: 0.5},
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	nearEq(t, "stored", l.ByComponent["extract_llm"].StoredUSD, 0.75)
	if l.ByComponent["extract_llm"].Rows != 1 {
		t.Errorf("rows = %d, want 1", l.ByComponent["extract_llm"].Rows)
	}
}

// A cancelled caller stops the scan.
func TestReplayLedgerStopsWhenItsContextIsCancelled(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{mkEvent(DayMs+1, "s", "m1", 1000, 900)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.ReplayLedgerForCtx(ctx, Filter{TenantAll: true}, testWindows{}, handPrice); err == nil {
		t.Error("a cancelled context did not stop the scan")
	}
}

// The assumed window is the smallest of the known window, the family floor and 200k.
func TestAssumedWindowIsTheSmallestOfKnownFamilyAndDefault(t *testing.T) {
	for _, tc := range []struct {
		model string
		known int
		want  int
	}{
		{"claude-x", 0, 200_000}, {"claude-x", 1_000_000, 200_000}, {"claude-x", 100_000, 100_000},
		{"gpt-4o-mini", 0, 128_000}, {"gpt-4o-mini", 1_000_000, 128_000}, {"mistral-large", 0, 32_000},
		{"Llama-3", 0, 128_000},
	} {
		if got := assumedWindowFor(tc.model, tc.known); got != tc.want {
			t.Errorf("assumedWindowFor(%q, %d) = %d, want %d", tc.model, tc.known, got, tc.want)
		}
	}
}

// Every summarizer is outside the replay ledger: the summarize ledger owns it.
func TestReplayLedgerLeavesEverySummarizerToItsOwnLedger(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(DayMs+1, "s", "m1", 300_000, 200_000)
	e.TenantID = "t1"
	e.TokenAccounting = AccountingComplete
	e.Components = nil
	for _, name := range append([]string{"extract_llm"}, SummarizerComponents...) {
		e.Components = append(e.Components, CompRow{Component: name, Acted: true, SavedGross: 100_000, SavedUSD: 1})
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.ByComponent) != 1 || l.ByComponent["extract_llm"] == nil {
		t.Errorf("ledger components = %v, want only extract_llm", l.ByComponent)
	}
}

// A drop that persists but with no compaction request in between is not a client compaction: the
// request is the evidence, not the drop alone.
func TestReplayCapNeedsTheCompactionRequestAsEvidence(t *testing.T) {
	rows := []capRow{
		capTestRow(1, 100_000, 10_000, 100_000, "searchfold", 1),
		capTestRow(2, 120_000, 10_000, 120_000, "searchfold", 1),
		capTestRow(3, 30_000, 10_000, 30_000, "searchfold", 1), // a persistent drop, no request
		capTestRow(4, 40_000, 10_000, 40_000, "searchfold", 1),
		capTestRow(5, 50_000, 10_000, 50_000, "searchfold", 1),
	}
	l := walkReplayCap(rows, fixedWindow(1_000_000))
	nearEq(t, "counted", l.ByComponent["searchfold"].CountedUSD, 5)
	if l.ClosedClientCompaction != 0 {
		t.Error("a drop with no compaction request closed the credit")
	}
	// A compaction request BEFORE the previous turn is not evidence for this drop either.
	early := capTestRow(1, 100_000, 0, 100_000, "", 0)
	early.Bypassed = true
	rows2 := append([]capRow{early}, rows...)
	for i := range rows2[1:] {
		rows2[i+1].TS += 10
	}
	if l := walkReplayCap(rows2, fixedWindow(1_000_000)); l.ClosedClientCompaction != 0 {
		t.Error("a compaction request before the previous turn closed the credit")
	}
	// And one after the drop is not evidence either.
	late := capTestRow(99, 100_000, 0, 100_000, "", 0)
	late.Bypassed = true
	if l := walkReplayCap(append(append([]capRow{}, rows...), late), fixedWindow(1_000_000)); l.ClosedClientCompaction != 0 {
		t.Error("a compaction request after the drop closed the credit")
	}
}

// firstShare prices the unique part through Event.ComponentSavedUSD: on a hit turn a first removal is
// worth the creation rate and the replay the read rate, so its share exceeds its token share.
func TestFirstShareIsPricedNotCountedInTokens(t *testing.T) {
	price := handPrice["m1"]
	ev := &Event{Model: "m1", CacheMissReason: CacheHit, FreshInput: 10, CacheRead: 50_000}
	ev.BilledTokenFactor = 1
	got := firstShare(ev, price, true, "extract_llm", 1000, 100)
	want := 100 * price.CacheWrite / (100*price.CacheWrite + 900*price.CacheRead)
	nearEq(t, "priced share", got, want)
	nearEq(t, "token share without a price", firstShare(ev, price, false, "extract_llm", 1000, 100), 0.1)
	nearEq(t, "no unique part", firstShare(ev, price, true, "extract_llm", 1000, 0), 0)
	nearEq(t, "unique clamped to gross", firstShare(ev, price, true, "extract_llm", 100, 500), 1)
}

// ... and the ledger reads the split through it.
func TestReplayLedgerSplitsFirstRemovalThroughFirstShare(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(DayMs+1, "s", "m1", 300_000, 299_000)
	e.TenantID = "t1"
	e.TokenAccounting = AccountingComplete
	e.FreshInput, e.CacheRead, e.CacheWrite = 10, 50_000, 0
	e.CacheMissReason = CacheHit
	e.Components = []CompRow{{Component: "extract_llm", Acted: true, SavedGross: 1000, SavedUnique: 100, SavedUSD: 2}}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	price := handPrice["m1"]
	share := 100 * price.CacheWrite / (100*price.CacheWrite + 900*price.CacheRead)
	c := l.ByComponent["extract_llm"]
	nearEq(t, "first removal", c.FirstRemovalUSD, 2*share)
	nearEq(t, "replay", c.ReplayUSD, 2*(1-share))
}

// The lean and the full read of one window give the same net and total, so the figure on the page
// does not move when the full read replaces the lean one.
func TestLeanStatsMatchFullForTheSameWindow(t *testing.T) {
	api, until := replayCapAPI(t)
	type body struct {
		Net        float64 `json:"net_saved_usd"`
		Total      float64 `json:"total_saved_usd"`
		Reduced    float64 `json:"total_reduced_usd"`
		Correction float64 `json:"replay_cap_correction_usd"`
	}
	for _, path := range []string{fmt.Sprintf("/api/stats?since=0&until=%d", until), "/api/stats?session=s"} {
		var full, lean body
		api.get(t, path, &full)
		api.get(t, strings.Replace(path, "/api/stats?", "/api/stats?lean=1&", 1), &lean)
		if full.Correction >= 0 {
			t.Fatalf("%s: the fixture is not capped (%v)", path, full.Correction)
		}
		nearEq(t, path+" correction", lean.Correction, full.Correction)
		nearEq(t, path+" net_saved_usd", lean.Net, full.Net)
		nearEq(t, path+" total_saved_usd", lean.Total, full.Total)
		nearEq(t, path+" total_reduced_usd", lean.Reduced, full.Reduced)
	}
}

// When the cap cannot be computed the headline says so, on both reads, instead of silently showing
// the uncapped figure.
func TestHeadlineSaysWhenTheCapWasNotApplied(t *testing.T) {
	api, until := replayCapAPI(t)
	api.a.replayLedgerFn = func(*DB, Filter) (*ReplayLedger, error) { return nil, errors.New("timed out") }
	for _, path := range []string{fmt.Sprintf("/api/stats?since=0&until=%d", until), fmt.Sprintf("/api/stats?lean=1&since=0&until=%d", until)} {
		var b struct {
			NotApplied bool    `json:"replay_cap_not_applied"`
			Correction float64 `json:"replay_cap_correction_usd"`
		}
		api.get(t, path, &b)
		if !b.NotApplied || b.Correction != 0 {
			t.Errorf("%s: not_applied=%v correction=%v, want true and 0", path, b.NotApplied, b.Correction)
		}
	}
	var ok struct {
		NotApplied bool `json:"replay_cap_not_applied"`
	}
	api.a.replayLedgerFn = nil
	api.a.statsCache = jsonCache{}
	api.get(t, fmt.Sprintf("/api/stats?since=0&until=%d", until), &ok)
	if ok.NotApplied {
		t.Error("a working cap reported not_applied")
	}
}

type replayCapHarness struct {
	a   *API
	mux *http.ServeMux
}

func (h replayCapHarness) get(t *testing.T, path string, v any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// replayCapAPI serves a session whose credit is capped from its second turn.
func replayCapAPI(t *testing.T) (replayCapHarness, int64) {
	t.Helper()
	db := openTestDB(t)
	mk := func(ts int64, before, after int, fresh, read int64) *Event {
		e := mkEvent(ts, "s", "aws/claude-sonnet-5", before, after)
		e.TenantID = "t1"
		e.FreshInput, e.CacheRead, e.CacheWrite = fresh, read, 0
		e.TokenAccounting = AccountingComplete
		e.Components = []CompRow{{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: before - after, SavedUnique: 0, SavedUSD: 1}}
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(DayMs+1, 300_000, 200_000, 1_000, 300_000),
		mk(DayMs+2, 700_000, 100_000, 1_000, 449_000),
		mk(DayMs+3, 710_000, 100_000, 1_000, 450_000),
	}); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{ibmSonnet})
	api.SetWindows(testWindows{})
	mux := http.NewServeMux()
	api.Mount(mux)
	return replayCapHarness{a: api, mux: mux}, DayMs + 10
}

// With no price table or no window resolver the cap cannot run, and the headline says so.
func TestHeadlineSaysCapNotAppliedWithoutAPriceTable(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pricer, window bool
	}{{"no pricer", false, true}, {"no windows", true, false}, {"neither", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			api, until := replayCapAPI(t)
			if !tc.pricer {
				api.a.pricer = nil
			}
			if !tc.window {
				api.a.windows = nil
			}
			for _, path := range []string{fmt.Sprintf("/api/stats?since=0&until=%d", until), fmt.Sprintf("/api/stats?lean=1&since=0&until=%d", until)} {
				var b struct {
					NotApplied bool `json:"replay_cap_not_applied"`
				}
				api.get(t, path, &b)
				if !b.NotApplied {
					t.Errorf("%s: replay_cap_not_applied is false with the cap unable to run", path)
				}
			}
		})
	}
}

// The agent's own compaction request is no longer bypassed. A row flagged AgentCompaction alone
// closes the cap, exactly as the bypassed row of an older proxy does.
func TestReplayCapEndsAtAnAgentCompactionRowThatIsNotBypassed(t *testing.T) {
	for _, tc := range []struct {
		name            string
		bypassed, agent bool
	}{
		{"new row: agent_compaction only", false, true},
		{"old row: bypassed only", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compaction := capTestRow(3, 130_000, 0, 130_000, "", 0)
			compaction.Bypassed, compaction.AgentCompaction = tc.bypassed, tc.agent
			rows := []capRow{
				capTestRow(1, 100_000, 10_000, 100_000, "searchfold", 1),
				capTestRow(2, 120_000, 10_000, 120_000, "searchfold", 1),
				compaction,
				capTestRow(4, 30_000, 10_000, 30_000, "searchfold", 1), // the client compacted
				capTestRow(5, 40_000, 10_000, 40_000, "searchfold", 1),
			}
			l := walkReplayCap(rows, fixedWindow(1_000_000))
			c := l.ByComponent["searchfold"]
			nearEq(t, "counted", c.CountedUSD, 2)
			nearEq(t, "past real", c.PastClientCompact, 2)
			if l.ClosedClientCompaction != 1 {
				t.Errorf("closed by client compaction = %d, want 1", l.ClosedClientCompaction)
			}
		})
	}
	// Control: no flag on the compaction row, the cap stays open and credit is not cut.
	rows := []capRow{
		capTestRow(1, 100_000, 10_000, 100_000, "searchfold", 1),
		capTestRow(3, 130_000, 0, 130_000, "", 0),
		capTestRow(4, 30_000, 10_000, 30_000, "searchfold", 1),
	}
	if l := walkReplayCap(rows, fixedWindow(1_000_000)); l.ClosedClientCompaction != 0 {
		t.Errorf("an unflagged row closed the cap: %d", l.ClosedClientCompaction)
	}
}

// Through SQL: the flag is stored on the row and read back by the cap query, so a compaction row
// with bypassed = 0 and agent_compaction = 1 closes the cap.
func TestReplayLedgerThroughSQLReadsAgentCompaction(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, before, after int, comp, agent bool) *Event {
		e := mkEvent(ts, "s", "m1", before, after)
		e.TenantID = "t1"
		e.FreshInput, e.CacheRead = 1_000, int64(before)
		e.CacheMissReason = CacheHit
		e.TokenAccounting = AccountingComplete
		e.AgentCompaction = agent
		if comp {
			e.Components = []CompRow{{Component: "searchfold", Kind: "offload", Acted: true, Mutated: true,
				SavedGross: before - after, SavedUSD: 1}}
		}
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(DayMs+1, 100_000, 90_000, true, false),
		mk(DayMs+2, 120_000, 110_000, true, false),
		mk(DayMs+3, 130_000, 130_000, false, true), // the compaction request: not bypassed
		mk(DayMs+4, 30_000, 20_000, true, false),   // the client compacted
	}); err != nil {
		t.Fatal(err)
	}
	l, err := db.ReplayLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if l.ClosedClientCompaction != 1 {
		t.Fatalf("closed by client compaction = %d, want 1 (the agent_compaction column was not read)", l.ClosedClientCompaction)
	}
	nearEq(t, "past real", l.ByComponent["searchfold"].PastClientCompact, 1)
}
