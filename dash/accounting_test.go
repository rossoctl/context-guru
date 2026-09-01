package dash

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// A price table with round numbers, so every expectation below can be checked by hand.
// Per token: fresh 1e-6, cache read 1e-7 (a tenth), cache write 1.25e-6 (12.5x a read).
var handPrice = staticTable{
	"m1": {Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6, Output: 5e-6},
	// A second model at 2x, because the per-model split is the point of ModelRate and a
	// single-model fixture cannot catch a blended rate.
	"m2": {Input: 2e-6, CacheRead: 2e-7, CacheWrite: 2.5e-6, Output: 1e-5},
}

func nearEq(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.10f, want %.10f", label, got, want)
	}
}

// insertReq writes one request row plus one component row, with the tiers and savings the
// caller wants. It goes through Store so the test exercises the real insert.
func insertReq(t *testing.T, db *DB, e *Event) int64 {
	t.Helper()
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// TestReplayDecompositionIsHandCheckable is the accumulation test the savings accounting rests
// on: a fixture session whose correct answer is computable on paper.
//
// THE SESSION. Three turns on model m1, all warm (cache_read > 0, so every replay is priced at
// the cache-READ rate). One component, `extract`:
//
//	turn 1: removes 1,000 tokens of content it has never seen  -> gross 1000, unique 1000
//	turn 2: the agent re-sends everything, so the SAME 1,000 are removed again, plus 500 of
//	        genuinely new content                              -> gross 1500, unique  500
//	turn 3: all 1,500 are re-sent and re-removed, nothing new  -> gross 1500, unique    0
//
// Totals: gross 4,000, unique 1,500, replay 2,500.
//
// Every turn here READ cache and wrote none, so Event.uniqueRate is the FRESH rate: content
// entering these prompts for the first time would have been billed as input, not as cache
// creation, because nothing on these turns created cache.
//
// FIRST-REMOVAL value  = 1,500 x input      = 1,500 x 1.0e-6  = $0.001500
// REPLAY value         = 2,500 x cache_read = 2,500 x 1.0e-7  = $0.000250
// AMORTIZED total      = $0.001750
//
// The replay is 62.5% of the tokens and 14.3% of the money, which is the asymmetry the whole
// decomposition exists to show: a large token overcount ratio is a small dollar multiple,
// because a re-removal is worth a tenth of a first removal.
func TestReplayDecompositionIsHandCheckable(t *testing.T) {
	db := openTestDB(t)
	turns := []struct{ gross, unique int }{{1000, 1000}, {1500, 500}, {1500, 0}}
	for i, tr := range turns {
		e := &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 10000 - tr.gross,
			// Warm: a nonzero cache_read is what makes repeatRate the cache-read rate.
			FreshInput: 10, CacheRead: 50000, CacheWrite: 0, OutputTokens: 100,
			TokenAccounting: AccountingComplete,
			Components: []CompRow{{
				Component: "extract", Acted: true, Mutated: true,
				SavedGross: tr.gross, SavedUnique: tr.unique,
			}},
		}
		insertReq(t, db, e)
	}

	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 component row, got %d", len(rows))
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	c := rows[0]
	if c.SavedGross != 4000 || c.SavedUnique != 1500 {
		t.Fatalf("gross/unique = %d/%d, want 4000/1500", c.SavedGross, c.SavedUnique)
	}
	nearEq(t, "first removal", c.SavedUSDFirstRemoval, 0.001500)
	nearEq(t, "replay", c.SavedUSDReplay, 0.000250)
	nearEq(t, "decomposed total", c.SavedUSDDecomposed, 0.001750)
	// The multiple the sign flip turns on, and the number a reader needs: 1.17x in dollars
	// against a 2.67x token overcount ratio.
	nearEq(t, "replay multiple", c.ReplayMultiple, 0.001750/0.001500)
	if got := float64(c.SavedGross) / float64(c.SavedUnique); math.Abs(got-2.6666666667) > 1e-6 {
		t.Errorf("token overcount ratio = %.4f, want 2.6667", got)
	}
}

// TestReplayIsPricedAtTheTierTheTurnActuallyPaid pins the rule that the brief asked for
// explicitly: a replayed token is valued at what THAT turn was billed, not at a fixed rate.
//
// Same single re-removal on two turns, one warm and one whose cache had expired. The warm turn
// prices the replay at the cache-read rate; the cold turn, which had to re-create the prefix,
// prices it at the cache-WRITE rate. Getting this wrong in either direction is a 12.5x error.
func TestReplayIsPricedAtTheTierTheTurnActuallyPaid(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		fresh, read, write     int64
		wantReplayRatePerToken float64
	}{
		{"warm turn pays the cache-read rate", 10, 50000, 0, 1e-7},
		// cache_write >= fresh_input is repeatRate's cache-creation case.
		{"cold turn pays the cache-creation rate", 10, 0, 50000, 1.25e-6},
		// No cache at all: full input rate.
		{"uncached turn pays full input rate", 50000, 0, 0, 1e-6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			// Turn 1 establishes the content as unique; turn 2 is the replay under test.
			insertReq(t, db, &Event{
				TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
				TokensBefore: 10000, TokensAfter: 9000,
				FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
				Components: []CompRow{{Component: "extract", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 1000}},
			})
			insertReq(t, db, &Event{
				TS: 2000, SessionID: "s1", Model: "m1", TenantID: "t1",
				TokensBefore: 10000, TokensAfter: 9000,
				FreshInput: tc.fresh, CacheRead: tc.read, CacheWrite: tc.write,
				TokenAccounting: AccountingComplete,
				Components: []CompRow{{Component: "extract", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 0}},
			})
			rows, err := db.Components(Filter{TenantAll: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
				t.Fatal(err)
			}
			nearEq(t, "replay", rows[0].SavedUSDReplay, 1000*tc.wantReplayRatePerToken)
			// The first removal is priced at TURN 1's own tier and is unaffected by the replay
			// tier under test — turn 1 read 50k and wrote nothing, so Event.uniqueRate is the
			// fresh rate, not cache creation. Same 1,000 tokens in all three cases.
			nearEq(t, "first removal", rows[0].SavedUSDFirstRemoval, 1000*1e-6)
		})
	}
}

