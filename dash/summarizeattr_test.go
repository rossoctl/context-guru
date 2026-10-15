package dash

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// The first removal of a summary is priced at the rate the previous turn paid for that content.
// A live cache means the removed tokens, left in, would have been READ. A lapsed one means they
// would have been re-created. Every other component keeps the write rate: its content is new.
func TestSummarizeFirstRemovalIsPricedAtTheRateThePreviousTurnPaid(t *testing.T) {
	price := handPrice["m1"]
	for _, tc := range []struct {
		name, component, reason string
		want                    float64
	}{
		{"hit reads", SummarizeComponent, CacheHit, price.CacheRead},
		{"our own rewrite of a live entry reads", SummarizeComponent, CachePrefixChange, price.CacheRead},
		{"a lapsed entry re-creates", SummarizeComponent, CacheTTLExpiry, price.CacheWrite},
		{"a cold start re-creates", SummarizeComponent, CacheColdStart, price.CacheWrite},
		{"another component's new content is a write", "extract", CacheHit, price.CacheWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Event{
				Model: "m1", TS: 1, TokensBefore: 10_000, TokensAfter: 9_000, SavedUnique: 1000,
				FreshInput: 10, CacheRead: 50_000, TokenAccounting: AccountingComplete,
				CacheMissReason: tc.reason,
				Components: []CompRow{{Component: tc.component, Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 1000}},
			}
			e.Price(price, true)
			want := 1000 * tc.want * e.BilledTokenFactor
			nearEq(t, "component saved_usd", e.Components[0].SavedUSD, want)
			// The request-level baseline must agree with the component figure, or the headline and
			// the Components tab part ways on the same turn.
			nearEq(t, "baseline delta", e.BaselineCostUSD-e.CostUSD, want)
		})
	}
}

// rePriceRow is the read-time twin of Event.Price: a row stored under the old rule reads like a
// row written under the new one.
func TestRePriceRowMatchesWhatPriceWouldHaveStored(t *testing.T) {
	price := handPrice["m1"]
	e := &Event{
		Model: "m1", FreshInput: 10, CacheRead: 49_000, CacheWrite: 7_400,
		CacheMissReason: CacheHit, TokenAccounting: AccountingComplete,
		Components: []CompRow{{Component: SummarizeComponent, SavedGross: 589_000, SavedUnique: 589_000}},
	}
	e.Price(price, true)
	r := compactRow{
		Model: "m1", MissReason: CacheHit, FreshInput: 10, CacheRead: 49_000, CacheWrite: 7_400,
		SavedGross: 589_000, SavedUnique: 589_000,
		// What the old rule stored: the whole removal at the write rate.
		SavedUSD: 589_000 * price.CacheWrite, StoredUSD: 589_000 * price.CacheWrite,
	}
	rePriceRow(&r, func(string) (modelinfo.Price, bool) { return price, true })
	nearEq(t, "re-priced", r.SavedUSD, e.Components[0].SavedUSD)
	if r.SavedUSD >= r.StoredUSD {
		t.Errorf("re-priced %v is not below the stored %v: a read is cheaper than a write", r.SavedUSD, r.StoredUSD)
	}

	// A pure replay (no unique part) keeps its stored figure, and so its historical rates.
	replay := compactRow{Model: "m1", MissReason: CacheHit, SavedGross: 1000, SavedUSD: 0.5, StoredUSD: 0.5}
	rePriceRow(&replay, func(string) (modelinfo.Price, bool) { return price, true })
	if replay.SavedUSD != 0.5 {
		t.Errorf("a replay row was re-priced: %v", replay.SavedUSD)
	}
}

type testWindows struct{}

func (testWindows) Window(context.Context, string) (int, bool) { return 1_000_000, true }
func (testWindows) WindowExact(context.Context, string) (int, bool, bool) {
	return 1_000_000, true, true
}

