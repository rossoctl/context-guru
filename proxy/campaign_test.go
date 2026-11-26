package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/tenant"
)

// Every activatable, non-baseline arm's own live parameters must actually clear
// validStrategyBounds — a static safety net for the hardcoded constants in
// campaign.ArmFor, so a future edit that pushes one out of band fails a test instead of
// failing a manager's create call.
func TestCampaignArmForStaysWithinValidStrategyBounds(t *testing.T) {
	arms := []string{
		kvcache.StrategyKeepAlive5m, kvcache.StrategyKeepAlive5mOnce,
		kvcache.StrategyStopReasonGated, kvcache.StrategyKeepAlive1h, kvcache.StrategyKeepAlive1hOnce,
	}
	for _, arm := range arms {
		a := campaign.ArmFor(arm)
		if !a.Activatable || a.Baseline {
			t.Fatalf("%s: expected activatable, non-baseline", arm)
		}
		idle := time.Duration(a.IdleSeconds) * time.Second
		if err := validStrategyBounds(idle, a.MaxPings, config.DefaultKeepAliveMinPrefix, 0,
			a.HeadTTL1h, a.HeadTTLMinTokens); err != nil {
			t.Errorf("%s: %+v fails validStrategyBounds: %v", arm, a, err)
		}
	}
}

// A duplicate (tenant, hour) cell in an uploaded payload must be refused before any
// strategy is created for it — not left to surface only after tenant.CreateCampaign's
// own persistence-time guard, by which point the create-strategy loop would already
// have committed real, live keepalive_strategies rows for every other coalesced group.
func TestCtlCreateCampaignRejectsDuplicateCellBeforeCreatingAnyStrategy(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
			// Same (tenant, hour) again, a different arm — a plausible hand-edit artifact.
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5mOnce,
				SavingUSD: 1.00, BaselineUSD: 2.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "dup", "source": "upload", "suggest": suggest})
	w, _ := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d %s, want 400", w.Code, w.Body)
	}
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 0 {
		t.Fatalf("got %d strategies, want 0 — a rejected duplicate-cell upload must create none at all, "+
			"not create-then-roll-back", len(strategies))
	}
}

// validateGroupStrategy is defense in depth for ctlCreateCampaign's group-creation
// loop: a campaign.StrategyForGroup result built from valid inputs must pass; one built from a
// deliberately-broken window (bypassing campaign.TileHours entirely, the way a future bug in
// this package's own logic might) must be caught here rather than persisted.
func TestValidateGroupStrategyCatchesAnInvalidWindow(t *testing.T) {
	valid := campaign.StrategyForGroup("camp", &campaign.Group{
		Arm: kvcache.StrategyKeepAlive5m, Config: campaign.ArmFor(kvcache.StrategyKeepAlive5m),
		HourSet: []int{9}, TenantIDs: []string{"t1"},
	}, nil)
	if err := validateGroupStrategy(valid); err != nil {
		t.Errorf("a normally-built strategy failed validation: %v", err)
	}

	broken := valid
	broken.Windows = []tenant.Window{{Start: "09:00", End: "25:00"}}
	if err := validateGroupStrategy(broken); err == nil {
		t.Error("a strategy with an invalid window passed validation")
	}

	noTarget := valid
	noTarget.Target = tenant.Target{Mode: tenant.TargetList}
	if err := validateGroupStrategy(noTarget); err == nil {
		t.Error("a strategy with an empty list-target passed validation")
	}
}