// TestDecompositionReconcilesWithTheStoredFigure is the cross-check the UI prints.
//
// Event.Price computes baseline_cost_usd and request_components.saved_usd at WRITE time from the
// live price; DecomposeComponentSavedUSD recomputes the same quantity at READ time from the
// stored token columns. They are different code over the same rows, so their agreement is
// evidence that neither is drifting — and the panel says "cross-check FAILED" if they part.
func TestDecompositionReconcilesWithTheStoredFigure(t *testing.T) {
	db := openTestDB(t)
	for i, tr := range []struct{ gross, unique int }{{800, 800}, {1200, 400}, {1200, 0}, {1500, 300}} {
		e := &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 20000, TokensAfter: 20000 - tr.gross,
			FreshInput: 5, CacheRead: 40000, OutputTokens: 50,
			Components: []CompRow{{Component: "extract", Acted: true, Mutated: true,
				SavedGross: tr.gross, SavedUnique: tr.unique}},
		}
		// Price at write time, exactly as the proxy does.
		p, _ := handPrice.Price(context.Background(), "m1")
		e.Price(p, true)
		insertReq(t, db, e)
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	c := rows[0]
	if c.SavedUSD == 0 {
		t.Fatal("stored saved_usd is 0; the fixture did not price at write time")
	}
	nearEq(t, "read-time decomposition vs write-time stored figure", c.SavedUSDDecomposed, c.SavedUSD)
}

// TestGatewayPriceWinsForTheComponentVerdict pins the fix for the same model calls being
// reported at two different prices on two tabs.
//
// extraction_calls.cost_usd is what the COMPONENT priced its own call at (public model map);
// requests.cg_llm_cost_usd is what the PROXY priced the identical call at from the operator's
// configured rate list — the rates the invoice is denominated in. Measured on production the
// two are 31.6% apart, and the Components tab showed one while Overview showed the other. The
// verdict must follow the invoice, and both figures must stay visible.
func TestGatewayPriceWinsForTheComponentVerdict(t *testing.T) {
	db := openTestDB(t)
	e := &Event{
		TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 10000, TokensAfter: 9000,
		FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
		// The proxy's own gateway-priced figure for this request's model calls.
		CGLLMCostUSD: 0.60,
		Components: []CompRow{{Component: "extract_llm", Acted: true, Mutated: true,
			SavedGross: 1000, SavedUnique: 1000}},
		// The component's own self-reported price for the same call: 31.6% higher.
		Extractions: []ExtractionRow{{Component: "extract_llm", Model: "m1", CostUSD: 0.79}},
	}
	insertReq(t, db, e)
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	c := rows[0]
	nearEq(t, "gateway-priced cost", c.LLMCostGatewayUSD, 0.60)
	nearEq(t, "component-reported cost", c.LLMCostReportedUSD, 0.79)
	nearEq(t, "the figure the verdict uses", c.LLMCostUSD, 0.60)
	// And the verdict is computed from it, not from the component's own number.
	nearEq(t, "net", c.NetUSD, c.SavedUSD-0.60)
}

