package proxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// kaMaxPingsMiss records one addressable ttl_expiry pair (a prior request establishing
// prefix tokens of cached prefix, then a miss that re-created it after gapS seconds idle) on
// tenantID/session — the same shape dash's own kaExpiry test fixture builds, reproduced here
// because this package's own fixture (mgrFixture) records through the real HTTP-adjacent
// f.record helper rather than dash's direct DB writer.
func kaMaxPingsMiss(f *mgrFixture, t *testing.T, tenantID, session string, ts, gapS int64,
	prefix int64, cost float64) {
	t.Helper()
	f.record(t, tenantID, session, &dash.Event{
		TS: ts - gapS*1000, Model: "aws/claude-sonnet-5", CacheRead: prefix, CacheWrite: 0,
		CostUSD: 0.01,
	})
	f.record(t, tenantID, session, &dash.Event{
		TS: ts, Model: "aws/claude-sonnet-5", CacheRead: 0, CacheWrite: prefix,
		CacheMissReason: "ttl_expiry", CostUSD: cost,
	})
}

// The per-tenant max_pings summary must never blend tenants into one curve — that is the
// exact defect PHASE2.md's finding P2-4 found in the pooled recommendation this replaces.
// One tenant with plenty of quickly-reachable expiries (healthy: profitable, not thin), one
// with almost no history (thin: too little data to trust), and one whose expiries are ALL
// past reach of even the widest rung (off: no max_pings pays for it) — the three states the
// panel exists to tell apart.
func TestCtlKeepAliveMaxPingsByTenantSeparatesHealthyThinAndOffTenants(t *testing.T) {
	f := newMgrFixture(t)
	f.h.opts.Prices = fixedPricer{}
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	// Registered directly through the registry, not /api/register: this test needs three
	// MORE tenants purely to attach traffic to, and /api/register's own 3/minute-per-address
	// limiter (control.go's registrationsPerMinute) would refuse the 4th HTTP signup in one
	// test process — a limiter this test has no reason to exercise.
	healthyT, _, err := f.reg.Register("healthy", "healthy@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	thinT, _, err := f.reg.Register("thin", "thin@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	offT, _, err := f.reg.Register("off", "off@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	healthy, thin, off := healthyT.ID, thinT.ID, offT.ID

	ts := int64(1_800_000_000_000)
	// healthy: 25 expiries, each with a 400s gap — comfortably inside K=1's 580s coverage, so
	// more pings only add cost with no more reach. Clears dash.KeepAliveMinDecisionPoints (20)
	// on its own; the filler traffic below clears dash.KeepAliveMinRequests (200), the SAME
	// two floors dash.KeepAliveRecommend already gates on for the same reason.
	for i := 0; i < 25; i++ {
		kaMaxPingsMiss(f, t, healthy, "s"+string(rune('a'+i)), ts+int64(i)*3_600_000, 400,
			50_000, 1.00)
	}
	for i := 0; i < 180; i++ {
		f.record(t, healthy, "filler", &dash.Event{
			TS: ts + 100_000_000 + int64(i)*1000, Model: "aws/claude-sonnet-5", CostUSD: 0.01,
		})
	}
	// thin: 3 expiries only, same shape as healthy's — below the floor regardless of shape.
	for i := 0; i < 3; i++ {
		kaMaxPingsMiss(f, t, thin, "t"+string(rune('a'+i)), ts+int64(i)*3_600_000, 400,
			50_000, 1.00)
	}
	// off: 5 expiries, each with a 10,000s gap — past K=24's own 7,020s coverage
	// (24*280+300), so NOTHING converts at any rung while every rung still pays for K pings.
	for i := 0; i < 5; i++ {
		kaMaxPingsMiss(f, t, off, "o"+string(rune('a'+i)), ts+int64(i)*3_600_000, 10_000,
			50_000, 1.00)
	}

	w, out := f.do(t, "GET", "/api/keepalive/strategies/max-pings-by-tenant", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	rows, _ := out["tenants"].([]any)
	byTenant := map[string]map[string]any{}
	tenantOrder := make([]string, 0, len(rows))
	for _, r := range rows {
		row := r.(map[string]any)
		id := row["tenant_id"].(string)
		byTenant[id] = row
		tenantOrder = append(tenantOrder, id)
	}

	// Worst first: the off-recommended tenant must sort ahead of the profitable one — the
	// specific ordering that puts the case a pooled curve would have hidden at the top,
	// rather than wherever the tenant happened to land in the registry's own listing.
	offIdx, healthyIdx := -1, -1
	for i, id := range tenantOrder {
		if id == off {
			offIdx = i
		}
		if id == healthy {
			healthyIdx = i
		}
	}
	if offIdx < 0 || healthyIdx < 0 || offIdx > healthyIdx {
		t.Errorf("tenant order = %v, want the off-recommended tenant (%s) ahead of the "+
			"profitable one (%s)", tenantOrder, off, healthy)
	}

	h, ok := byTenant[healthy]
	if !ok {
		t.Fatal("the healthy tenant is missing from the response")
	}
	if h["thin_data"] != false {
		t.Errorf("healthy tenant thin_data = %v, want false (25 >= the 20-decision-point floor)",
			h["thin_data"])
	}
	if h["priced"] != true {
		t.Fatalf("healthy tenant priced = %v, want true", h["priced"])
	}
	if net, _ := h["optimal_net_usd"].(float64); net <= 0 {
		t.Errorf("healthy tenant optimal_net_usd = %v, want positive — every expiry converts "+
			"at K=1 for one ping each", net)
	}
	if h["off_recommended"] != false {
		t.Error("healthy tenant off_recommended = true, want false — it has a profitable rung")
	}

	th, ok := byTenant[thin]
	if !ok {
		t.Fatal("the thin tenant is missing from the response")
	}
	if th["thin_data"] != true {
		t.Errorf("thin tenant thin_data = %v, want true (3 decision points, floor is 20)",
			th["thin_data"])
	}

	o, ok := byTenant[off]
	if !ok {
		t.Fatal("the off tenant is missing from the response")
	}
	if o["off_recommended"] != true {
		t.Errorf("off tenant off_recommended = %v, want true — every gap is past even K=24's "+
			"own coverage, so no rung ever converts anything while every rung still pings",
			o["off_recommended"])
	}
	if net, _ := o["optimal_net_usd"].(float64); net >= 0 {
		t.Errorf("off tenant optimal_net_usd = %v, want negative", net)
	}
	if k, _ := o["optimal_max_pings"].(float64); k != 1 {
		t.Errorf("off tenant optimal_max_pings = %v, want 1 (fewest pings is least-bad when "+
			"nothing converts at any K)", k)
	}
}

// A non-manager must be refused, same as every other route in this table —
// TestEveryControlRouteEnforcesItsScope already walks the declared scope; this is the
// behavioural half of that promise for this specific route.
func TestCtlKeepAliveMaxPingsByTenantRefusesANonManager(t *testing.T) {
	f := newMgrFixture(t)
	_, _ = f.signUpJar(t, "boss@ibm.com") // first sign-up becomes the manager
	plainJar, _ := f.signUpJar(t, "plain@ibm.com")
	w, _ := f.do(t, "GET", "/api/keepalive/strategies/max-pings-by-tenant", "", plainJar)
	if w.Code != http.StatusForbidden {
		t.Fatalf("get as non-manager = %d, want 403", w.Code)
	}
}
