package dash

import "testing"

// UNPRICED IS NOT FREE. A tool carried only on a session whose model has no published rate has no dollar
// figure, and the row must say so (priced=false) rather than report a $0 that reads as "this cost nothing".
// Found by the e2e harness: the unpriced probe's items came back priced=true, usd=0.
func TestAToolCarriedOnlyOnAnUnpricedSessionIsReportedUnpriced(t *testing.T) {
	sessions := map[string]*sessionCost{"s-unpriced": {fresh: 1, model: "no-such-model"}}
	decls := []declRow{{session: "s-unpriced", kind: KindMCPTool, name: "mcp__docs__fetch", server: "docs", tokens: 120}}
	rep := buildToolReport(sessions, decls, nil, false)
	var got *ToolStat
	for i := range rep.Tools {
		if rep.Tools[i].Name == "mcp__docs__fetch" {
			got = &rep.Tools[i]
		}
	}
	if got == nil {
		t.Fatalf("the tool is missing from the report: %+v", rep.Tools)
	}
	if got.Priced || got.UnusedUSD != 0 {
		t.Errorf("priced=%v usd=%v on an unpriced session's tool; want priced=false and no dollar figure",
			got.Priced, got.UnusedUSD)
	}
}

// The same rule when the declarations are known but the scope holds no billed request for them: nothing was
// priced, so nothing may claim to be. A row that starts out priced=true and is never visited by an unpriced
// session kept that initial true with usd=0 -- a $0 that is not a measurement.
func TestAToolWithNoPricedRequestInScopeIsNotReportedPriced(t *testing.T) {
	sessions := map[string]*sessionCost{"s-probe": {model: "no-such-model"}} // no reads, writes or fresh
	decls := []declRow{{session: "s-probe", kind: KindMCPTool, name: "mcp__docs__fetch", server: "docs", tokens: 120}}
	rep := buildToolReport(sessions, decls, nil, false)
	for _, st := range rep.Tools {
		if st.Name == "mcp__docs__fetch" && st.Priced {
			t.Fatalf("priced=true usd=%v with no priced request in scope; want priced=false", st.UnusedUSD)
		}
	}
}

// repriceCarried replaces each row's figures with the per-request pass. A row that pass has NO entry for has
// no measured price at all, so it must not keep buildToolReport's priced=true beside the usd=0 this zeroes:
// that combination reads as "measured, and free". Found by the e2e harness (the unpriced probe, priced=true
// usd=0), from the merge that brought repriceCarried in.
func TestRepricingARowThePerRequestPassNeverSawLeavesItUnpriced(t *testing.T) {
	rep := &ToolReport{Tools: []ToolStat{{Kind: KindMCPTool, Name: "mcp__docs__fetch", Server: "docs",
		Priced: true, UnusedUSD: 0.42}}}
	repriceCarried(rep, &carriedScan{Items: map[string]*carried{}}, &carriedScan{Items: map[string]*carried{}}) // no priced request in scope
	if st := rep.Tools[0]; st.Priced || st.UnusedUSD != 0 {
		t.Errorf("a row with no carried entry came back priced=%v usd=%v; want priced=false, usd=0",
			st.Priced, st.UnusedUSD)
	}
}

// The case the harness actually hit: carriedCost pre-creates an entry for every declared item, and a request
// it skips (incomplete token accounting) never touches them, so they stay at Requests=0, Unpriced=false. That
// zero value is "no request priced this", not "priced, and free".
func TestRepricingARowNoPricedRequestCarriedLeavesItUnpriced(t *testing.T) {
	rep := &ToolReport{Tools: []ToolStat{{Kind: KindMCPTool, Name: "mcp__docs__fetch", Server: "docs", Priced: true}}}
	repriceCarried(rep, &carriedScan{Items: map[string]*carried{KindMCPTool + "\x00mcp__docs__fetch": {}}}, &carriedScan{Items: map[string]*carried{}})
	if rep.Tools[0].Priced {
		t.Errorf("a row whose carried entry no request touched came back priced=true; want false")
	}
}

// The other side of the same rule, from the golden fixture: Bash invoked in every session, every request
// priced. A used tool has no unused carry, so no request touches its entry -- but the scope WAS priced, so its
// $0 unused is a measurement and the row stays priced.
func TestRepricingAUsedToolInAPricedScopeKeepsItPriced(t *testing.T) {
	rep := &ToolReport{Aside: []ToolStat{{Kind: KindTool, Name: "Bash", Priced: true, SessionsUsed: 60}}}
	repriceCarried(rep, &carriedScan{Items: map[string]*carried{KindTool + "\x00Bash": {}}, pricedRequests: 591}, &carriedScan{Items: map[string]*carried{}})
	if !rep.Aside[0].Priced {
		t.Errorf("a used tool in a fully priced scope came back priced=false; its $0 unused is measured")
	}
}