// TestSelfRemovalNeedsComparableLaterSessions pins the correctness restriction on the
// self-removal credit, which the first implementation got wrong in a way that mattered.
//
// That version considered every declaration and reported that the account had "removed" Bash,
// Agent and TodoWrite — because sessions legitimately declare different inventories, so a name
// missing from a later session is not evidence of anything. Two rules fix it: only MCP tools
// and skills are eligible, and only a later session of the same COHORT (one that carries MCP
// tools at all, or a skills listing at all) can testify.
func TestSelfRemovalNeedsComparableLaterSessions(t *testing.T) {
	db := openTestDB(t)
	// Session 1 declares a built-in, an MCP tool and a skill, and is the only one to do so.
	decl := func(session string, ts int64, kinds [][3]string) {
		e := &Event{
			TS: ts, SessionID: session, Model: "m1", TenantID: "t1",
			TokensBefore: 5000, TokensAfter: 5000, Meta: Meta{Tools: 3},
			FreshInput: 10, CacheRead: 20000, TokenAccounting: AccountingComplete,
		}
		insertReq(t, db, e)
		for _, k := range kinds {
			if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
				tenant_id, session_id, digest, kind, name, server, tokens, ts)
				VALUES('t1',?,'d1',?,?,?,100,?)`, session, k[0], k[1], k[2], ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	decl("s1", 1000, [][3]string{
		{KindTool, "Bash", ""},
		{KindMCPTool, "mcp__srv__thing", "srv"},
		{KindSkill, "someskill", ""},
	})
	// Five later sessions that carry an MCP tool and a skills listing but NOT the two above:
	// these are comparable, so they can testify.
	for i := 0; i < 5; i++ {
		decl("later"+string(rune('a'+i)), int64(2000+i), [][3]string{
			{KindMCPTool, "mcp__srv__other", "srv"},
			{KindSkillListing, "listing", SkillsOK},
		})
	}
	// Ten later sessions that declare nothing comparable at all. These must NOT count as
	// evidence for anything — they are the rows that produced the false positives.
	for i := 0; i < 10; i++ {
		decl("bare"+string(rune('a'+i)), int64(3000+i), [][3]string{{KindTool, "Read", ""}})
	}

	got, err := db.SelfRemovals(Filter{TenantAll: true},
		func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) },
		nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]SelfRemoval{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if _, ok := byName["Bash"]; ok {
		t.Error("a built-in client tool was reported as self-removed; that is the false positive " +
			"this restriction exists to prevent")
	}
	mcp, ok := byName["mcp__srv__thing"]
	if !ok {
		t.Fatal("the removed MCP tool was not credited")
	}
	// Only the five comparable sessions testify, never the ten bare ones.
	if mcp.SessionsAfter != 5 {
		t.Errorf("SessionsAfter = %d, want 5 (only sessions that carry MCP tools can testify)",
			mcp.SessionsAfter)
	}
	sk, ok := byName["someskill"]
	if !ok {
		t.Fatal("the removed skill was not credited")
	}
	if sk.SessionsAfter != 5 {
		t.Errorf("skill SessionsAfter = %d, want 5", sk.SessionsAfter)
	}
	// Overlap with a server-side filter list is reported, never netted away.
	got2, err := db.SelfRemovals(Filter{TenantAll: true},
		func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) },
		[]string{"mcp__srv__thing"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range got2 {
		if r.Name == "mcp__srv__thing" {
			found = r.Overlap
		}
	}
	if !found {
		t.Error("a name on the server-side filter list must be marked Overlap so one reduction " +
			"is not credited twice")
	}
}

// TestSelfRemovalDoesNotPoolAcrossTenants pins the bug found auditing the manager's
// service-wide (TenantAll) view against a copy of the live DB: an MCP tool declared in exactly
// ONE session of ONE tenant was reported "removed" on the strength of hundreds of OTHER
// tenants' sessions that simply never carried that tenant's MCP server at all — a candidate's
// query grouped `tool_declarations` by (kind, name, server) with no tenant_id in sight, so two
// unrelated accounts' declaration timelines for a same-named item were pooled into one. The
// production instance of this credited $154.53 of avoided cost to a removal nobody made.
//
// t1 declares mcp__srv__thing once and never again; t2, which has ALWAYS carried it (every one
// of its sessions still declares it), must contribute nothing to t1's SessionsAfter or dollars —
// and t1's own comparable-but-not-yet-three-strong count must not be inflated to "removed" by
// borrowing t2's session count. session_id namespaces overlap deliberately (see
// TestInventoryKeysAreTenantScoped, same technique on the write side): a fix that merely happened
// to key off distinct ids would still be wrong the day two tenants' clients collide.
func TestSelfRemovalDoesNotPoolAcrossTenants(t *testing.T) {
	db := openTestDB(t)
	decl := func(tenant, session string, ts int64, kinds [][3]string) {
		e := &Event{
			TS: ts, SessionID: session, Model: "m1", TenantID: tenant,
			TokensBefore: 5000, TokensAfter: 5000, Meta: Meta{Tools: 3},
			FreshInput: 10, CacheRead: 20000, TokenAccounting: AccountingComplete,
		}
		insertReq(t, db, e)
		for _, k := range kinds {
			if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
				tenant_id, session_id, digest, kind, name, server, tokens, ts)
				VALUES(?,?,'d1',?,?,?,100,?)`, tenant, session, k[0], k[1], k[2], ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	mcpTool := [][3]string{{KindMCPTool, "mcp__srv__thing", "srv"}}
	// t1: one session declares the tool, then five later (same session-id namespace as t2's,
	// on purpose) comparable sessions that do not.
	decl("t1", "shared-a", 1000, mcpTool)
	for i := 0; i < 5; i++ {
		decl("t1", "shared-later"+string(rune('a'+i)), int64(2000+i), [][3]string{{KindMCPTool, "mcp__srv__other", "srv"}})
	}
	// t2: carries the SAME tool in every one of its sessions, including ones that collide with
	// t1's session ids above. If these leak into t1's evidence, t1's tool would wrongly look
	// still-carried (diluting SessionsAfter) or, in the pre-fix bug's actual failure mode,
	// t2's carrying sessions would wrongly count as t1's "confirmed removed" testimony.
	for i := 0; i < 5; i++ {
		decl("t2", "shared-later"+string(rune('a'+i)), int64(2000+i), mcpTool)
	}

	got, err := db.SelfRemovals(Filter{TenantAll: true},
		func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) },
		nil)
	if err != nil {
		t.Fatal(err)
	}
	var mine SelfRemoval
	found := false
	for _, r := range got {
		if r.Name == "mcp__srv__thing" {
			mine, found = r, true
		}
	}
	if !found {
		t.Fatal("t1's removed MCP tool was not credited at all")
	}
	// Only t1's own 5 later sessions may testify. If t2's sessions leaked in, this would be 10.
	if mine.SessionsAfter != 5 {
		t.Errorf("SessionsAfter = %d, want 5 — t2's sessions must not count as t1's evidence",
			mine.SessionsAfter)
	}
	// t1's carried-then-dropped tool was 100 tokens; t2 read 20000 tokens on 5 sessions of a
	// tool it never dropped. If those requests were priced into t1's row, AvoidedUSD would be
	// off by orders of magnitude. Sanity bound: at 100 tokens x 5 sessions x a few requests each,
	// this cannot rationally exceed a fraction of a cent.
	if mine.AvoidedUSD > 0.01 {
		t.Errorf("AvoidedUSD = %.6f, implausibly large for a 100-token item over 5 sessions — "+
			"looks like another tenant's traffic leaked in", mine.AvoidedUSD)
	}
}