// The full create flow, end to end, through the real HTTP route: an uploaded suggest
// payload with two tenants sharing an arm at the same hour, an empty-tenant cell that
// must be excluded, and a simulation-only arm that must be recorded but not activated.
func TestCtlCreateCampaignEndToEnd(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")
	_, tenantB := f.signUpJar(t, "b@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Baseline: "fixed-5m",
		Weekdays: []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday"},
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 42, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.23, BaselineUSD: 4.56},
			{User: tenantB, HourUTC: 9, Requests: 12, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 0.50, BaselineUSD: 2.00},
			{User: tenantA, HourUTC: 10, Requests: 3, BestStrategy: kvcache.StrategyStickySession1h,
				InsufficientData: true},
			// Must be excluded entirely: an ambiguous, unsafe campaign target.
			{User: "", HourUTC: 11, Requests: 999, BestStrategy: kvcache.StrategyKeepAlive5m},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "rollout-1", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := out["id"].(string)
	if campaignID == "" {
		t.Fatal("no campaign id returned")
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 3 {
		t.Fatalf("got %d cells, want 3 (the empty-user cell must be excluded): %v", len(cells), cells)
	}

	// tenantA and tenantB share the same arm at the same hour -> one coalesced
	// strategy targeting both, at keepalive-5m's live parameters.
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 1 {
		t.Fatalf("got %d strategies, want 1 (coalesced): %+v", len(strategies), strategies)
	}
	s := strategies[0]
	if s.IdleSeconds != 240 || s.MinPrefixTokens != 80000 || len(s.Target.TenantIDs) != 2 || s.Target.Mode != tenant.TargetList {
		t.Errorf("strategy = %+v, want idle 240s, 80k floor, targeting both tenants", s)
	}

	if w, _ := f.do(t, "GET", "/api/keepalive/campaigns", "", mgrJar); w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}

	w, out = f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	detailCells, _ := out["cells"].([]any)
	if len(detailCells) != 3 {
		t.Fatalf("got %d cells, want 3", len(detailCells))
	}
	foundSkipped := false
	for _, c := range detailCells {
		m := c.(map[string]any)
		if m["arm"] == kvcache.StrategyStickySession1h {
			foundSkipped = true
			if activatable, _ := m["activatable"].(bool); activatable {
				t.Errorf("sticky-session-1h cell = %v, want not activatable", m)
			}
			if reason, _ := m["skip_reason"].(string); reason == "" {
				t.Errorf("sticky-session-1h cell = %v, want a skip reason", m)
			}
		}
	}
	if !foundSkipped {
		t.Error("the sticky-session-1h cell was not recorded at all")
	}

	w, out = f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID+"/tenants/"+tenantA, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant drilldown = %d %s", w.Code, w.Body)
	}
	tCells, _ := out["cells"].([]any)
	if len(tCells) != 2 {
		t.Fatalf("got %d cells for tenant A, want 2 (hour 9 and hour 10)", len(tCells))
	}
}

// rollbackCampaignStrategies must reload the keeper after deleting, not only the
// registry's own row: campaign.StrategyForGroup marks a created strategy Active immediately, so
// it is already matchable to live traffic the instant it exists in the keeper's
// in-memory list — deleting it from SQLite without evicting that in-memory copy would
// leave it pinging/toggling real traffic until some unrelated strategy mutation
// happened to trigger the next reload.
func TestRollbackCampaignStrategiesReloadsTheKeeperNotJustTheRegistry(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "rollback-test", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	strategyID, _ := cells[0].(map[string]any)["strategy_id"].(string)
	if strategyID == "" {
		t.Fatal("no strategy id created")
	}

	inKeeper := func() bool {
		f.h.keeper.mu.Lock()
		defer f.h.keeper.mu.Unlock()
		for _, s := range f.h.keeper.strategies {
			if s.ID == strategyID {
				return true
			}
		}
		return false
	}
	if !inKeeper() {
		t.Fatal("the created strategy is not yet visible to the keeper (create should load it)")
	}

	f.h.rollbackCampaignStrategies([]tenant.Strategy{{ID: strategyID}})

	if inKeeper() {
		t.Error("the rolled-back strategy is still in the keeper's in-memory list — " +
			"it would keep matching live traffic despite being deleted")
	}
}

// An empty-user suggest payload creates nothing to campaign over — a clear 400, not a
// campaign with zero cells.
func TestCtlCreateCampaignAllCellsExcludedIsRefused(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{{User: "", HourUTC: 9, BestStrategy: kvcache.StrategyKeepAlive5m}},
	}
	body, _ := json.Marshal(map[string]any{"name": "empty", "source": "upload", "suggest": suggest})
	w, _ := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d %s, want 400", w.Code, w.Body)
	}
}

