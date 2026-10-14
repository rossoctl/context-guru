package dash

import "testing"

// #455 / PR #458 review: a committed keep-alive summary call is billed on its ping row, so its
// extraction_calls row (strategy ending "_billed") must not take a share of the next turn's
// gateway cost, and must not drop out of its component's verdict cost when that component also
// has gateway-priced calls.

func TestBilledElsewhereRowTakesNoShareOfTheGatewayCost(t *testing.T) {
	db := openTestDB(t)
	insertReq(t, db, &Event{
		TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 10000, TokensAfter: 9000, FreshInput: 10, CacheRead: 50000,
		TokenAccounting: AccountingComplete,
		CGLLMCostUSD:    0.05, // extract_llm's call only
		Components: []CompRow{
			{Component: "extract_llm", Acted: true, Mutated: true, SavedGross: 1000, SavedUnique: 1000},
			{Component: "cache_aware_summarizer"},
		},
		Extractions: []ExtractionRow{
			{Component: "extract_llm", Model: "m1", CostUSD: 0.05},
			{Component: "cache_aware_summarizer", Model: "m1", CostUSD: 0.0161, Strategy: "messages/keepalive_substitute_billed"},
		},
	})
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, c := range rows {
		if c.Component == "extract_llm" {
			seen = true
			nearEq(t, "extract_llm gateway cost", c.LLMCostGatewayUSD, 0.05)
		}
	}
	if !seen {
		t.Fatal("no extract_llm row: the fixture is broken")
	}
}

func TestBilledElsewhereCallStaysInItsComponentsVerdictCost(t *testing.T) {
	db := openTestDB(t)
	for i, x := range []struct {
		cg, cost float64
		strat    string
	}{{0.02, 0.02, "messages/turn"}, {0, 0.0161, "messages/keepalive_substitute_billed"}} {
		insertReq(t, db, &Event{
			TS: int64(1000 + i), SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 9000, FreshInput: 10, CacheRead: 50000,
			TokenAccounting: AccountingComplete, CGLLMCostUSD: x.cg,
			Components:  []CompRow{{Component: "cache_aware_summarizer", Acted: true}},
			Extractions: []ExtractionRow{{Component: "cache_aware_summarizer", Model: "m1", CostUSD: x.cost, Strategy: x.strat}},
		})
	}
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, c := range rows {
		if c.Component == "cache_aware_summarizer" {
			seen = true
			nearEq(t, "cache_aware_summarizer verdict cost", c.LLMCostUSD, 0.0361)
			nearEq(t, "cache_aware_summarizer gateway cost", c.LLMCostGatewayUSD, 0.02)
		}
	}
	if !seen {
		t.Fatal("no cache_aware_summarizer row: the fixture is broken")
	}
}