// TestSessionLengthUsesTheRequestWeightedMean pins the statistic a per-session projection
// multiplies by.
//
// A corpus of many one-request sidechains plus a few long sessions has a median session length
// of 1 and a mean under 4, and quoting either as "an average session" understates the cost of
// carrying a declaration by more than an order of magnitude — the money is in the long
// sessions, which is where a typical REQUEST lives. sum(n^2)/sum(n) is that figure.
//
// Fixture: nine 1-request sessions and one 100-request session.
//
//	plain mean       = 109/10                      = 10.9
//	median           =                               1
//	request-weighted = (9*1 + 100*100) / 109       = 10009/109 = 91.8
func TestSessionLengthUsesTheRequestWeightedMean(t *testing.T) {
	db := openTestDB(t)
	add := func(session string, ts int64) {
		insertReq(t, db, &Event{
			TS: ts, SessionID: session, Model: "m1", TenantID: "t1",
			TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
			FreshInput: 5, CacheRead: 100, TokenAccounting: AccountingComplete,
		})
	}
	for i := 0; i < 9; i++ {
		s := "short" + string(rune('a'+i))
		add(s, int64(1000+i))
		if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
			tenant_id, session_id, digest, kind, name, server, tokens, ts)
			VALUES('t1',?,'d1',?,'X','',100,1)`, s, KindTool); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		add("long", int64(5000+i))
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts)
		VALUES('t1','long','d1',?,'X','',100,1)`, KindTool); err != nil {
		t.Fatal(err)
	}
	rep, err := db.ToolReportFor(Filter{TenantAll: true},
		func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.RequestsPerSessionMedian != 1 {
		t.Errorf("median = %d, want 1", rep.Totals.RequestsPerSessionMedian)
	}
	nearEq(t, "request-weighted mean", rep.Totals.RequestsPerSessionTypical, 10009.0/109.0)
	if rep.Totals.RequestsPerSessionTypical <= rep.Totals.RequestsPerSession {
		t.Error("the request-weighted mean must exceed the plain mean whenever session lengths " +
			"vary; if it does not, the projection is understating the cost")
	}
}

// TestSharesAreAPartOfARealWhole pins the denominator bug that produced shares of 650%.
//
// SharePct used to divide by Totals.DeclaredTokens, which is a per-session MEAN. With sessions
// ranging from a 2-tool sidechain to a full inventory the mean is far below a single large
// declaration, so shares exceeded 100% — which is not a rounding problem, it is proof the
// denominator is not the whole the numerator is part of.
func TestSharesAreAPartOfARealWhole(t *testing.T) {
	db := openTestDB(t)
	// One big session and many tiny ones, which is the shape that exposed the bug.
	insertReq(t, db, &Event{
		TS: 1000, SessionID: "big", Model: "m1", TenantID: "t1", Meta: Meta{Tools: 3},
		TokensBefore: 100, TokensAfter: 100, FreshInput: 5, CacheRead: 100,
		TokenAccounting: AccountingComplete,
	})
	for i, tok := range []int{5000, 3000, 2000} {
		if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
			tenant_id, session_id, digest, kind, name, server, tokens, ts)
			VALUES('t1','big','d1',?,?,'',?,1)`,
			KindMCPTool, "mcp__s__t"+string(rune('a'+i)), tok); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		s := "tiny" + string(rune('a'+i))
		insertReq(t, db, &Event{
			TS: int64(2000 + i), SessionID: s, Model: "m1", TenantID: "t1", Meta: Meta{Tools: 1},
			TokensBefore: 100, TokensAfter: 100, FreshInput: 5, CacheRead: 100,
			TokenAccounting: AccountingComplete,
		})
		if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
			tenant_id, session_id, digest, kind, name, server, tokens, ts)
			VALUES('t1',?,'d1',?,'mcp__s__ta','',5000,1)`, s, KindMCPTool); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := db.ToolReportFor(Filter{TenantAll: true},
		func(m string) (modelinfo.Price, bool) { return handPrice.Price(context.Background(), m) })
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, tl := range rep.Tools {
		if tl.SharePct > 100 {
			t.Errorf("%s has share %.1f%%: a part cannot exceed its whole", tl.Name, tl.SharePct)
		}
		sum += tl.SharePct
	}
	if sum > 100.001 {
		t.Errorf("shares sum to %.2f%%, which is not a partition of one whole", sum)
	}
	if rep.Totals.DeclaredSetTokens != 10000 {
		t.Errorf("DeclaredSetTokens = %d, want 10000 (5000+3000+2000, each counted once)",
			rep.Totals.DeclaredSetTokens)
	}
}