// The campaign detail route's per-tenant aggregate sums frozen predicted $ from the
// cells alongside real $ read live from dash — the wiring TestCampaignRealSavings*
// (dash package) already covers at the query level; this covers that ctlGetCampaign
// actually reaches it and sums it correctly across tenants.
func TestCtlGetCampaignAggregatesPredictedAndRealPerTenant(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.50, BaselineUSD: 3.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "agg-test", "source": "upload", "suggest": suggest})
	w, created := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := created["id"].(string)
	createdCells, _ := created["cells"].([]any)
	strategyID, _ := createdCells[0].(map[string]any)["strategy_id"].(string)
	if strategyID == "" {
		t.Fatalf("created cell has no strategy id: %v", createdCells)
	}

	f.record(t, tenantA, "s1", &dash.Event{
		KeepAlive: true, KeepAliveStrategyID: strategyID, CostUSD: 0.02,
		CacheRead: 40_000, Model: "aws/claude-sonnet-5", TokenAccounting: dash.AccountingComplete,
	})
	// The credited row carries the same strategy id its rescuing ping did — which is what
	// makes it THIS campaign's saving rather than merely this tenant's. Before
	// dash.CampaignRealSavings scoped the credited half by strategy id, an untagged row
	// here was collected all the same, so this test passed while proving something weaker
	// than it claimed: that a campaign picks up any credit its tenant happens to have.
	f.record(t, tenantA, "s1", &dash.Event{
		KeepAliveSavedUSD: 0.10, KeepAliveStrategyID: strategyID,
		Model: "aws/claude-sonnet-5", TokenAccounting: dash.AccountingComplete,
	})
	// A second credited row for the same tenant, under a strategy this campaign does NOT
	// own — the manually-created strategies this deployment already runs alongside any
	// campaign. It must not reach real_saved_usd below.
	f.record(t, tenantA, "s1", &dash.Event{
		KeepAliveSavedUSD: 5.00, KeepAliveStrategyID: "someone-elses-strategy",
		Model: "aws/claude-sonnet-5", TokenAccounting: dash.AccountingComplete,
	})

	out := f.waitForCampaignRealSaved(t, mgrJar, campaignID, tenantA)
	tenants, _ := out["tenants"].([]any)
	if len(tenants) != 1 {
		t.Fatalf("got %d tenant summaries, want 1: %v", len(tenants), tenants)
	}
	row := tenants[0].(map[string]any)
	if row["tenant_id"] != tenantA {
		t.Errorf("tenant_id = %v, want %v", row["tenant_id"], tenantA)
	}
	if p, _ := row["predicted_usd"].(float64); p < 1.49 || p > 1.51 {
		t.Errorf("predicted_usd = %v, want ~1.50", row["predicted_usd"])
	}
	if s, _ := row["real_saved_usd"].(float64); s < 0.099 || s > 0.101 {
		t.Errorf("real_saved_usd = %v, want ~0.10 — this campaign's own strategy only, not "+
			"the $5.10 of credit this tenant has in total", row["real_saved_usd"])
	}
	if total, _ := out["total_predicted_usd"].(float64); total < 1.49 || total > 1.51 {
		t.Errorf("total_predicted_usd = %v, want ~1.50", out["total_predicted_usd"])
	}
	if out["caveat"] == "" || out["caveat"] == nil {
		t.Error("the attribution caveat was not carried onto the response")
	}
}