func summarizeEvent(ts int64, before int, session string, fresh, read int64, reason string,
	gross, unique int, stored float64, ev map[string]int) *Event {
	e := mkEvent(ts, session, "m1", before, before-1000)
	e.TenantID = "t1"
	e.FreshInput, e.CacheRead, e.CacheWrite = fresh, read, 0
	e.CacheMissReason = reason
	e.TokenAccounting = AccountingComplete
	e.Components = []CompRow{{
		Component: SummarizeComponent, Kind: "offload", Acted: true, Mutated: true,
		SavedGross: gross, SavedUnique: unique, SavedUSD: stored, Events: ev,
	}}
	return e
}

// The ledger through SQL: the first removal is re-priced, replay inside the span counts, and
// credit after the span closed is handed back.
func TestSummarizeLedgerCapsCreditAtTheSpanAndRePricesTheFirstRemoval(t *testing.T) {
	db := openTestDB(t)
	const stored1, storedReplay = 1000 * 1.25e-6, 1000 * 1e-7
	err := db.insertBatch([]*Event{
		// t0: a summary. The old rule stored the whole removal at the write rate.
		summarizeEvent(DayMs+1, 300_000, "s", 1_000, 50_000, CacheHit, 1000, 1000, stored1,
			map[string]int{offload.EventFreshSummary: 1}),
		summarizeEvent(DayMs+2, 310_000, "s", 10_000, 50_000, CacheHit, 1000, 0, storedReplay,
			map[string]int{offload.EventReusedCheckpoint: 1}),
		// 100k of new content: the span (ceiling 1.00 - fill 0.90 of a 1M window) closes here.
		summarizeEvent(DayMs+3, 320_000, "s", 100_000, 50_000, CacheHit, 1000, 0, storedReplay,
			map[string]int{offload.EventReusedCheckpoint: 1}),
		// After the span: not ours.
		summarizeEvent(DayMs+4, 330_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay,
			map[string]int{offload.EventReusedCheckpoint: 1}),
		summarizeEvent(DayMs+5, 340_000, "s", 10, 50_000, CacheHit, 1000, 0, storedReplay,
			map[string]int{offload.EventReusedCheckpoint: 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := db.SummarizeLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	bf, _ := tokens.BilledDeltaFactor("m1")
	nearEq(t, "first removal", l.FirstRemovalUSD, 1000*1e-7*bf)
	nearEq(t, "replay", l.ReplayUSD, 2*storedReplay)
	nearEq(t, "past span", l.PastSpanUSD, 2*storedReplay)
	nearEq(t, "stored", l.StoredUSD, stored1+4*storedReplay)
	nearEq(t, "delta", l.DeltaUSD(), 1000*1e-7*bf+2*storedReplay-stored1-4*storedReplay)
	if l.DeltaUSD() >= 0 {
		t.Errorf("delta = %v, want negative: the stored credit over-states this session", l.DeltaUSD())
	}

	// The headline moves by the same amount.
	o := &Overview{BaselineCostUSD: 10, NetSavedUSD: 4, TotalSavedUSD: 5, TotalReducedUSD: 5}
	o.ApplySummarizeLedger(l)
	nearEq(t, "baseline", o.BaselineCostUSD, 10+l.DeltaUSD())
	nearEq(t, "net saved", o.NetSavedUSD, 4+l.DeltaUSD())
	nearEq(t, "total saved", o.TotalSavedUSD, 5+l.DeltaUSD())

	// And so does the component row, with the verdict following.
	row := &ComponentRow{Component: SummarizeComponent, SavedUSD: l.StoredUSD, NetUSD: l.StoredUSD,
		NetUSDWithEstimate: l.StoredUSD, LLMCostUSD: 0.001}
	applySummarizeLedgerToRow(row, l)
	nearEq(t, "row saved", row.SavedUSD, l.CountedUSD())
	nearEq(t, "row first removal", row.SavedUSDFirstRemoval, l.FirstRemovalUSD)
	nearEq(t, "row replay", row.SavedUSDReplay, l.ReplayUSD)
}

// A model whose window is not exactly known contributes nothing: its credit stays as stored.
func TestSummarizeLedgerLeavesAGuessedWindowAsStored(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		summarizeEvent(DayMs+1, 300_000, "s", 1_000, 50_000, CacheHit, 1000, 1000, 0.00125,
			map[string]int{offload.EventFreshSummary: 1}),
	}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SummarizeLedgerFor(Filter{TenantAll: true}, guessedWindows{}, handPrice)
	if err != nil {
		t.Fatal(err)
	}
	if l.Rows != 0 || l.DeltaUSD() != 0 {
		t.Errorf("ledger = %+v, want empty", l)
	}
}

type guessedWindows struct{}

func (guessedWindows) Window(context.Context, string) (int, bool) { return 200_000, true }

// summarize records no extraction_calls rows, so its own model spend used to sit in
// requests.cg_llm_cost_usd with no owner and the Components tab showed 0 calls and $0.
func TestSummarizerOwnCostIsAttributedToSummarize(t *testing.T) {
	db := openTestDB(t)
	a := summarizeEvent(DayMs+1, 300_000, "s", 1_000, 50_000, CacheHit, 1000, 1000, 0.001,
		map[string]int{offload.EventFreshSummary: 1})
	a.CGLLMCostUSD = 0.40
	b := summarizeEvent(DayMs+2, 310_000, "s", 1_000, 50_000, CacheHit, 1000, 0, 0.0001, nil)
	b.CGLLMCostUSD = 0.05
	// A request whose model spend an extraction call owns is left to that component.
	c := summarizeEvent(DayMs+3, 320_000, "s", 1_000, 50_000, CacheHit, 1000, 0, 0.0001, nil)
	c.CGLLMCostUSD = 0.30
	c.Extractions = []ExtractionRow{{Component: "extract_llm", CostUSD: 0.30}}
	if err := db.insertBatch([]*Event{a, b, c}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	var got *ComponentRow
	for _, r := range rows {
		if r.Component == SummarizeComponent {
			got = r
		}
	}
	if got == nil {
		t.Fatal("no summarize row")
	}
	if got.LLMCalls != 2 {
		t.Errorf("llm_calls = %d, want 2", got.LLMCalls)
	}
	nearEq(t, "llm_cost_usd", got.LLMCostUSD, 0.45)
	nearEq(t, "net", got.NetUSD, got.SavedUSD-0.45)
}

// An empty ledger must leave the summarize row as the stored figures made it. The ledger is empty
// when the window is a guess, the trigger is above the client ceiling, or there is no resolver.
func TestAnEmptyLedgerLeavesTheSummarizeRowAlone(t *testing.T) {
	row := &ComponentRow{Component: SummarizeComponent, SavedUSD: 5, SavedUSDFirstRemoval: 1, SavedUSDReplay: 4,
		SavedUSDDecomposed: 5, NetUSDFirstRemoval: 0.9, LLMCostUSD: 0.1, NetUSD: 4.9}
	applySummarizeLedgerToRow(row, &SummarizeLedger{})
	if row.SavedUSD != 5 || row.SavedUSDFirstRemoval != 1 || row.SavedUSDReplay != 4 ||
		row.SavedUSDDecomposed != 5 || row.NetUSDFirstRemoval != 0.9 {
		t.Errorf("row changed under an empty ledger: %+v", row)
	}
}

// A partial ledger covers some conversations. The stored credit it did not see stays in the replay
// column, so the split still adds up to saved_usd.
func TestAPartialLedgerKeepsTheUncoveredCreditInTheSplit(t *testing.T) {
	l := &SummarizeLedger{FirstRemovalUSD: 1, ReplayUSD: 2, StoredUSD: 10, Rows: 3}
	row := &ComponentRow{Component: SummarizeComponent, SavedUSD: 15, NetUSD: 15, NetUSDWithEstimate: 15}
	applySummarizeLedgerToRow(row, l)
	nearEq(t, "saved", row.SavedUSD, 15+l.DeltaUSD())
	nearEq(t, "split adds up", row.SavedUSDFirstRemoval+row.SavedUSDReplay, row.SavedUSD)
	nearEq(t, "replay holds the uncovered 5", row.SavedUSDReplay, 2+5)
}