// TestBuiltinClassification pins the allowlist, including the two names that are easy to get
// wrong: there is no `Task` tool (the subagent spawner is `Agent`), and a client tool that is
// not one of Claude Code's own must NOT be filed under the built-ins the UI warns about.
func TestBuiltinClassification(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		builtin    bool
		removal    string
	}{
		{KindTool, "Read", true, "builtin"},
		{KindTool, "Agent", true, "builtin"},
		{KindTool, "TaskStop", true, "builtin"},
		// Verified against a live Claude Code session, and previously missing — each was
		// classified client_tool, kept out of the collapsed danger section and offered a
		// removal command with no warning.
		{KindTool, "Artifact", true, "builtin"},
		{KindTool, "SendMessage", true, "builtin"},
		{KindTool, "CronCreate", true, "builtin"},
		{KindTool, "EnterWorktree", true, "builtin"},
		{KindTool, "ExitWorktree", true, "builtin"},
		// Not a real tool name — the thing people write when they mean Agent.
		{KindTool, "Task", false, "client_tool"},
		// Some other agent's own tool: removable, and must not be warned about as a built-in.
		{KindTool, "DesignSync", false, "client_tool"},
		{KindMCPTool, "mcp__srv__thing", false, "mcp_tool"},
		{KindSkill, "dataviz", false, "skill"},
		{KindSkill, "someplugin:someskill", false, "plugin_skill"},
		// A DIRECTORY-SCOPED skill also wears a colon and is NOT a plugin: the prefix is a path.
		// Routing it to the plugin mechanism emitted `claude plugin disable apps/web`, which names
		// no plugin and silently does nothing. See TestDirectoryScopedSkillIsNotAPlugin.
		{KindSkill, "apps/web:deploy", false, "skill"},
		{KindServerTool, "code_execution", false, "provider"},
	} {
		if got := IsBuiltinTool(tc.kind, tc.name); got != tc.builtin {
			t.Errorf("IsBuiltinTool(%q,%q) = %v, want %v", tc.kind, tc.name, got, tc.builtin)
		}
		r := RemovalFor(tc.kind, tc.name, "srv")
		if r.Kind != tc.removal {
			t.Errorf("RemovalFor(%q,%q).Kind = %q, want %q", tc.kind, tc.name, r.Kind, tc.removal)
		}
		if tc.builtin && !r.Danger {
			t.Errorf("%s is a built-in and its removal must be marked dangerous", tc.name)
		}
		if !tc.builtin && r.Danger {
			t.Errorf("%s is not a built-in and must not be marked dangerous", tc.name)
		}
	}
}

// TestRemovalSnippetsUseTheBareToolName pins the one semantic the whole removal feature rests
// on: in permissions.deny a BARE tool name removes the declaration from the prompt, while a
// SCOPED rule only blocks the call and saves nothing. A dashboard about token weight that
// emitted a scoped rule would be advising a change with no effect on the number it displays.
//
// Also pins that no snippet uses `disallowedTools` as a settings.json key, which is not one —
// it exists only as a CLI flag, an SDK option and frontmatter.
func TestRemovalSnippetsUseTheBareToolName(t *testing.T) {
	r := RemovalFor(KindTool, "WebSearch", "")
	if want := `"deny": ["WebSearch"]`; !hasSub(r.Settings, want) {
		t.Errorf("settings snippet = %q, want it to contain %q", r.Settings, want)
	}
	if hasSub(r.Settings, "WebSearch(") {
		t.Error("a scoped rule leaves the declaration in the prompt and saves nothing")
	}
	if hasSub(r.Settings, "disallowedTools") {
		t.Error("disallowedTools is not a settings.json key; permissions.deny is")
	}
	// A plugin-bundled MCP server was never added by hand, so `claude mcp remove` has no name
	// to take and must not be offered.
	p := RemovalFor(KindMCPTool, "mcp__plugin_ctx_ctx__query", "plugin_ctx_ctx")
	if p.Command != "" {
		t.Errorf("a plugin-bundled server must not be offered `claude mcp remove`, got %q", p.Command)
	}
	// A hand-added server should be.
	h := RemovalFor(KindMCPTool, "mcp__srv__query", "srv")
	if h.Command != "claude mcp remove srv" {
		t.Errorf("Command = %q, want `claude mcp remove srv`", h.Command)
	}
}

func hasSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEstimatorDivergenceIsAMedianOverComparableRows pins the honesty check on the token unit.
//
// tokens_before is schema.MessagesTokens — message TEXT only, counted locally — while the
// provider bills the system prompt, the tool declarations and the JSON envelope too. On
// production the median per-request ratio is 3.38x. Two things have to be right or the figure
// lies: it must be measured only where the two counts describe the SAME prompt (nothing
// removed), and it must be a MEDIAN, because a ratio of sums is dominated by the largest
// requests and reads much lower (2.87x on the same corpus).
func TestEstimatorDivergenceIsAMedianOverComparableRows(t *testing.T) {
	db := openTestDB(t)
	// Five comparable rows with ratios 2, 3, 4, 5, 6 -> median 4.
	for i, mult := range []int64{2, 3, 4, 5, 6} {
		insertReq(t, db, &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 1000, TokensAfter: 1000, // nothing removed: comparable
			CacheRead: 1000 * mult, FreshInput: 0, TokenAccounting: AccountingComplete,
		})
	}
	// One enormous row that COMPACTED something. It must be excluded: the two counts no longer
	// describe the same prompt, and its size would swamp a ratio of sums.
	insertReq(t, db, &Event{
		TS: 2000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 1000000, TokensAfter: 900000,
		CacheRead: 50000000, TokenAccounting: AccountingComplete,
	})
	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.EstimatorDivergenceRows != 5 {
		t.Errorf("rows = %d, want 5 (the compacted row is not comparable and must be excluded)",
			o.EstimatorDivergenceRows)
	}
	nearEq(t, "median divergence", o.EstimatorDivergence, 4)
	// A ratio of sums over the same five rows would be 20000/5000 = 4 as well, so prove the
	// median is doing the work: skew the population and the two must part.
	insertReq(t, db, &Event{
		TS: 3000, SessionID: "s2", Model: "m1", TenantID: "t1",
		TokensBefore: 100000, TokensAfter: 100000,
		CacheRead: 10000000, TokenAccounting: AccountingComplete, // ratio 100
	})
	o2, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	sumRatio := float64(o2.BilledInputTokens) / float64(o2.TokensBefore)
	if o2.EstimatorDivergence >= sumRatio {
		t.Errorf("median %.2f must be BELOW the ratio-of-sums %.2f once one huge row is present; "+
			"if they track each other the median is not being computed",
			o2.EstimatorDivergence, sumRatio)
	}
}