// waitForCampaignRealSaved polls the campaign detail route until the tenant's aggregate
// reports a real saving, and returns that response. The record helper waits for each ROW
// to persist; real_saved_usd is computed over several of them at read time, and #163
// caught it reading 0 on a starved 2-core CI runner with every row persisted. Waiting for
// the answer the assertion needs, with a loud timeout, keeps the assertion meaningful — a
// wrong value still fails it — while removing the load sensitivity.
func (f *mgrFixture) waitForCampaignRealSaved(t *testing.T, jar []*http.Cookie, campaignID, tenantID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, out := f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID, "", jar)
		if w.Code != http.StatusOK {
			t.Fatalf("get = %d %s", w.Code, w.Body)
		}
		tenants, _ := out["tenants"].([]any)
		var row map[string]any
		for _, tn := range tenants {
			if r, _ := tn.(map[string]any); r["tenant_id"] == tenantID {
				row = r
			}
		}
		if row == nil {
			t.Fatalf("campaign %s has no summary for %s: %v", campaignID, tenantID, tenants)
		}
		if s, _ := row["real_saved_usd"].(float64); s != 0 {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("real_saved_usd for %s stayed 0 for 5s — the aggregate never saw the "+
				"credited row:\n%v", tenantID, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A genuinely zero real-saving RATE (real traffic happened, but none of it was
// credited that hour) must render as 0 on the wire, not be omitted as though it were
// never computed at all — the *float64 fields exist specifically so a caller can tell
// "$0 per 1k requests" apart from "no requests to divide by."
func TestCtlGetCampaignTenantDrilldownRendersAGenuineZeroRateNotAnOmittedOne(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "zero-rate", "source": "upload", "suggest": suggest})
	w, created := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := created["id"].(string)

	// Real traffic in the same hour, but none of it credited any keep-alive saving —
	// Requests/ActiveDays must be nonzero while SavedUSD (and so the normalized rates)
	// are genuinely, computably zero.
	f.record(t, tenantA, "s1", &dash.Event{
		TS: futureHourUTC(9), Model: "aws/claude-sonnet-5", TokenAccounting: dash.AccountingComplete,
	})

	w, out := f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID+"/tenants/"+tenantA, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant drilldown = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1", len(cells))
	}
	c := cells[0].(map[string]any)
	if reqs, _ := c["real_requests"].(float64); reqs != 1 {
		t.Fatalf("real_requests = %v, want 1", c["real_requests"])
	}
	per1k, ok := c["real_saved_usd_per_1k_requests"]
	if !ok || per1k == nil {
		t.Errorf("real_saved_usd_per_1k_requests is absent, want a present, genuinely-zero rate: %v", c)
	} else if v, _ := per1k.(float64); v != 0 {
		t.Errorf("real_saved_usd_per_1k_requests = %v, want 0", v)
	}
	perDay, ok := c["real_saved_usd_per_active_day"]
	if !ok || perDay == nil {
		t.Errorf("real_saved_usd_per_active_day is absent, want a present, genuinely-zero rate: %v", c)
	} else if v, _ := perDay.(float64); v != 0 {
		t.Errorf("real_saved_usd_per_active_day = %v, want 0", v)
	}
}

// The 1h-tier model-honoring gate must work with the REAL, DB-backed check
// (tenantHonorsHeadTTL1h), not just the fake closures every other test injects directly
// into campaign.ResolveCell. This is the actual mechanism that decides whether a
// keepalive-1h/1h-once cell is safe to activate for a given tenant's real traffic.
func TestCtlCreateCampaignGatesThe1hTierByRealTenantTrafficModel(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, honoredTenant := f.signUpJar(t, "haiku@ibm.com")
	_, downgradedTenant := f.signUpJar(t, "sonnet@ibm.com")

	// aws/claude-haiku-4-5 is the one model this deployment has measured actually
	// honoring the 1h cache tier (see campaign.HeadTTL1hHonored); aws/claude-sonnet-5 is
	// silently downgraded to 5m.
	f.record(t, honoredTenant, "s1", &dash.Event{Model: "aws/claude-haiku-4-5"})
	f.record(t, downgradedTenant, "s1", &dash.Event{Model: "aws/claude-sonnet-5"})

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: honoredTenant, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive1h,
				SavingUSD: 5.00, BaselineUSD: 10.00},
			{User: downgradedTenant, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive1h,
				SavingUSD: 5.00, BaselineUSD: 10.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "1h-gate", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2", len(cells))
	}
	byTenant := map[string]map[string]any{}
	for _, c := range cells {
		m := c.(map[string]any)
		byTenant[m["tenant_id"].(string)] = m
	}
	honored := byTenant[honoredTenant]
	if activatable, _ := honored["activatable"].(bool); !activatable {
		t.Errorf("honored-model tenant's cell = %v, want activatable", honored)
	}
	downgraded := byTenant[downgradedTenant]
	if activatable, _ := downgraded["activatable"].(bool); activatable {
		t.Errorf("downgraded-model tenant's cell = %v, want NOT activatable", downgraded)
	}
	if reason, _ := downgraded["skip_reason"].(string); reason == "" {
		t.Errorf("downgraded-model tenant's cell = %v, want a skip reason", downgraded)
	}

	// The activated strategy must actually carry a real HeadTTLMinTokens, not the zero
	// value that would silently disable the upgrade despite HeadTTL1h being set.
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 1 {
		t.Fatalf("got %d strategies, want 1 (only the honored tenant's)", len(strategies))
	}
	s := strategies[0]
	if !s.HeadTTL1h || s.HeadTTLMinTokens <= 0 {
		t.Errorf("strategy = %+v, want HeadTTL1h=true with a positive HeadTTLMinTokens", s)
	}
	if len(s.Target.TenantIDs) != 1 || s.Target.TenantIDs[0] != honoredTenant {
		t.Errorf("strategy target = %+v, want only the honored tenant", s.Target)
	}
}

