package dash

import (
	"math"
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

// The two halves are computed and reported apart. Adding them would be the whole mistake this
// type exists to prevent: one is a component of ours rewriting a body, the other is the account
// changing its own configuration, and only the first is a saving this product may claim.
func TestDeclCreditKeepsTheMeasuredAndModelledHalvesApart(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice, nil)
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
	// Modelled: mcp__gone__x, 800 tokens x the 30 requests of the six later sessions.
	if c.SelfItems != 1 || c.SelfReads != 24_000 {
		t.Errorf("self half = %d items / %d reads, want 1 / 24000", c.SelfItems, c.SelfReads)
	}
	if math.Abs(c.SelfUSD-0.0024) > 1e-9 {
		t.Errorf("self USD = %g, want 0.0024", c.SelfUSD)
	}
	if !c.Priced {
		t.Error("priced = false on a fully priced fixture")
	}
	// The one thing that must NOT exist: a field holding the two added together.
	if c.FilterUSD == c.SelfUSD {
		t.Error("the fixture cannot tell the halves apart; it is not testing the split")
	}
}

// A name on the account's own removal list is already counted in the MEASURED half, so the
// modelled half must drop it — and say that it did, rather than the row silently vanishing.
func TestDeclCreditDropsWhatTheFilterIsAlreadyCreditedFor(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice,
		map[string]bool{"mcp__gone__x": true})
	if err != nil {
		t.Fatal(err)
	}
	if c.SelfItems != 0 || c.SelfReads != 0 || c.SelfUSD != 0 {
		t.Errorf("self half = %d items / %d reads / $%g, want nothing: the filter is already "+
			"credited for those tokens", c.SelfItems, c.SelfReads, c.SelfUSD)
	}
	if c.SelfOverlap != 1 {
		t.Errorf("overlap = %d, want 1 — a dropped row must be counted, not silently absent",
			c.SelfOverlap)
	}
	// The measured half is untouched by the overlap accounting.
	if c.FilterReads != 30_000 {
		t.Errorf("filter reads = %d, want 30000", c.FilterReads)
	}
}

// No pricer: the token counts stand and the dollars do not exist. Reported unpriced rather than
// as zero, which is this dashboard's rule everywhere — a $0.00 saving is a claim.
func TestDeclCreditWithoutRatesReportsTokensAndNoDollars(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Priced {
		t.Error("priced = true with no pricer")
	}
	if c.FilterReads != 30_000 {
		t.Errorf("filter reads = %d, want 30000 even unpriced", c.FilterReads)
	}
	if c.FilterUSD != 0 || c.SelfUSD != 0 {
		t.Errorf("dollars were invented without rates: %g / %g", c.FilterUSD, c.SelfUSD)
	}
}

// An unpriced MODEL, which is different from no pricer: the figure is the priced subset and must
// be flagged, or a reader adds it to a total it is not the total of.
func TestDeclCreditFlagsAnUnpricedModel(t *testing.T) {
	db := seedCredit(t, 1000)
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"},
		func(string) (modelinfo.Price, bool) { return modelinfo.Price{}, false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Priced {
		t.Error("priced = true for a model with no rates")
	}
}

// The filter half reaches TotalSavedUSD and the self half does not; both reach TotalReducedUSD.
// This is the field-level statement of the whole argument, and it is asserted on the Overview
// rather than on the credit, because the totals are where a wrong answer becomes a headline.
func TestSetDeclCreditPutsEachHalfInTheRightTotal(t *testing.T) {
	db := seedCredit(t, 1000)
	o, err := db.Overview(Filter{Tenant: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	base, baseReduced := o.TotalSavedUSD, o.TotalReducedUSD
	if base != baseReduced {
		t.Fatalf("the two totals differ before any credit is attached: %g vs %g", base, baseReduced)
	}
	o.SetDeclCredit(&DeclCredit{FilterUSD: 0.25, SelfUSD: 0.75, Priced: true})
	if got := o.TotalSavedUSD - base; math.Abs(got-0.25) > 1e-9 {
		t.Errorf("total_saved moved by %g, want exactly the filter's $0.25 — a self-removal is "+
			"the account's own reduction and this total claims ours", got)
	}
	if got := o.TotalReducedUSD - baseReduced; math.Abs(got-1.00) > 1e-9 {
		t.Errorf("total_reduced moved by %g, want $1.00 (both halves)", got)
	}
	// And a nil credit changes nothing, so a deployment where the query fails reads a total that
	// is short rather than one that is wrong.
	saved, reduced := o.TotalSavedUSD, o.TotalReducedUSD
	o.SetDeclCredit(nil)
	if o.TotalSavedUSD != saved || o.TotalReducedUSD != reduced {
		t.Error("a nil credit moved a total")
	}
}

// TestTheDeclarationFilterSavingIsDisjointFromCompactions makes the word "disjoint" executable.
//
// TotalSavedUSD adds the filter's saving to compaction's and calls the token sets disjoint. For
// the tools half that is true by construction — a tool schema is not in `messages` and
// `tokens_before` counts nothing else. For the SKILLS half it is not: a skill's listing entry IS
// in `messages`, so the two could describe the same tokens and the total would double-count them.
//
// It holds because both halves of the filter run in apply BEFORE the pipeline takes its baseline,
// so `tokens_before` is measured on the already-filtered body and the removal is simply absent
// from Saved() rather than inside it. That is an ORDERING property of a different package, which
// is exactly the kind of thing a later refactor falsifies silently — the total would keep adding
// and nothing would complain.
//
// Asserted as the invariant rather than by re-deriving the order: a request that carries a filter
// saving and no compaction contributes to the filter half and to NOTHING else.
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
	c, err := db.DeclCreditFor(Filter{Tenant: "t1"}, flatPrice, nil)
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