// TestBufferedAndStreamedLatencyAreNeverBlended pins that the two response-latency figures stay
// apart.
//
// They come from the same ttfb_ms column and are NOT the same measurement: on a streamed
// response it is a real time-to-first-byte, on a buffered one proxy.go leaves the first-byte
// instant zero and msSince falls back to now, so the value is the TOTAL response time. Averaging
// the two populations together would report the healthiest number for exactly the requests
// having the worst experience.
func TestBufferedAndStreamedLatencyAreNeverBlended(t *testing.T) {
	db := openTestDB(t)
	insertReq(t, db, &Event{
		TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 100, TokensAfter: 100, CacheRead: 100,
		TokenAccounting: AccountingComplete, TTFBMs: 800, SSEBuffered: false,
	})
	insertReq(t, db, &Event{
		TS: 2000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 100, TokensAfter: 100, CacheRead: 100,
		TokenAccounting: AccountingComplete, TTFBMs: 29000, SSEBuffered: true,
	})
	// A non-streamed row: no TTFB at all, and it must not drag either average toward zero.
	insertReq(t, db, &Event{
		TS: 3000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 100, TokensAfter: 100, CacheRead: 100,
		TokenAccounting: AccountingComplete, TTFBMs: 0, SSEBuffered: false,
	})
	o, err := db.Overview(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.SSEStreamed != 1 || o.SSEBuffered != 1 {
		t.Fatalf("streamed/buffered = %d/%d, want 1/1", o.SSEStreamed, o.SSEBuffered)
	}
	nearEq(t, "streamed first byte", o.TTFBMsAvgStreamed, 800)
	nearEq(t, "buffered total", o.TotalMsAvgBuffered, 29000)
	nearEq(t, "buffered share", o.SSEBufferedPct, 50)
	// Coverage: rows that carry neither fact predate the capture and must be countable, so the
	// UI can say "not recorded yet" instead of showing a 0% buffered rate over old history.
	if o.SSERecorded != 2 {
		t.Errorf("SSERecorded = %d, want 2 (the non-streamed row carries neither fact)", o.SSERecorded)
	}
}

// TestMarkUniqueCountsKeylessComponentsInFull is the test that was missing, and its absence is
// why the fabricated `unique` for the reformatters went unnoticed.
//
// Both decomposition tests above set SavedUnique directly as an Event field, so they never reach
// MarkUnique and cannot see that it returns the saving IN FULL when a component reports no
// content keys. Only Offload components ever set one — the pipeline assigns CacheKeys solely on
// the Offload branch — so for every reformatter `unique == gross` on every turn, by construction.
//
// That is not a bug in MarkUnique: with no key there is nothing to dedup against, and counting
// the run once is the only defensible answer. The bug was reading the resulting 1.0x ratio as
// "every removal was genuinely new content" and using it as evidence the decomposition is sound.
func TestMarkUniqueCountsKeylessComponentsInFull(t *testing.T) {
	// A real Recorder: MarkUnique writes into seenKeys, which NewRecorder allocates.
	rec, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test teardown

	// A keyed component: the same content key on a later turn contributes nothing.
	if got := rec.MarkUnique("t1", "extract", []string{"k1"}, 1000); got != 1000 {
		t.Errorf("first sighting of a keyed removal = %d, want 1000", got)
	}
	if got := rec.MarkUnique("t1", "extract", []string{"k1"}, 1000); got != 0 {
		t.Errorf("repeat of a keyed removal = %d, want 0 — the key was already seen", got)
	}

	// A KEYLESS component: full saving every time, so unique can never fall below gross and the
	// overcount ratio is pinned at exactly 1.0 forever.
	for turn := 1; turn <= 5; turn++ {
		if got := rec.MarkUnique("t1", "textclean", nil, 328); got != 328 {
			t.Errorf("turn %d of a keyless removal = %d, want 328 — MarkUnique cannot dedup "+
				"without a key, so every turn counts in full", turn, got)
		}
	}

	// Tenant namespacing still holds for keyed components: one account's key must not suppress
	// another's identical content.
	if got := rec.MarkUnique("t2", "extract", []string{"k1"}, 1000); got != 1000 {
		t.Errorf("another tenant's identical key = %d, want 1000", got)
	}
}

// TestUnkeyedComponentsAreFlaggedNotRepriced pins the reporting decision that follows from the
// test above: a reformatter's dollar figure is marked as not-a-measurement, and is NOT moved to
// a different rate on an inference.
//
// The stakes: with unique == gross the decomposition puts a reformatter's whole saving at the
// cache-WRITE rate (12.5x a read) and reports a replay multiple of exactly 1.00. On measured
// traffic that is 77% of the entire "credited once" figure, and it runs in the flattering
// direction — onto the very verdict the decomposition exists to make conservative.
func TestUnkeyedComponentsAreFlaggedNotRepriced(t *testing.T) {
	db := openTestDB(t)
	// Two components on the same warm turns: one Offload (keyed), one Reformat (keyless, so its
	// unique equals its gross exactly as MarkUnique would leave it).
	// 25 turns: above the row floor the flag needs before it will judge at all.
	for i := 0; i < 25; i++ {
		insertReq(t, db, &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 8000,
			FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
			Components: []CompRow{
				{Component: "extract", Kind: "offload", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: map[bool]int{true: 1000, false: 0}[i == 0]},
				{Component: "textclean", Kind: "reformat", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 1000},
			},
		})
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	by := map[string]*ComponentRow{}
	for _, c := range rows {
		by[c.Component] = c
	}
	kl, keyed := by["textclean"], by["extract"]
	if kl == nil || keyed == nil {
		t.Fatal("both components should be present")
	}
	if !kl.UniqueUnkeyed {
		t.Error("a reformat component reports no content key, so its unique is not a dedup " +
			"measurement and it must be flagged")
	}
	if keyed.UniqueUnkeyed {
		t.Error("an offload component does set content keys and must NOT be flagged")
	}
	// Not repriced: the flagged component's whole saving is still valued as a first removal on
	// every turn, exactly as the stored figures say. Flagging is a disclosure, not an
	// adjustment. These turns read cache and wrote none, so the first-removal rate is the FRESH
	// rate (Event.uniqueRate) — the flag's meaning is unchanged by which rate that is.
	nearEq(t, "keyless first removal", kl.SavedUSDFirstRemoval, 25*1000*1e-6)
	nearEq(t, "keyless replay", kl.SavedUSDReplay, 0)
	nearEq(t, "keyless replay multiple", kl.ReplayMultiple, 1)
	// The keyed one behaves as before: 1,000 unique at this turn's entry rate, 2,000 at read.
	nearEq(t, "keyed first removal", keyed.SavedUSDFirstRemoval, 1000*1e-6)
	nearEq(t, "keyed replay", keyed.SavedUSDReplay, 24*1000*1e-7)
}

// TestCrossCheckCoversOnlyStoredRows pins the fix for a cross-check that was near-tautological.
//
// EstimateComponentSavedUSD and DecomposeComponentSavedUSD run the IDENTICAL formula and their
// queries differ only by `AND c.saved_usd = 0`. So for a row with no stored figure the two agree
// by arithmetic, and on production almost every row is in that bucket — which made
// "their agreement is evidence, not a tautology" false exactly where it mattered.
// SavedUSDDecomposedStored is the part that is genuinely checked, and the UI prints the fraction.
func TestCrossCheckCoversOnlyStoredRows(t *testing.T) {
	db := openTestDB(t)
	p, _ := handPrice.Price(context.Background(), "m1")
	// One row priced at write time (stored), one left unpriced (estimated on read).
	stored := &Event{
		TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 10000, TokensAfter: 9000,
		FreshInput: 10, CacheRead: 50000, OutputTokens: 10,
		Components: []CompRow{{Component: "extract", Kind: "offload", Acted: true,
			Mutated: true, SavedGross: 1000, SavedUnique: 1000}},
	}
	stored.Price(p, true)
	insertReq(t, db, stored)
	insertReq(t, db, &Event{
		TS: 2000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 10000, TokensAfter: 9000,
		FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
		Components: []CompRow{{Component: "extract", Kind: "offload", Acted: true,
			Mutated: true, SavedGross: 1000, SavedUnique: 1000}},
	})
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	c := rows[0]
	if c.SavedUSDDecomposedStored <= 0 {
		t.Fatal("the stored-row half of the decomposition should be nonzero")
	}
	if c.SavedUSDDecomposedStored >= c.SavedUSDDecomposed {
		t.Errorf("stored-only %.9f must be strictly below the whole decomposition %.9f: half "+
			"these rows carry no stored figure and are therefore not cross-checked at all",
			c.SavedUSDDecomposedStored, c.SavedUSDDecomposed)
	}
	// And the checked half really does agree with what was stored.
	nearEq(t, "stored-row cross-check", c.SavedUSDDecomposedStored, c.SavedUSD)
}

// TestEndConversationCannotBeRemoved pins the documented exception: a deny rule cannot drop this
// tool while any other remains, so emitting the usual "removes it from the prompt entirely"
// promise for it would be a false claim in a UI whose whole job is not making those.
func TestEndConversationCannotBeRemoved(t *testing.T) {
	r := RemovalFor(KindTool, "EndConversation", "")
	if r.Command != "" || r.Settings != "" {
		t.Errorf("no removal should be offered: command=%q settings=%q", r.Command, r.Settings)
	}
	if !r.Danger {
		t.Error("still a built-in, so still marked dangerous")
	}
	if !hasSub(r.Effect, "Cannot be removed") {
		t.Errorf("Effect must say it cannot be removed, got %q", r.Effect)
	}
}

// TestUnkeyedFlagClearsOnceRowsDedup is the point of deriving the flag from rows rather than from
// the component's kind.
//
// PR #89 gives the reformatters content-derived keys, so their saved_unique becomes a real dedup
// measurement. A flag keyed off `Kind == "reformat"` could never notice: it would keep printing
// "NOT a deduplicated figure" over figures that had become measurements — this dashboard's own
// worst failure mode, triggered by somebody else's merge rather than by any change here.
//
// Same component, same kind, rows that dedup: the flag must be off.
func TestUnkeyedFlagClearsOnceRowsDedup(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 25; i++ {
		insertReq(t, db, &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 9000,
			FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
			// Kind is still "reformat" — only the DATA changed, which is exactly the
			// post-#89 situation.
			Components: []CompRow{{Component: "textclean", Kind: "reformat", Acted: true,
				Mutated: true, SavedGross: 1000, SavedUnique: map[bool]int{true: 1000, false: 0}[i == 0]}},
		})
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].UniqueUnkeyed {
		t.Error("rows that dedup must clear the flag regardless of the component's kind — " +
			"otherwise the warning outlives the defect it describes")
	}
	// And the money moves to where a real dedup measurement puts it: one first removal at the
	// tier that turn would have entered content at (read-only turn, so fresh), 24 replays at the
	// read rate.
	nearEq(t, "first removal", rows[0].SavedUSDFirstRemoval, 1000*1e-6)
	nearEq(t, "replay", rows[0].SavedUSDReplay, 24*1000*1e-7)

	// Below the row floor the flag stays OFF even with no differing rows: a handful of rows that
	// happen to agree is not evidence, and calling a real measurement fake is the worse error.
	db2 := openTestDB(t)
	for i := 0; i < 3; i++ {
		insertReq(t, db2, &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 9000,
			FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
			Components: []CompRow{{Component: "textclean", Kind: "reformat", Acted: true,
				Mutated: true, SavedGross: 1000, SavedUnique: 1000}},
		})
	}
	few, err := db2.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db2.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, few); err != nil {
		t.Fatal(err)
	}
	if few[0].UniqueUnkeyed {
		t.Error("three agreeing rows are not enough to call a component's unique fabricated")
	}
}

