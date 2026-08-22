package dash

import (
	"context"
	"math"
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
// FIRST-REMOVAL value  = 1,500 x cache_write = 1,500 x 1.25e-6 = $0.001875
// REPLAY value         = 2,500 x cache_read  = 2,500 x 1.0e-7  = $0.000250
// AMORTIZED total      = $0.002125
//
// The replay is 62.5% of the tokens and 11.8% of the money, which is the asymmetry the whole
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
	nearEq(t, "first removal", c.SavedUSDFirstRemoval, 0.001875)
	nearEq(t, "replay", c.SavedUSDReplay, 0.000250)
	nearEq(t, "decomposed total", c.SavedUSDDecomposed, 0.002125)
	// The multiple the sign flip turns on, and the number a reader needs: 1.13x in dollars
	// against a 2.67x token overcount ratio.
	nearEq(t, "replay multiple", c.ReplayMultiple, 0.002125/0.001875)
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
			// The first removal is always the cache-write rate regardless of the replay tier.
			nearEq(t, "first removal", rows[0].SavedUSDFirstRemoval, 1000*1.25e-6)
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
		map[string]bool{})
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
		map[string]bool{"mcp__srv__thing": true})
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
		// Not a real tool name — the thing people write when they mean Agent.
		{KindTool, "Task", false, "client_tool"},
		// Some other agent's own tool: removable, and must not be warned about as a built-in.
		{KindTool, "DesignSync", false, "client_tool"},
		{KindMCPTool, "mcp__srv__thing", false, "mcp_tool"},
		{KindSkill, "dataviz", false, "skill"},
		{KindSkill, "someplugin:someskill", false, "plugin_skill"},
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
