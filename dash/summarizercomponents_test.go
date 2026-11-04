package dash

import (
	"regexp"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// The medium, high and xhigh presets run cache_aware_summarizer, and its rows carry that name. Every
// fix in this change has to hold for every name on SummarizerComponents, not only "summarize".

func TestSummarizerComponentListIsOneSafeList(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z_]+$`)
	seen := map[string]bool{}
	for _, n := range SummarizerComponents {
		if !safe.MatchString(n) {
			t.Errorf("%q is spliced into SQL and must match %v", n, safe)
		}
		if seen[n] {
			t.Errorf("%q is listed twice", n)
		}
		seen[n] = true
		if !IsSummarizer(n) {
			t.Errorf("IsSummarizer(%q) = false for a listed name", n)
		}
	}
	for _, want := range []string{"summarize", "cache_aware_summarizer"} {
		if !seen[want] {
			t.Errorf("%q is missing from SummarizerComponents", want)
		}
	}
	if IsSummarizer("extract") || IsSummarizer("") {
		t.Error("a non-summarizer was accepted")
	}
	if compactEpisodeSelectFor([]string{"x'; DROP TABLE requests; --"}) != "" {
		t.Error("an unlisted component name reached the SQL")
	}
}

// namedSummarizers is spelled out here on purpose: a test that looped over SummarizerComponents
// would shrink with it and still pass when a name is dropped from the list.
var namedSummarizers = []string{"summarize", "cache_aware_summarizer"}

func TestEveryNamedSummarizerPricesItsFirstRemovalAsARead(t *testing.T) {
	price := handPrice["m1"]
	for _, name := range namedSummarizers {
		for _, tc := range []struct {
			reason string
			want   float64
		}{{CacheHit, price.CacheRead}, {CachePrefixChange, price.CacheRead}, {CacheTTLExpiry, price.CacheWrite}} {
			e := &Event{
				Model: "m1", FreshInput: 10, CacheRead: 50_000, TokenAccounting: AccountingComplete,
				CacheMissReason: tc.reason, SavedUnique: 1000, TokensBefore: 10_000, TokensAfter: 9_000,
				Components: []CompRow{{Component: name, SavedGross: 1000, SavedUnique: 1000}},
			}
			e.Price(price, true)
			want := 1000 * tc.want * e.BilledTokenFactor
			nearEq(t, name+"/"+tc.reason+" component", e.Components[0].SavedUSD, want)
			nearEq(t, name+"/"+tc.reason+" baseline", e.BaselineCostUSD-e.CostUSD, want)
		}
	}
}

func asComponent(e *Event, name string) *Event {
	e.Components[0].Component = name
	return e
}

func TestEveryNamedSummarizerGoesThroughTheLedgerAndCostAttribution(t *testing.T) {
	bf, _ := tokens.BilledDeltaFactor("m1")
	for _, name := range namedSummarizers {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			const stored1, storedReplay = 1000 * 1.25e-6, 1000 * 1e-7
			reused := map[string]int{offload.EventReusedCheckpoint: 1}
			mk := func(ts int64, before int, fresh int64, gross, unique int, stored float64, ev map[string]int) *Event {
				return asComponent(summarizeEvent(ts, before, "s", fresh, 50_000, CacheHit, gross, unique, stored, ev), name)
			}
			a := mk(DayMs+1, 300_000, 1_000, 1000, 1000, stored1, map[string]int{offload.EventFreshSummary: 1})
			a.CGLLMCostUSD = 0.40
			if err := db.insertBatch([]*Event{
				a,
				mk(DayMs+2, 310_000, 10_000, 1000, 0, storedReplay, reused),
				mk(DayMs+3, 320_000, 100_000, 1000, 0, storedReplay, reused), // closes the span
				mk(DayMs+4, 330_000, 10, 1000, 0, storedReplay, reused),      // past it
			}); err != nil {
				t.Fatal(err)
			}
			ls, err := db.SummarizeLedgersFor(Filter{TenantAll: true}, testWindows{}, handPrice)
			if err != nil {
				t.Fatal(err)
			}
			l := ls[name]
			if l == nil {
				t.Fatalf("no ledger for %s: it is not on SummarizerComponents", name)
			}
			nearEq(t, "first removal", l.FirstRemovalUSD, 1000*1e-7*bf)
			nearEq(t, "replay", l.ReplayUSD, 2*storedReplay)
			nearEq(t, "past span", l.PastSpanUSD, storedReplay)
			for _, other := range namedSummarizers {
				if other != name && ls[other].Rows != 0 {
					t.Errorf("the %s rows leaked into the %s ledger", name, other)
				}
			}
			// The headline sees the same rows.
			sum, err := db.SummarizeLedgerFor(Filter{TenantAll: true}, testWindows{}, handPrice)
			if err != nil {
				t.Fatal(err)
			}
			nearEq(t, "headline delta", sum.DeltaUSD(), l.DeltaUSD())
			if sum.DeltaUSD() >= 0 {
				t.Errorf("delta = %v, want negative", sum.DeltaUSD())
			}

			rows, err := db.Components(Filter{TenantAll: true})
			if err != nil {
				t.Fatal(err)
			}
			var got *ComponentRow
			for _, r := range rows {
				if r.Component == name {
					got = r
				}
			}
			if got == nil {
				t.Fatalf("no %s row", name)
			}
			nearEq(t, "llm_cost_usd", got.LLMCostUSD, 0.40)
			if got.LLMCalls != 1 {
				t.Errorf("llm_calls = %d, want 1", got.LLMCalls)
			}
			applySummarizeLedgerToRow(got, ls[got.Component])
			nearEq(t, "row saved", got.SavedUSD, l.CountedUSD())

			// The episode view lists the same component's summary.
			ep, err := db.CompactionEpisodesFor(Filter{TenantAll: true}, testWindows{}, handPrice, KVCacheSimConfig{}, 0, 0.9, 1.0)
			if err != nil {
				t.Fatal(err)
			}
			nearEq(t, "episode view counted", ep.Ledger.CountedUSD(), l.CountedUSD())
		})
	}
}

// The client-compaction detector compares a request with the previous one on ITS thread. A
// subagent's small request on the same model is not the client compacting the main transcript.
func TestASubagentRequestIsNotAClientCompaction(t *testing.T) {
	sub := func(r *compactRow) { r.Thread = "a:sub"; r.TokensBefore = 5_000 }
	out := walk([]compactRow{
		row(1, 1_000, fresh),
		row(2, 10_000, saved(100_000)),
		row(3, 1_000, sub), // small, interleaved, other thread
		row(4, testSpan, saved(100_000)),
	}, exactWindow, testPrice)
	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Errorf("state = %q, want closed: a subagent's request ended the span", e.State)
	}
	if want := 200_000 * readRate; offUSD(out.Ledger.CountedUSD(), want) {
		t.Errorf("counted = %v, want %v", out.Ledger.CountedUSD(), want)
	}
	// A drop on the SAME thread still ends it.
	drop := func(r *compactRow) { r.TokensBefore = 5_000 }
	out = walk([]compactRow{
		row(1, 1_000, fresh),
		row(2, 10_000, saved(100_000)),
		row(3, 1_000, drop),
		row(4, testSpan, saved(100_000)),
	}, exactWindow, testPrice)
	if e := only(t, out); e.State != EpisodeClientCompacted {
		t.Errorf("state = %q, want %q for a drop on the main thread", e.State, EpisodeClientCompacted)
	}
}
