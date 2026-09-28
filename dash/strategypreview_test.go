package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// previewExpiry is kaExpiry (dash/keepalivetab_test.go) with a caller-chosen model and tenant,
// since the model-compatibility checks below need more than one model in the fixture.
func previewExpiry(ts int64, tenantID, session, model string, gapS float64, cost float64, prevPrefix int64) []*Event {
	prev := mkEvent(ts-int64(gapS*1000), session, model, 0, 0)
	prev.TenantID, prev.CacheRead, prev.CacheWrite, prev.CostUSD = tenantID, prevPrefix, 0, 0.05
	miss := mkEvent(ts, session, model, 0, 0)
	miss.TenantID, miss.CacheMissReason, miss.CacheRead, miss.CacheWrite, miss.CostUSD =
		tenantID, "ttl_expiry", 0, prevPrefix, cost
	return []*Event{prev, miss}
}

// previewOpenSpan is a session with a first (ignored, turn 0) request and a SECOND, final
// request above the ping gate's own prefix floor (kaGateMinPrefix, 20000) that nothing ever
// follows — an "open" span (see pingSpan.open): a live keep-alive sends its full K pings on
// it and it can never become an addressable credit, because there is no third request for a
// miss to resume on. This is the shape a per-tenant harm case is actually made of: ping cost
// paid broadly, benefit realised rarely.
func previewOpenSpan(ts int64, tenantID, session, model string) []*Event {
	first := mkEvent(ts, session, model, 0, 0)
	first.TenantID, first.CacheRead, first.CacheWrite = tenantID, 1000, 0
	final := mkEvent(ts+10_000, session, model, 0, 0)
	final.TenantID, final.CacheRead, final.CacheWrite, final.CostUSD = tenantID, 25_000, 0, 0.01
	return []*Event{first, final}
}

// doPreview calls the route through the real mux, exactly as a browser would, and decodes into
// the typed result — this route has no manager gate to satisfy in single-tenant mode (see
// requireManager's own doc comment: auth == nil makes it a no-op), so no session setup is
// needed.
func doPreview(t *testing.T, a *API, candidate string) (*httptest.ResponseRecorder, StrategyPreviewResult) {
	t.Helper()
	m := http.NewServeMux()
	a.Mount(m)
	req := httptest.NewRequest(http.MethodGet,
		"/api/keepalive/strategies/preview?candidate="+url.QueryEscape(candidate), nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	var out StrategyPreviewResult
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding the preview response: %v (%s)", err, w.Body.String())
		}
	}
	return w, out
}

// A candidate with no idle_seconds or max_pings is refused, not silently defaulted — this
// preview is never allowed to guess at a config nobody actually asked to preview.
func TestStrategyPreviewRefusesAnEmptyCandidate(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	w, _ := doPreview(t, a, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400: %s", w.Code, w.Body)
	}
}