// TestUniqueHasThreeStatesNotTwo pins the reason the boolean was not enough.
//
// Measured on production: 892 of 908 (session, component) pairs carry fewer than 20 rows, so in
// a session drawer — the view users actually drill into — a boolean flag is off and a fabricated
// figure renders identically to a real measurement. And a MIXED window (some rows deduped, some
// not) is off too, while part of its dollars are still fabricated. Both need their own answer.
func TestUniqueHasThreeStatesNotTwo(t *testing.T) {
	// rows = turns to write, dedupFrom = the turn index from which unique starts differing.
	mk := func(t *testing.T, rows, dedupFrom int) *ComponentRow {
		t.Helper()
		db := openTestDB(t)
		for i := 0; i < rows; i++ {
			uniq := 1000
			if i >= dedupFrom {
				uniq = 0
			}
			insertReq(t, db, &Event{
				TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
				TokensBefore: 10000, TokensAfter: 9000,
				FreshInput: 10, CacheRead: 50000, TokenAccounting: AccountingComplete,
				Components: []CompRow{{Component: "textclean", Kind: "reformat", Acted: true,
					Mutated: true, SavedGross: 1000, SavedUnique: uniq}},
			})
		}
		out, err := db.Components(Filter{TenantAll: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, out); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}

	// 25 rows, none dedup -> confident: not a measurement.
	if c := mk(t, 25, 25); !c.UniqueUnkeyed || c.UniqueRows != 25 || c.UniqueDiffRows != 0 {
		t.Errorf("fabricated case: unkeyed=%v rows=%d diff=%d, want true/25/0",
			c.UniqueUnkeyed, c.UniqueRows, c.UniqueDiffRows)
	}
	// 5 rows, none dedup -> the 98.2% case. The flag must be OFF, and the counts must still be
	// there so the UI can say "too few rows to tell" instead of implying a measurement.
	c := mk(t, 5, 5)
	if c.UniqueUnkeyed {
		t.Error("five rows is not enough to call a figure fabricated")
	}
	if c.UniqueRows != 5 || c.UniqueDiffRows != 0 {
		t.Errorf("too-few case must still report its evidence: rows=%d diff=%d, want 5/0",
			c.UniqueRows, c.UniqueDiffRows)
	}
	// Any dedup at all means the component reports content keys, which is what is being
	// tested — so this is MEASURED, not "partly" anything. A row where unique equals gross is
	// the normal first sighting of new content and every healthy component has some.
	m := mk(t, 25, 10)
	if m.UniqueUnkeyed {
		t.Error("a window where rows deduped is not fabricated")
	}
	if m.UniqueDiffRows == 0 {
		t.Errorf("rows did dedup, so diff must be nonzero: diff=%d of rows=%d",
			m.UniqueDiffRows, m.UniqueRows)
	}
}

// TestDirectoryScopedSkillIsNotAPlugin: two different things wear a colon, and conflating them
// produced the worst output this file can emit — a command that appears to succeed and changes
// nothing, in a panel whose whole purpose is to be pasted.
//
// `ponytail:ponytail` is a plugin skill and its unit is the plugin. `apps/web:deploy` is a
// DIRECTORY-SCOPED skill; the prefix is a path, which is why it can contain a `/`, and
// `claude plugin disable apps/web` names nothing that exists. Pre-existing in splitPluginSkill,
// but this change claims "the exact command for each skill", so it is in scope now.
func TestDirectoryScopedSkillIsNotAPlugin(t *testing.T) {
	plug := RemovalFor(KindSkill, "ponytail:ponytail", "")
	if plug.Kind != "plugin_skill" || plug.Command != "claude plugin disable ponytail" {
		t.Errorf("plugin skill = %q / %q, want plugin_skill and the plugin command",
			plug.Kind, plug.Command)
	}
	for _, name := range []string{"apps/web:deploy", "a/b/c:thing"} {
		got := RemovalFor(KindSkill, name, "")
		if got.Kind == "plugin_skill" {
			t.Errorf("%s routed to the plugin mechanism (command %q): that names a path, not a "+
				"plugin, so running it does nothing at all", name, got.Command)
		}
		if got.Kind != "skill" {
			t.Errorf("%s = kind %q, want the plain skill mechanism", name, got.Kind)
		}
		// The snippet names the skill by the SAME string the listing prints, which is what
		// skillOverrides is keyed by.
		if !strings.Contains(got.Settings, name) {
			t.Errorf("%s: the settings snippet does not name it: %q", name, got.Settings)
		}
	}
	// Plain skills and degenerate colons are unaffected.
	for _, name := range []string{"dataviz", ":x", "x:"} {
		if got := RemovalFor(KindSkill, name, ""); got.Kind != "skill" {
			t.Errorf("%s = kind %q, want skill", name, got.Kind)
		}
	}
}
