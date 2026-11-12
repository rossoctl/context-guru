package dash

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// seedCredit builds an account that did BOTH things: an MCP server whose declarations the filter
// is stopping (filtered_decl_tokens on real requests), and a second MCP server it stopped
// declaring partway through the window with plenty of later MCP sessions to testify.
//
// Ten sessions, each five cache-hit requests. Sessions 0-3 declare both servers; 4-9 declare only
// the first. So `mcp__gone__x` was last seen in session 3 and six qualifying sessions ran without
// it — well past the three the analysis requires.
func seedCredit(t *testing.T, filteredTokens int) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	var msgs []invMsg
	for i := 0; i < 10; i++ {
		s := "s" + string(rune('a'+i))
		for k := 0; k < 5; k++ {
			e := mkEvent(day(i)+int64(k), s, "claude", 100, 90)
			e.TenantID, e.Tools = "t1", 4
			if i >= 4 {
				// Only the later sessions carry a filter saving, so the two halves are
				// distinguishable in the totals below.
				e.FilteredDeclTokens = filteredTokens
			}
			evs = append(evs, e)
		}
		ds := []Decl{{Kind: KindMCPTool, Name: "mcp__stay__x", Server: "stay", Tokens: 500}}
		if i < 4 {
			ds = append(ds, Decl{Kind: KindMCPTool, Name: "mcp__gone__x", Server: "gone", Tokens: 800})
		}
		msgs = append(msgs, invMsg{tenant: "t1", session: s, ts: day(i), inv: &Inventory{
			Digest: s, Decls: ds, UseFingerprint: uint64(i)}})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	return db
}

// The credit is the filter's measured saving and nothing else. What an account stops declaring on
// its own is not credited anywhere (#583, #584), even when the fixture holds such a removal.
func TestDeclCreditIsTheFiltersMeasuredSavingOnly(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	// Measured: 30 requests (sessions 4-9, five each) x 1,000 tokens.
	if c.FilterRequests != 30 || c.FilterReads != 30_000 {
		t.Errorf("filter half = %d requests / %d reads, want 30 / 30000",
			c.FilterRequests, c.FilterReads)
	}
	// 30,000 cache-read tokens at flatPrice's $0.1/MTok.
	if math.Abs(c.FilterUSD-0.003) > 1e-9 {
		t.Errorf("filter USD = %g, want 0.003", c.FilterUSD)
	}
	if !c.Priced {
		t.Error("priced = false on a fully priced fixture")
	}
}

// No pricer: the token counts stand and the dollars do not exist. Reported unpriced rather than
// as zero, which is this dashboard's rule everywhere — a $0.00 saving is a claim.
func TestDeclCreditWithoutRatesReportsTokensAndNoDollars(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Priced {
		t.Error("priced = true with no pricer")
	}
	if c.FilterReads != 30_000 {
		t.Errorf("filter reads = %d, want 30000 even unpriced", c.FilterReads)
	}
	if c.FilterUSD != 0 {
		t.Errorf("dollars were invented without rates: %g", c.FilterUSD)
	}
}

// An unpriced MODEL, which is different from no pricer: the figure is the priced subset and must
// be flagged, or a reader adds it to a total it is not the total of.
func TestDeclCreditFlagsAnUnpricedModel(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"},
		func(string) (modelinfo.Price, bool) { return modelinfo.Price{}, false })
	if err != nil {
		t.Fatal(err)
	}
	if c.Priced {
		t.Error("priced = true for a model with no rates")
	}
}