// keepalive-5m-once, stop-reason-gated, and keepalive-1h-once were, until now, only
// ever driven through campaign.ArmFor/campaign.ResolveCell directly in unit tests, never
// through the real create-campaign HTTP route with an assertion on every field of the
// resulting tenant.Strategy — precisely the gap that let stop-reason-gated ship with no
// PredictorThreshold at all (a silent zero value that makes predictorFor's gate a
// no-op, so the arm pinged unconditionally, identically to keepalive-5m, despite being
// labeled and billed as the predictor-gated arm) go undetected.
//
// One tenant, three hours, three arms — not one tenant per arm — because self-
// registration is rate-limited to 3/minute per client address (registrationsPerMinute)
// and this fixture has no way to vary the address per signUpJar call; a single tenant
// with cells at different hours resolves into separate, uncoalesced strategies just as
// well, since campaign.Coalesce groups by (tenant, arm) before ever looking at hours.
func TestCtlCreateCampaignEveryArmCarriesItsOwnLiveParameters(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantID := f.signUpJar(t, "many-arms@ibm.com")

	// keepalive-1h-once needs this tenant's traffic to honor the 1h cache tier.
	f.record(t, tenantID, "s1", &dash.Event{Model: "aws/claude-haiku-4-5"})

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantID, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5mOnce,
				SavingUSD: 1.00, BaselineUSD: 2.00},
			{User: tenantID, HourUTC: 10, Requests: 10, BestStrategy: kvcache.StrategyStopReasonGated,
				SavingUSD: 1.00, BaselineUSD: 2.00},
			{User: tenantID, HourUTC: 11, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive1hOnce,
				SavingUSD: 5.00, BaselineUSD: 10.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "every-arm", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	for _, c := range cells {
		m := c.(map[string]any)
		if activatable, _ := m["activatable"].(bool); !activatable {
			t.Errorf("cell %v not activatable, want all three arms activatable here", m)
		}
	}

	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 3 {
		t.Fatalf("got %d strategies, want 3 (one per arm, no coalescing across arms): %+v",
			len(strategies), strategies)
	}
	byIdle := map[int]tenant.Strategy{}
	// keepalive-5m-once and stop-reason-gated share IdleSeconds=280 — disambiguated by
	// PredictorID, which only the latter sets.
	var once5m, gated tenant.Strategy
	for _, s := range strategies {
		if len(s.Target.TenantIDs) != 1 || s.Target.TenantIDs[0] != tenantID {
			t.Fatalf("strategy %+v targets %v, want only %q", s, s.Target.TenantIDs, tenantID)
		}
		byIdle[s.IdleSeconds] = s
		if s.IdleSeconds != 280 {
			continue
		}
		if s.PredictorID != "" {
			gated = s
		} else {
			once5m = s
		}
	}
	if once5m.MaxPings != 1 {
		t.Errorf("keepalive-5m-once strategy = %+v, want idle 280s, max 1 ping", once5m)
	}
	if gated.MaxPings != config.DefaultKeepAliveMaxPings {
		t.Errorf("stop-reason-gated strategy = %+v, want idle 280s, max %d pings",
			gated, config.DefaultKeepAliveMaxPings)
	}
	if gated.PredictorID != "stop-reason-gated" {
		t.Errorf("stop-reason-gated strategy PredictorID = %q, want %q",
			gated.PredictorID, "stop-reason-gated")
	}
	if gated.PredictorThreshold <= 0 {
		t.Errorf("stop-reason-gated strategy PredictorThreshold = %v, want a positive value — "+
			"0 makes the predictor gate a no-op (predictorFor never returns a negative probability)",
			gated.PredictorThreshold)
	}

	hourOnce := byIdle[3360]
	if hourOnce.MaxPings != 1 {
		t.Errorf("keepalive-1h-once strategy MaxPings = %d, want 1", hourOnce.MaxPings)
	}
	if !hourOnce.HeadTTL1h || hourOnce.HeadTTLMinTokens <= 0 {
		t.Errorf("keepalive-1h-once strategy = %+v, want HeadTTL1h=true with a positive HeadTTLMinTokens",
			hourOnce)
	}
}

