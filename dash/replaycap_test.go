package dash

import (
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

// No exact window, no ceiling: nothing is capped.
func TestReplayCapNeedsAnExactWindow(t *testing.T) {
	rows := []capRow{capTestRow(1, 100_000, 900_000, 500_000, "extract_llm", 1)}
	l := walkReplayCap(rows, func(string) (int, bool) { return 200_000, false })
	if len(l.ByComponent) != 0 || l.DeltaUSD() != 0 || l.WindowUnknownCovered != 1 {
		t.Errorf("ledger = %+v, want untouched", l)
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
	row := &ComponentRow{Component: "extract_llm", SavedUSD: c.StoredUSD, NetUSD: c.StoredUSD,
		NetUSDWithEstimate: c.StoredUSD, SavedUSDReplay: c.StoredUSD, SavedUSDDecomposed: c.StoredUSD}
	applyReplayLedgerToRow(row, l)
	nearEq(t, "row saved", row.SavedUSD, c.CountedUSD)
	nearEq(t, "row replay", row.SavedUSDReplay, c.CountedUSD)
	nearEq(t, "row past cap", row.ReplayCreditPastCapUSD, c.PastCapUSD)
	// summarize's own component is left to its ledger.
	other := &ComponentRow{Component: SummarizeComponent, SavedUSD: 1}
	applyReplayLedgerToRow(other, l)
	if other.SavedUSD != 1 {
		t.Errorf("summarize row moved by the replay ledger: %v", other.SavedUSD)
	}
}
