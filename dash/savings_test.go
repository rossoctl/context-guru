package dash

import (
	"math"
	"testing"
)

// TestSavingsTotalsAgreesWithOverview pins the whole point of SavingsTotals: computed
// through its own, smaller query, it must land on exactly the figures Overview's
// hand-tuned megaquery reports for the same rows — cachesplit-only traffic, a priced
// CG-LLM component, and a keep-alive ping all included, since each is a separate addend
// in savingsArithmetic.
func TestSavingsTotalsAgreesWithOverview(t *testing.T) {
	db := openTestDB(t)
	evs := []*Event{
		{TS: 1, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 90,
			CostUSD: 0.01, BaselineCostUSD: 0.02, CGLLMCostUSD: 0.001, CachesplitSavedUSD: 0.003},
		{TS: 2, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
			CostUSD: 0.02, BaselineCostUSD: 0.02, CachesplitSavedUSD: 0.001},
		// A keep-alive ping: counted in the ping-cost query only, never in the agent SUMs.
		{TS: 3, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
			CostUSD: 0.0005, BaselineCostUSD: 0.0005, KeepAlive: true},
		// The real turn the ping kept warm for, credited with a keep-alive saving.
		{TS: 4, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 95,
			CostUSD: 0.015, BaselineCostUSD: 0.02, KeepAliveSavedUSD: 0.004},
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.SavingsTotals(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}

	const eps = 1e-12
	for name, pair := range map[string][2]float64{
		"cost_usd":            {o.CostUSD, s.CostUSD},
		"baseline_cost_usd":   {o.BaselineCostUSD, s.BaselineCostUSD},
		"cg_llm_cost_usd":     {o.CGLLMCostUSD, s.CGLLMCostUSD},
		"cachesplit_saved":    {o.CachesplitSavedUSD, s.CachesplitSavedUSD},
		"keepalive_ping_usd":  {o.KeepAlivePingUSD, s.KeepAlivePingUSD},
		"keepalive_saved_usd": {o.KeepAliveSavedUSD, s.KeepAliveSavedUSD},
		"net_saved_usd":       {o.NetSavedUSD, s.NetSavedUSD},
		"keepalive_net_usd":   {o.KeepAliveNetUSD, s.KeepAliveNetUSD},
		"total_saved_usd":     {o.TotalSavedUSD, s.TotalSavedUSD},
	} {
		if math.Abs(pair[0]-pair[1]) > eps {
			t.Errorf("%s: Overview=%.10f SavingsTotals=%.10f disagree", name, pair[0], pair[1])
		}
	}
	if s.TotalSavedUSD == 0 {
		t.Fatal("fixture produced no savings at all — test proves nothing")
	}
}

// TestSavingsTotalsScopesBySession pins the reason SavingsTotals takes a Filter at all:
// /stats' "current" and "live" scopes depend on Session actually narrowing the query.
func TestSavingsTotalsScopesBySession(t *testing.T) {
	db := openTestDB(t)
	evs := []*Event{
		{TS: 1, SessionID: "a", Model: "m", TokensBefore: 100, TokensAfter: 90,
			CostUSD: 0.01, BaselineCostUSD: 0.02},
		{TS: 2, SessionID: "b", Model: "m", TokensBefore: 100, TokensAfter: 80,
			CostUSD: 0.01, BaselineCostUSD: 0.03},
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	sa, err := db.SavingsTotals(Filter{TenantAll: true, Session: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sa.NetSavedUSD-0.01) > 1e-12 {
		t.Errorf("session a: net_saved_usd = %.10f, want 0.01", sa.NetSavedUSD)
	}
	all, err := db.SavingsTotals(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(all.NetSavedUSD-0.03) > 1e-12 {
		t.Errorf("all sessions: net_saved_usd = %.10f, want 0.03", all.NetSavedUSD)
	}
}

// TestSavingsTotalsSessionInSumsAcrossSessionsInOneQuery pins the point of SessionIn: a
// caller summing several sessions (/stats' "live" scope, for every keep-alive-tracked
// session) gets one grouped query rather than one SavingsTotals call per session — the
// result must equal a's and b's totals added together, and must exclude session c.
func TestSavingsTotalsSessionInSumsAcrossSessionsInOneQuery(t *testing.T) {
	db := openTestDB(t)
	evs := []*Event{
		{TS: 1, SessionID: "a", Model: "m", TokensBefore: 100, TokensAfter: 90,
			CostUSD: 0.01, BaselineCostUSD: 0.02},
		{TS: 2, SessionID: "b", Model: "m", TokensBefore: 100, TokensAfter: 80,
			CostUSD: 0.01, BaselineCostUSD: 0.03},
		{TS: 3, SessionID: "c", Model: "m", TokensBefore: 100, TokensAfter: 70,
			CostUSD: 0.01, BaselineCostUSD: 0.05},
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	got, err := db.SavingsTotals(Filter{TenantAll: true, SessionIn: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.NetSavedUSD-0.03) > 1e-12 {
		t.Errorf("session_id IN (a,b): net_saved_usd = %.10f, want 0.03 (a's 0.01 + b's 0.02)",
			got.NetSavedUSD)
	}
}

// TestSavingsTotalsEmptyIsZeroNotError pins the fail-open shape callers depend on: an
// empty (or non-matching) filter is a valid, zero-valued result, never an error.
func TestSavingsTotalsEmptyIsZeroNotError(t *testing.T) {
	db := openTestDB(t)
	s, err := db.SavingsTotals(Filter{TenantAll: true, Session: "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if *s != (SavingsTotals{}) {
		t.Errorf("no matching rows: %+v, want all zero", s)
	}
}