// A candidate with no tenant_ids at all previews nothing — an empty result, not an error —
// since there is nothing named to replay.
func TestStrategyPreviewWithNoTenantsIsEmptyNotAnError(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	w, out := doPreview(t, a, `{"idle_seconds":280,"max_pings":2}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body)
	}
	if len(out.Cohorts) != 0 {
		t.Errorf("cohorts = %+v, want none", out.Cohorts)
	}
	if len(out.Caveats) == 0 {
		t.Error("no caveats on the response — uncertainty must always be disclosed, even here")
	}
}

// A tenant with no addressable history at all still gets a cohort row (so a caller can tell
// "no data" from "not named"), but it carries no dollar figures and cannot be harmed.
func TestStrategyPreviewTenantWithNoHistory(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	w, out := doPreview(t, a, `{"idle_seconds":280,"max_pings":2,"tenant_ids":["ghost"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	if len(out.Cohorts) != 1 || out.Cohorts[0].TenantID != "ghost" {
		t.Fatalf("cohorts = %+v", out.Cohorts)
	}
	if out.Cohorts[0].Priced || out.Cohorts[0].Harmed {
		t.Errorf("a tenant with no history was priced or marked harmed: %+v", out.Cohorts[0])
	}
}

// The core bracket: a tenant with one real, priced addressable rescue gets a low/high net
// range rather than one number, and the low bound never exceeds the high one.
func TestStrategyPreviewReportsAnUncertaintyBracketNotAPointEstimate(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	seed(t, rec, previewExpiry(2_000_000, "t1", "s1", "aws/claude-sonnet-5", 20*60, 0.30, 40_000)...)
	w, out := doPreview(t, a, `{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	if len(out.Cohorts) != 1 {
		t.Fatalf("cohorts = %+v", out.Cohorts)
	}
	c := out.Cohorts[0]
	if !c.Priced {
		t.Fatalf("cohort not priced: %+v", c)
	}
	if c.NetUSDLow > c.NetUSDHigh {
		t.Errorf("net_usd_low (%v) > net_usd_high (%v) — the conservative bound must never "+
			"exceed the generous one", c.NetUSDLow, c.NetUSDHigh)
	}
	if c.EffectiveMaxPings != 2 {
		t.Errorf("effective_max_pings = %d, want the candidate's own 2 (no override for t1)", c.EffectiveMaxPings)
	}
}

// The per-tenant off-switch: a tenant named Off in tenant_caps is never priced and never
// counted toward the totals, even though it is still listed.
func TestStrategyPreviewPerTenantOffSwitch(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	seed(t, rec, previewExpiry(2_000_000, "t1", "s1", "aws/claude-sonnet-5", 20*60, 0.30, 40_000)...)
	w, out := doPreview(t, a,
		`{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"],"tenant_caps":{"t1":{"off":true}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	c := out.Cohorts[0]
	if !c.Off {
		t.Error("off was not reported")
	}
	// Priced/DecisionPoints stay informational (a manager can still see this tenant's own
	// history and scale) — only the DOLLAR figures a live "off" strategy would never spend
	// or credit are suppressed.
	if c.PingUSD != 0 || c.NetUSDLow != 0 || c.NetUSDHigh != 0 || c.Harmed {
		t.Errorf("an off tenant carries a dollar figure or is marked harmed: %+v", c)
	}
	if out.TotalPingUSD != 0 {
		t.Errorf("total_ping_usd = %v, want 0 — an off tenant must not contribute", out.TotalPingUSD)
	}
}

// The per-tenant MaxPings override changes which ladder rung this tenant is previewed at.
func TestStrategyPreviewPerTenantMaxPingsOverride(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	seed(t, rec, previewExpiry(2_000_000, "t1", "s1", "aws/claude-sonnet-5", 20*60, 0.30, 40_000)...)
	_, out := doPreview(t, a,
		`{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"],"tenant_caps":{"t1":{"max_pings":8}}}`)
	if out.Cohorts[0].EffectiveMaxPings != 8 {
		t.Errorf("effective_max_pings = %d, want the override (8)", out.Cohorts[0].EffectiveMaxPings)
	}
}

// The advisory per-tenant $ budget: a tenant whose ping cost at the effective rung exceeds
// max_usd_per_tenant is flagged, and one comfortably under it is not — see
// dash/strategypreview.go's own comment on why this is advisory (checked here, never
// live-enforced on the ping path itself).
func TestStrategyPreviewOverTenantBudget(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	seed(t, rec, previewExpiry(2_000_000, "t1", "s1", "aws/claude-sonnet-5", 20*60, 0.30, 40_000)...)

	_, tight := doPreview(t, a,
		`{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"],"max_usd_per_tenant":0.000001}`)
	if !tight.Cohorts[0].OverTenantBudget {
		t.Errorf("a budget far below the ping cost was not flagged: %+v", tight.Cohorts[0])
	}

	_, loose := doPreview(t, a,
		`{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"],"max_usd_per_tenant":1000}`)
	if loose.Cohorts[0].OverTenantBudget {
		t.Errorf("a budget far above the ping cost was flagged: %+v", loose.Cohorts[0])
	}

	// 0 (the default) means "no budget declared" — never flagged, regardless of cost.
	_, none := doPreview(t, a, `{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"]}`)
	if none.Cohorts[0].OverTenantBudget {
		t.Error("max_usd_per_tenant=0 (undeclared) was treated as a budget of zero")
	}
}

// A tenant whose only pingable spans are session-final ones that never resume is exactly
// the shape the study found losing money: ping cost paid broadly, credit realised never.
// Even the generous bound (net_usd_high) must go negative, and the tenant must be flagged.
func TestStrategyPreviewFlagsAHarmedTenant(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	var evs []*Event
	for i := 0; i < 8; i++ {
		evs = append(evs, previewOpenSpan(int64(1_000_000+i*10_000_000), "loser",
			"sess-"+string(rune('a'+i)), "aws/claude-sonnet-5")...)
	}
	// One tiny real rescue elsewhere, so the tenant is priced at all.
	evs = append(evs, previewExpiry(9_000_000, "loser", "s-rescue", "aws/claude-sonnet-5",
		20*60, 0.001, 20_000)...)
	seed(t, rec, evs...)
	w, out := doPreview(t, a, `{"idle_seconds":280,"max_pings":24,"tenant_ids":["loser"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	c := out.Cohorts[0]
	if !c.Priced {
		t.Fatalf("cohort not priced: %+v", c)
	}
	if c.NetUSDHigh > 0 {
		t.Errorf("net_usd_high = %v, want <= 0 — this tenant's cost must dominate even the "+
			"generous bound", c.NetUSDHigh)
	}
	if !c.Harmed {
		t.Error("a tenant whose generous bound is negative was not flagged harmed")
	}
	if len(out.HarmedTenants) != 1 || out.HarmedTenants[0] != "loser" {
		t.Errorf("harmed_tenants = %v, want [\"loser\"]", out.HarmedTenants)
	}
}

// Model compatibility is exact, not a substring: a tenant on a DIFFERENT model than the
// candidate names is reported at 0% compatible, not silently matched.
func TestStrategyPreviewModelCompatibility(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	opus := mkEvent(1000, "s1", "aws/claude-opus-5", 100, 90)
	opus.TenantID = "t1"
	sonnet := mkEvent(2000, "s2", "aws/claude-sonnet-5", 100, 90)
	sonnet.TenantID = "t1"
	evs := []*Event{opus, sonnet}
	// One addressable rescue, so the tenant is priced and reaches the model check at all.
	evs = append(evs, previewExpiry(3_000_000, "t1", "s3", "aws/claude-sonnet-5", 20*60, 0.05, 30_000)...)
	seed(t, rec, evs...)

	w, out := doPreview(t, a,
		`{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"],"models":["aws/claude-opus-5"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	c := out.Cohorts[0]
	if c.ModelCompatiblePct == nil {
		t.Fatal("model_compatible_pct was omitted with a non-empty Models list")
	}
	// 1 of 4 non-keepalive rows (the opus request) is on the compatible model.
	if *c.ModelCompatiblePct < 24 || *c.ModelCompatiblePct > 26 {
		t.Errorf("model_compatible_pct = %v, want ~25", *c.ModelCompatiblePct)
	}

	_, out2 := doPreview(t, a, `{"idle_seconds":280,"max_pings":2,"tenant_ids":["t1"]}`)
	if out2.Cohorts[0].ModelCompatiblePct != nil {
		t.Error("model_compatible_pct was set with an empty Models list, where it says nothing")
	}
}