// A simulation-only arm's frozen PredictedUSD must not inflate the campaign's total: it
// was never attempted, so there is no "real" side it could ever be compared against —
// summing it in would make Predicted and Real describe two different populations of
// cells, with Predicted silently counting arms this deployment could never enforce.
func TestCampaignTenantSummariesOnlyCountsPredictedUSDForActivatedCells(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00, OptimalSavingUSD: 1.50},
			// A big number, but simulation-only: this must be EXCLUDED from the total.
			{User: tenantA, HourUTC: 10, Requests: 10, BestStrategy: kvcache.StrategyHistorical,
				SavingUSD: 1000.00, BaselineUSD: 2000.00, OptimalSavingUSD: 1500.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "mixed", "source": "upload", "suggest": suggest})
	w, created := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := created["id"].(string)

	w, out := f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	total, _ := out["total_predicted_usd"].(float64)
	if total < 0.99 || total > 1.01 {
		t.Errorf("total_predicted_usd = %v, want ~1.00 (the 1000.00 simulation-only cell must be excluded)", total)
	}
	// The oracle ceiling must be scoped to exactly the same population as Predicted — the
	// 1500.00 simulation-only cell's ceiling must be excluded right alongside its saving.
	totalOptimal, _ := out["total_optimal_saving_usd"].(float64)
	if totalOptimal < 1.49 || totalOptimal > 1.51 {
		t.Errorf("total_optimal_saving_usd = %v, want ~1.50 (the 1500.00 simulation-only "+
			"cell's ceiling must be excluded, matching total_predicted_usd's own population)",
			totalOptimal)
	}
	tenants, _ := out["tenants"].([]any)
	if len(tenants) != 1 {
		t.Fatalf("got %d tenant summaries, want 1", len(tenants))
	}
	row := tenants[0].(map[string]any)
	if p, _ := row["predicted_usd"].(float64); p < 0.99 || p > 1.01 {
		t.Errorf("tenant predicted_usd = %v, want ~1.00", p)
	}
	if p, _ := row["optimal_saving_usd"].(float64); p < 1.49 || p > 1.51 {
		t.Errorf("tenant optimal_saving_usd = %v, want ~1.50", p)
	}
}

// An out-of-range hour_utc in an uploaded payload must never become a real, live-but-
// dead strategy: campaign.ResolveCell's bounds check should catch it before it's ever
// coalesced, and validateGroupStrategy is defense in depth in case it somehow weren't.
// End to end, through the real route, on top of the unit tests for each layer.
func TestCtlCreateCampaignOutOfRangeHourIsNotActivatedAsADeadStrategy(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 24, Requests: 42, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.23, BaselineUSD: 4.56},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "bad-hour", "source": "upload", "suggest": suggest})
	w, out := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1", len(cells))
	}
	c := cells[0].(map[string]any)
	if activatable, _ := c["activatable"].(bool); activatable {
		t.Errorf("cell = %v, want not activatable", c)
	}
	if reason, _ := c["skip_reason"].(string); reason == "" {
		t.Errorf("cell = %v, want a skip reason", c)
	}
	if sid, _ := c["strategy_id"].(string); sid != "" {
		t.Errorf("cell = %v, want no strategy id", c)
	}
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 0 {
		t.Fatalf("got %d strategies, want 0 (no dead strategy should have been created): %+v",
			len(strategies), strategies)
	}
}