// TestWaterfallTotalMatchesTheHeadlineAfterAllPricedAdditions is the API-handler-level version
// of TestSetDeclCreditPutsEachHalfInTheRightTotal: it caught a real bug that the field-level
// test could not, because o.Waterfall is built INSIDE Overview(), before CachesplitHistoricalUSD
// and SetDeclCredit ever run — so the waterfall's own "total_saved" bar baked in a total short by
// both additions, silently disagreeing with the headline tile two inches above it (which reads
// o.TotalSavedUSD directly, not through the waterfall). Exercises the real GET /api/stats
// handler, not the field-level helpers, because that staleness is invisible to anything that
// calls Overview()/SetDeclCredit directly without going through the handler's full sequence.
func TestWaterfallTotalMatchesTheHeadlineAfterAllPricedAdditions(t *testing.T) {
	db := seedCredit(t, 1000)
	// A pre-instrumentation cachesplit-historical row, same shape as
	// TestHistoricalSplitValuationIsReadOnlyAndConservative: one row teaching the model's stable
	// half, one session-first row that qualifies for the historical credit.
	teach := mkEvent(9000, "s-hist", "aws/claude-sonnet-5", 100, 100)
	teach.TenantID, teach.CacheRead, teach.CacheWrite, teach.SplitStableTokens = "t1", 54_304, 1_000, 5_697
	qualifies := mkEvent(9100, "s-new-hist", "aws/claude-sonnet-5", 100, 100)
	qualifies.TenantID, qualifies.CacheRead, qualifies.CacheWrite = "t1", 54_304, 1_000
	if err := db.insertBatch([]*Event{teach, qualifies}); err != nil {
		t.Fatal(err)
	}
	// A Recorder built directly around the already-seeded db, not via NewRecorder: that
	// constructor starts background goroutines (the writer, the dedupe backfill) against
	// whatever DB it opens, and swapping rec.db out from under them afterward is a data race
	// (caught by -race: dedupeLoop reads r.db concurrently with this test's own write to it).
	// This test only needs a Recorder that answers DB() and Close() for NewAPI/t.Cleanup — no
	// live capture pipeline is exercised here.
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{ibmSonnet})
	mux := http.NewServeMux()
	api.Mount(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/stats = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		TotalSavedUSD float64 `json:"total_saved_usd"`
		DeclFilterUSD float64 `json:"decl_filter_usd"`
		Waterfall     []struct {
			Key      string  `json:"key"`
			DeltaUSD float64 `json:"delta_usd"`
		} `json:"waterfall"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, w.Body)
	}
	if resp.DeclFilterUSD <= 0 {
		t.Fatal("decl_filter_usd is not positive; the fixture stopped exercising it")
	}
	var waterfallTotal, waterfallDeclFilter float64
	var sawDeclFilter bool
	for _, s := range resp.Waterfall {
		if s.Key == "total_saved" {
			waterfallTotal = s.DeltaUSD
		}
		if s.Key == "decl_filter_saved" {
			waterfallDeclFilter, sawDeclFilter = -s.DeltaUSD, true
		}
	}
	if !sawDeclFilter {
		t.Fatal("no decl_filter_saved step in the waterfall")
	}
	if math.Abs(waterfallTotal-resp.TotalSavedUSD) > 1e-9 {
		t.Errorf("waterfall total_saved = %v, headline total_saved_usd = %v — the chart and the "+
			"tile disagree", waterfallTotal, resp.TotalSavedUSD)
	}
	if math.Abs(waterfallDeclFilter-resp.DeclFilterUSD) > 1e-9 {
		t.Errorf("waterfall's decl_filter_saved step = %v, headline decl_filter_usd = %v — the "+
			"waterfall step is stale", waterfallDeclFilter, resp.DeclFilterUSD)
	}
}

// The filter's saving reaches TotalSavedUSD, which is where our savings live. This is asserted on
// the Overview rather than on the credit, because the totals are where a wrong answer becomes a
// headline.
func TestSetDeclCreditAddsTheFilterSavingToTheTotal(t *testing.T) {
	db := seedCredit(t, 1000)
	o, err := db.Overview(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	base := o.TotalSavedUSD
	o.SetDeclCredit(&DeclCredit{FilterUSD: 0.25, Priced: true})
	if got := o.TotalSavedUSD - base; math.Abs(got-0.25) > 1e-9 {
		t.Errorf("total_saved moved by %g, want exactly the filter's $0.25", got)
	}
	// And a nil credit changes nothing, so a deployment where the query fails reads a total that
	// is short rather than one that is wrong.
	saved := o.TotalSavedUSD
	o.SetDeclCredit(nil)
	if o.TotalSavedUSD != saved {
		t.Error("a nil credit moved the total")
	}
}

// TestTheDeclarationFilterSavingIsDisjointFromCompactions pins the DOWNSTREAM ARITHMETIC, and the
// title used to over-claim. It does not exercise apply and it cannot: it builds the row shape by
// hand, so it verifies "given a request the filter acted on and compaction did not, no total counts
// it twice" — which is a real and separate property of Overview and SetDeclCredit.
//
// What it does NOT verify is that such a row is what apply actually produces. That is an ordering
// property of another package — the filter must run before the pipeline takes its baseline
// (components/pipeline.go:35 over the normalized request built at apply/apply.go:441) — and it is
// guarded by TestFilterRemovalIsNotInsideTheCompactionBaseline in the apply package, which compares
// two real runs instead of asserting a fixture.
//
// The distinction is not pedantry. A reviewer showed that the only mutation that reddened THIS test
// was deleting its own `e.BaselineCostUSD = e.CostUSD` fixture line, not changing any production
// code — so on its own it was evidence about the fixture. The two tests together cover the claim;
// either alone does not.
func TestTheDeclarationFilterSavingIsDisjointFromCompactions(t *testing.T) {
	db := openTestDB(t)
	// The shape a filtered request really has, taken from a live run: tokens_before ==
	// tokens_after (the pipeline saw an already-filtered body and removed nothing further), and a
	// non-zero filtered_decl_tokens beside it.
	e := mkEvent(1000, "s", "claude", 4910, 4910)
	e.TenantID, e.Tools, e.FilteredDeclTokens = "t1", 4, 561
	e.CacheRead, e.CacheWrite = 49132, 0
	// mkEvent hardcodes BaselineCostUSD: 0.02 against CostUSD: 0.01 whatever tokens it is given,
	// so its default row claims a cent of compaction saving on a request that compacted nothing.
	// Equal is what a filtered-but-not-compacted request ACTUALLY looks like — verified on the
	// live run, where baseline_cost_usd − cost_usd was 0.0 on all six such rows. Without this the
	// test fails on the fixture rather than on the mechanism, which is the wrong red.
	e.BaselineCostUSD = e.CostUSD
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	// Compaction saw nothing, so every compaction-side figure is zero and the baseline equals the
	// bill. If a future reordering put the filter's removal inside the baseline, this is where it
	// would show up — as a saving compaction did not make.
	if o.SavedGross != 0 || o.SavedUnique != 0 {
		t.Errorf("compaction reports saved_gross=%d saved_unique=%d on a request it did not "+
			"touch: the declaration filter's removal has entered the compaction accounting, and "+
			"TotalSavedUSD now counts it twice", o.SavedGross, o.SavedUnique)
	}
	if o.BaselineCostUSD != o.CostUSD {
		t.Errorf("baseline $%g != cost $%g on a request compaction did not touch: the filter's "+
			"removal is inside the baseline, so it is in NetSavedUSD AND in DeclFilterUSD",
			o.BaselineCostUSD, o.CostUSD)
	}
	if o.NetSavedUSD != 0 {
		t.Errorf("net_saved_usd = $%g before any credit is attached, so the filter's saving is "+
			"already counted once here and will be counted again by SetDeclCredit", o.NetSavedUSD)
	}
	// And the filter half does carry it, so this is a test of disjointness rather than of an
	// empty fixture.
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if c.FilterReads != 561 {
		t.Fatalf("filter half = %d reads, want 561 — the fixture is not exercising anything",
			c.FilterReads)
	}
	o.SetDeclCredit(c)
	if o.TotalSavedUSD != c.FilterUSD {
		t.Errorf("total_saved $%g != the filter's $%g; something else contributed to a request "+
			"that only the filter acted on", o.TotalSavedUSD, c.FilterUSD)
	}
}

// TestTheWalkAndTheTotalAgreeOnHowManyAddendsThereAre.
//
// TotalSavedUSD's doc comment enumerates its addends in prose and the waterfall's `total_saved`
// step enumerates them again in prose shown to the reader. Adding a fourth means updating both,
// and the second one was missed — the page said "Three disjoint token sets" while the field summed
// four. Prose in two places drifts; this is the cheapest thing that notices.
func TestTheWalkAndTheTotalAgreeOnHowManyAddendsThereAre(t *testing.T) {
	var desc string
	for _, s := range (&Overview{}).waterfall() {
		if s.Key == "total_saved" {
			desc = s.Description
		}
	}
	if desc == "" {
		t.Fatal("the total_saved step is gone; this check needs rewriting")
	}
	// The four steps the total is built from, each of which must be described where the total is.
	for _, want := range []string{"compaction", "prefix-cache", "keep-alive", "declarations"} {
		if !strings.Contains(strings.ToLower(desc), want) {
			t.Errorf("the total_saved description does not mention %q:\n%s\n\n"+
				"Every addend of TotalSavedUSD is named here, or the page enumerates fewer "+
				"things than it sums.", want, desc)
		}
	}
	if strings.Contains(desc, "Three disjoint") {
		t.Errorf("the description still says 'Three disjoint token sets' while TotalSavedUSD " +
			"sums four")
	}
}

// TestOverlapMatchesTheConfigVocabularyNotTheReports: SelfRemovals marks a row that the server-side
// filter list already covers, so the savings-since-removal estimate (adaptToolsSaved) leaves it to the
// filter. The comparison used to be a REPORT name against a CONFIG list and could never fire for two
// of the three shapes.
//
// The config stores `skill__dataviz` and `mcp__plan`; a report row is named `dataviz` with kind
// skill, or `mcp__plan__make_plan` with server `plan`. An exact tool name is the ONE shape where the
// two vocabularies coincide.
func TestOverlapMatchesTheConfigVocabularyNotTheReports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		removed []string
		want    bool // expected Overlap on the one removed row
	}{
		// A whole MCP SERVER, which is how the dashboard's own group switch writes it. `gone` is
		// the server whose tools stopped being declared, so this must be recognised as overlap.
		{"whole mcp server, config form", []string{"mcp__gone"}, true},
		// The exact tool name — the one shape that worked before.
		{"exact mcp tool name", []string{"mcp__gone__x"}, true},
		// A name in the REPORT's vocabulary is not what the config holds and must NOT match, or
		// the exclusion starts firing on things the filter is not credited for.
		{"bare server name is not a config entry", []string{"gone"}, false},
		{"an unrelated entry", []string{"mcp__other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedCredit(t, 1000)
			rows, err := db.SelfRemovals(Filter{Tenant: "t1"}, flatPrice, tc.removed)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d removed rows, want 1: %+v", len(rows), rows)
			}
			if rows[0].Overlap != tc.want {
				t.Errorf("removed=%v gave overlap=%v, want %v\n"+
					"The config and the report name things differently; the comparison has to be "+
					"made in one vocabulary.", tc.removed, rows[0].Overlap, tc.want)
			}
		})
	}
}

// The SKILL half of the same bug, in its own test because a skill is the class this change newly
// made filterable and the one the reviewer reproduced: `skill__dataviz` on the list against a row
// named `dataviz`.
func TestOverlapFiresForASkillInTheConfigForm(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	var msgs []invMsg
	for i := 0; i < 10; i++ {
		s := "s" + string(rune('a'+i))
		for k := 0; k < 5; k++ {
			e := mkEvent(day(i)+int64(k), s, "claude", 100, 90)
			e.TenantID, e.Tools = "t1", 4
			evs = append(evs, e)
		}
		// Every session carries a skills LISTING (so a later session can testify about a missing
		// skill), and only the first four carry the skill itself.
		ds := []Decl{{Kind: KindSkillListing, Server: SkillsOK, Tokens: 900}}
		if i < 4 {
			ds = append(ds, Decl{Kind: KindSkill, Name: "dataviz", Tokens: 300})
		}
		msgs = append(msgs, invMsg{tenant: "t1", session: s, ts: day(i), inv: &Inventory{
			Digest: s, Decls: ds, UseFingerprint: uint64(i)}})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	// With nothing on the list the skill is a removal of the account's own.
	base, err := db.SelfRemovals(Filter{Tenant: "t1"}, flatPrice, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 1 || base[0].Overlap {
		t.Fatalf("the skill was not detected as a plain removal (rows=%+v); the fixture is "+
			"not exercising the overlap path", base)
	}
	// With the CONFIG form on the list it is overlap.
	got, err := db.SelfRemovals(Filter{Tenant: "t1"}, flatPrice, []string{"skill__dataviz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Overlap {
		t.Errorf("skill__dataviz on the list gave %+v, want one row marked overlap.\n"+
			"This is the shape the dashboard actually writes, and the exclusion has to see it.", got)
	}
	// The bare name is the REPORT's vocabulary and is not what a config holds.
	bare, err := db.SelfRemovals(Filter{Tenant: "t1"}, flatPrice, []string{"dataviz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bare) != 1 || bare[0].Overlap {
		t.Errorf("a bare `dataviz` matched (rows=%+v); the config stores skill__dataviz and "+
			"matching the bare form would exclude rows the filter is not credited for", bare)
	}
}

// The accountant library prices each toolfilter removal, so on a request it priced the removal is already
// in baseline_cost_usd and in the net saving. The measured credit must not add it a second time
// (context-guru#768 B2: one 1000-token removal gave a row of $0.0001 and a total of $0.0002). A request it
// could not price, and one written before the switch, have nothing in the baseline: they keep the credit.
func TestDeclCreditSkipsRequestsTheAccountantPriced(t *testing.T) {
	db := openTestDB(t)
	mk := func(i int, pricedBy string) *Event {
		e := mkEvent(day(0)+int64(i), "s1", "claude", 100, 90)
		e.TenantID, e.Tools, e.FilteredDeclTokens = "t1", 4, 1000
		e.Components = append(e.Components, CompRow{Component: "toolfilter", Kind: KindEnvelope, Acted: true,
			Mutated: true, PricedBy: pricedBy})
		return e
	}
	// 1: the library priced it. 2: it could not (no saving booked). 3: a row from before the switch.
	if err := db.insertBatch([]*Event{mk(1, PricedByAccountant), mk(2, PricedByNone), mk(3, "")}); err != nil {
		t.Fatal(err)
	}
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if c.FilterRequests != 3 || c.FilterReads != 3000 {
		t.Fatalf("filter half = %d requests / %d reads, want 3 / 3000 (the priced request keeps its facts and adds no dollars)",
			c.FilterRequests, c.FilterReads)
	}
	// 2,000 cache-read tokens at flatPrice's $0.1/MTok.
	if math.Abs(c.FilterUSD-0.0002) > 1e-9 {
		t.Errorf("filter USD = %g, want 0.0002", c.FilterUSD)
	}
}

// The accountant's pricing drops only the DOLLARS of a request's filter credit. The requests, the tokens
// the filter did not send and the first date are facts, so they stay (context-guru#911). The items of
// /api/adapt/tools must then agree with the total: both add no dollars for the same rows.
func TestFilterCreditKeepsFactsAndSkipsDollarsForAccountantPricedRows(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	for i := 0; i < 10; i++ {
		e := mkEvent(day(0)+int64(i), "s1", "claude", 100, 90)
		e.TenantID, e.Tools, e.FilteredDeclTokens, e.DeclDigest = "t1", 2, 5000, "d1"
		e.Components = append(e.Components, CompRow{Component: "toolfilter", Kind: KindEnvelope, Acted: true,
			Mutated: true, PricedBy: PricedByAccountant})
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "t1", session: "s1", ts: day(0), inv: &Inventory{Digest: "d1",
		UseFingerprint: 1, Decls: []Decl{{Kind: KindMCPTool, Name: "mcp__srv__q", Server: "srv", Tokens: 5000}}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeclFilterSavings(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 10 || got.Reads != 50000 || got.Since != day(0) {
		t.Errorf("facts = %d requests / %d reads / since %d, want 10 / 50000 / %d", got.Requests, got.Reads, got.Since, day(0))
	}
	if got.USD != 0 {
		t.Errorf("USD = %g, want 0: the accountant booked these tokens", got.USD)
	}
	doc, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Realized == nil || doc.Realized.Requests != 10 {
		t.Errorf("Realized = %+v, want the measured block with 10 requests", doc.Realized)
	}
	saved, err := db.adaptToolsSaved(Filter{Tenant: "t1"}, flatPrice, []string{"mcp__srv"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Filter.Requests != 10 || saved.Filter.USD != 0 {
		t.Errorf("saved.filter = %d requests / $%g, want 10 / 0", saved.Filter.Requests, saved.Filter.USD)
	}
	sum := 0.0
	for _, it := range saved.Filter.Items {
		sum += it.USD
	}
	if sum > saved.Filter.USD+1e-12 {
		t.Errorf("items sum to $%g, more than the total $%g: %+v", sum, saved.Filter.USD, saved.Filter.Items)
	}
}

// Review 927 round 1, finding 1. A request whose figure was held out (estimate_held = 1, #915) books
// nothing in the baseline, filter part included. The measured credit owns that request's filter dollars:
// it adds them, and the totals do not count the request. A request the accountant booked still adds none.
func TestFilterCreditKeepsDollarsForAHeldEstimatedRequest(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	for i := 0; i < 10; i++ {
		e := mkEvent(day(0)+int64(i), "s1", "claude", 100, 90)
		e.TenantID, e.Tools, e.FilteredDeclTokens, e.DeclDigest = "t1", 2, 5000, "d1"
		e.Components = append(e.Components, CompRow{Component: "toolfilter", Kind: KindEnvelope, Acted: true,
			Mutated: true, PricedBy: PricedByAccountant})
		e.BaselineCostUSD = e.CostUSD
		if i < 4 { // the first four are estimated requests whose figure was held out
			e.PriceEstimated, e.EstimateHeld, e.EstimatedSavedUSD = true, true, 0.05
			e.BaselineCostUSD = e.CostUSD
		}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "t1", session: "s1", ts: day(0), inv: &Inventory{Digest: "d1",
		UseFingerprint: 1, Decls: []Decl{{Kind: KindMCPTool, Name: "mcp__srv__q", Server: "srv", Tokens: 5000}}}}}); err != nil {
		t.Fatal(err)
	}
	one, ok := flatPrice("claude")
	if !ok {
		t.Fatal("flatPrice has no price")
	}
	want := 4 * 5000 * one.CacheRead // mkEvent rows read the cache
	got, err := db.DeclFilterSavings(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 10 || got.Reads != 50000 {
		t.Errorf("facts = %d requests / %d reads, want 10 / 50000", got.Requests, got.Reads)
	}
	if !accNear(got.USD, want) || want == 0 {
		t.Errorf("USD = %g, want %g: only the 4 held requests add dollars", got.USD, want)
	}
	doc, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Realized == nil || doc.Realized.Requests != 10 || !accNear(doc.Realized.USD, want) {
		t.Errorf("Realized = %+v, want 10 requests and $%g", doc.Realized, want)
	}
	saved, err := db.adaptToolsSaved(Filter{Tenant: "t1"}, flatPrice, []string{"mcp__srv"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Filter.Requests != 10 || !accNear(saved.Filter.USD, want) {
		t.Errorf("saved.filter = %d requests / $%g, want 10 / $%g", saved.Filter.Requests, saved.Filter.USD, want)
	}
	// The totals hold the request out: no baseline delta on any of these rows.
	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := o.BaselineCostUSD - o.CostUSD; !accNear(d, 0) || o.EstimatedRequests != 4 {
		t.Errorf("baseline delta %g, held requests %d, want 0 and 4", d, o.EstimatedRequests)
	}
}

// TestSelfRemovalsWithoutRatesLeavesTheRowUnpriced. A deployment with no pricer passes a nil price
// function (API.priceFn). The savings-since-removal estimate (adaptToolsSaved) reaches
// SelfRemovals with that nil, and the dollars are then unknown, not zero and not a panic.
func TestSelfRemovalsWithoutRatesLeavesTheRowUnpriced(t *testing.T) {
	db := seedCredit(t, 1000)
	rows, err := db.SelfRemovals(Filter{Tenant: "t1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d removed rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Priced || rows[0].AvoidedUSD != 0 {
		t.Errorf("priced=%v avoidedUSD=%g without rates, want unpriced and 0",
			rows[0].Priced, rows[0].AvoidedUSD)
	}
	if rows[0].AvoidedReads == 0 {
		t.Error("the token count is known without rates and must still stand")
	}
}
