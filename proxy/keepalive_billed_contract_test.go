package proxy

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/store"
)

// The "_billed" marker is a string contract between two packages (#458 review): offload writes a
// committed keep-alive summary call's detail row with trigger "keepalive_substitute_billed", and
// dash's component-cost query matches the strategy "*_billed" to keep that row out of the
// gateway cost split. Neither side imports the other, so a rename on one side would bring the
// double count back with no compile error. This test drives the real offload side, takes the
// trigger it actually wrote, and feeds a row with that strategy to the real dash query.
func TestBilledElsewhereMarkerReachesTheDashboardCostQuery(t *testing.T) {
	// Offload side: a committed keep-alive summary call.
	k, _, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &usageSummaryModel{}
	kaCandidate(t, "sess-1", st, model)
	recordOne(t, k, kaPolicy(), kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if n := k.sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 {
		t.Fatal("no committed summary call: the offload side never ran")
	}
	_, calls := readDeferred(t, st, "sess-1")
	if len(calls) != 1 {
		t.Fatalf("want one deferred detail row, got %v", calls)
	}
	path, _ := calls[0]["path"].(string)
	trigger, _ := calls[0]["trigger"].(string)
	// The same Strategy takeDeferredUsage builds for the extraction_calls row.
	strategy := path + "/" + trigger

	// Dash side: the real component-cost query, over one turn whose gateway cost belongs to
	// extract_llm only, and one earlier gateway-priced summarizer call.
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	ev := func(ts int64, cg float64, ex ...dash.ExtractionRow) *dash.Event {
		comps := []dash.CompRow{{Component: "cache_aware_summarizer", Acted: true}}
		for _, x := range ex {
			if x.Component == "extract_llm" {
				comps = append(comps, dash.CompRow{Component: "extract_llm", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 1000})
			}
		}
		return &dash.Event{TS: ts, SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 9000, FreshInput: 10, CacheRead: 50000,
			TokenAccounting: dash.AccountingComplete, CGLLMCostUSD: cg,
			Components: comps, Extractions: ex}
	}
	rec.Record(ev(1000, 0.02, dash.ExtractionRow{Component: "cache_aware_summarizer", Model: "m1",
		CostUSD: 0.02, Strategy: "prefix_ask/turn"}))
	rec.Record(ev(2000, 0.05,
		dash.ExtractionRow{Component: "extract_llm", Model: "m1", CostUSD: 0.05},
		dash.ExtractionRow{Component: "cache_aware_summarizer", Model: "m1", CostUSD: 0.0161, Strategy: strategy}))

	var rows []*dash.ComponentRow
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, err = rec.DB().Components(dash.Filter{TenantAll: true}); err == nil && len(rows) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := map[string]*dash.ComponentRow{}
	for _, r := range rows {
		got[r.Component] = r
	}
	el, ca := got["extract_llm"], got["cache_aware_summarizer"]
	if el == nil || ca == nil {
		t.Fatalf("component rows never appeared: %v", got)
	}
	if math.Abs(el.LLMCostGatewayUSD-0.05) > 1e-9 {
		t.Errorf("extract_llm gateway cost = %v, want 0.05: the dash query did not recognise the "+
			"strategy offload wrote (%q) as billed elsewhere", el.LLMCostGatewayUSD, strategy)
	}
	if math.Abs(ca.LLMCostUSD-0.0361) > 1e-9 {
		t.Errorf("cache_aware_summarizer verdict cost = %v, want 0.0361 (gateway 0.02 + the "+
			"ping-billed call 0.0161); strategy %q", ca.LLMCostUSD, strategy)
	}
}