// futureHourUTC returns an epoch-ms timestamp guaranteed to land in the given UTC hour
// on a day strictly after "now" — so a recorded event's own UTC hour, and its
// after-ActivatedAt-ness, never depend on what wall-clock hour the test happens to run
// at (unlike time.Now().UnixMilli(), which mgrFixture.record defaults to when TS is 0).
func futureHourUTC(hour int) int64 {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, hour, 0, 0, 0, time.UTC).UnixMilli()
}

// Two tenants sharing a coalesced strategy (same arm, same hour) must never see each
// other's real ping/saving numbers in the per-tenant drill-down: the underlying
// CampaignRealSavings query is deliberately not tenant-scoped on its cost half (a
// shared strategy's ping cost legitimately spans every tenant it targets), so the
// caller must filter to the tenant actually being viewed.
//
// tenant B's events are pinned to hour 9 UTC (matching the suggest cells' own
// HourUTC: 9) rather than left to default to time.Now(): without that pin, this test
// only actually exercises the leak it claims to guard when it happens to run during
// UTC hour 9 — any other hour, tenant B's leaked row would land under a different hour
// key than tenant A's cell looks up, and the test would pass vacuously whether or not
// the tenant filter this test guards is even present.
func TestCtlGetCampaignTenantDoesNotLeakAnotherTenantsRealSavings(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")
	_, tenantB := f.signUpJar(t, "b@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
			{User: tenantB, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "shared-strategy", "source": "upload", "suggest": suggest})
	w, created := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := created["id"].(string)
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 1 || len(strategies[0].Target.TenantIDs) != 2 {
		t.Fatalf("want 1 strategy targeting both tenants, got %+v", strategies)
	}
	strategyID := strategies[0].ID

	// Only tenant B pings/gets credited — tenant A has NO real traffic at all. TS pinned
	// to UTC hour 9 (see futureHourUTC's own doc comment) so the leak this test guards
	// is exercised regardless of what hour the suite happens to run at.
	f.record(t, tenantB, "s1", &dash.Event{
		TS: futureHourUTC(9), KeepAlive: true, KeepAliveStrategyID: strategyID, CostUSD: 0.05,
		CacheRead: 40_000, Model: "aws/claude-sonnet-5", TokenAccounting: dash.AccountingComplete,
	})
	f.record(t, tenantB, "s1", &dash.Event{
		TS: futureHourUTC(9), KeepAliveSavedUSD: 0.30, Model: "aws/claude-sonnet-5",
		TokenAccounting: dash.AccountingComplete,
	})

	w, out := f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID+"/tenants/"+tenantA, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant A drilldown = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 1 {
		t.Fatalf("got %d cells for tenant A, want 1", len(cells))
	}
	c := cells[0].(map[string]any)
	if realPings, _ := c["real_pings"].(float64); realPings != 0 {
		t.Errorf("tenant A's cell shows %v real pings — tenant B's activity leaked into it: %v", realPings, c)
	}
	if realSaved, _ := c["real_saved_usd"].(float64); realSaved != 0 {
		t.Errorf("tenant A's cell shows $%v real saved — tenant B's credit leaked into it: %v", realSaved, c)
	}
}

// A tenant with zero activated cells in a campaign must show $0 real saving, not just
// $0 predicted — even when that tenant genuinely has real, credited keep-alive saving
// from something this campaign never created (another campaign, a manually-created
// strategy, or any other pre-existing keep-alive mechanism). Predicted and Real must
// describe the SAME population: a tenant this campaign never activated is a tenant it
// gets no credit for, on either side of the comparison.
func TestCampaignRealSavingsOnlyCoversTenantsThisCampaignActivated(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, tenantA := f.signUpJar(t, "a@ibm.com")
	_, tenantB := f.signUpJar(t, "b@ibm.com")

	suggest := dash.KVCacheSuggestions{
		Cells: []dash.KVCacheSuggestion{
			{User: tenantA, HourUTC: 9, Requests: 10, BestStrategy: kvcache.StrategyKeepAlive5m,
				SavingUSD: 1.00, BaselineUSD: 2.00},
			// Simulation-only: tenant B never gets a strategy from this campaign.
			{User: tenantB, HourUTC: 14, Requests: 10, BestStrategy: kvcache.StrategyHistorical,
				SavingUSD: 1000.00, BaselineUSD: 2000.00},
		},
	}
	body, _ := json.Marshal(map[string]any{"name": "mixed-activation", "source": "upload", "suggest": suggest})
	w, created := f.do(t, "POST", "/api/keepalive/campaigns", string(body), mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	campaignID, _ := created["id"].(string)
	strategies, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(strategies) != 1 {
		t.Fatalf("got %d strategies, want 1 (only tenant A's)", len(strategies))
	}

	// Tenant B has genuine real credited saving, entirely unrelated to this campaign
	// (no strategy of this campaign's ever matched it), landing in hour 14 UTC — the
	// same hour tenant B's own (non-activated) cell names, so a leak would show up
	// exactly where a manager would look for it.
	f.record(t, tenantB, "s1", &dash.Event{
		TS: futureHourUTC(14), KeepAliveSavedUSD: 0.75, Model: "aws/claude-sonnet-5",
		TokenAccounting: dash.AccountingComplete,
	})

	w, out := f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	tenants, _ := out["tenants"].([]any)
	if len(tenants) != 2 {
		t.Fatalf("got %d tenant summaries, want 2 (both tenants still listed)", len(tenants))
	}
	for _, tv := range tenants {
		row := tv.(map[string]any)
		if row["tenant_id"] != tenantB {
			continue
		}
		if p, _ := row["predicted_usd"].(float64); p != 0 {
			t.Errorf("tenant B predicted_usd = %v, want 0 (never activated)", p)
		}
		if s, _ := row["real_saved_usd"].(float64); s != 0 {
			t.Errorf("tenant B real_saved_usd = %v, want 0 — its unrelated real credit must not "+
				"be attributed to a campaign that never activated anything for it", s)
		}
		if n, _ := row["real_net_usd"].(float64); n != 0 {
			t.Errorf("tenant B real_net_usd = %v, want 0", n)
		}
	}

	w, out = f.do(t, "GET", "/api/keepalive/campaigns/"+campaignID+"/tenants/"+tenantB, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant B drilldown = %d %s", w.Code, w.Body)
	}
	cells, _ := out["cells"].([]any)
	if len(cells) != 1 {
		t.Fatalf("got %d cells for tenant B, want 1", len(cells))
	}
	c := cells[0].(map[string]any)
	if realSaved, _ := c["real_saved_usd"].(float64); realSaved != 0 {
		t.Errorf("tenant B's drilldown cell shows $%v real saved despite never being activated: %v",
			realSaved, c)
	}
}

// A K-PING ARM BECOMES A LIVE STRATEGY WITH K PINGS. The campaign must enforce exactly the budget the
// cell was scored at; a strategy left at the default of 2 would run a schedule nobody simulated.
func TestCampaignEnforcesTheKPingArmsBudget(t *testing.T) {
	for k := 1; k <= kvcache.MaxKeepAlivePings; k++ {
		arm := kvcache.StrategyKeepAlive5mPingsPrefix + strconv.Itoa(k)
		a := campaign.ArmFor(arm)
		if !a.Activatable || a.Baseline {
			t.Fatalf("%s: activatable=%v baseline=%v, want a live non-baseline arm", arm, a.Activatable, a.Baseline)
		}
		if a.MaxPings != k {
			t.Errorf("%s enforces %d pings, want %d", arm, a.MaxPings, k)
		}
		if err := validStrategyBounds(time.Duration(a.IdleSeconds)*time.Second, a.MaxPings, a.MinPrefixTokens, 0, false, 0); err != nil {
			t.Errorf("%s builds a strategy the live bounds refuse: %v", arm, err)
		}
	}
	if a := campaign.ArmFor("keepalive-5m-x12"); a.Activatable {
		t.Error("x12 is above the live bound and must not be enforceable")
	}
}
