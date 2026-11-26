package proxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/predictive"
)

func flatPrice(string, int64) (float64, float64, float64, float64, bool) {
	return 3e-6, 15e-6, 3e-7, 3.75e-6, true
}

func TestCacheCampaignAutoBuildRunsInProcess(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent") // the auto builder must not need the Python pipeline
	mgr, _ := f.signUpJar(t, "boss@ibm.com")
	_, tid := f.signUpJar(t, "user@ibm.com")
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"tenant","id":"`+tid+`","builder":"magic"}`, mgr); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown builder = %d, want 400", w.Code)
	}
	ts := time.Now().Add(-3 * time.Hour).UnixMilli()
	f.record(t, tid, "s1", &dash.Event{TS: ts, Model: "m1", Status: 200, CacheRead: 50000, UpstreamMs: 1000})
	f.record(t, tid, "s1", &dash.Event{TS: ts + 400e3, Model: "m1", Status: 200, CacheRead: 50000, UpstreamMs: 1000})
	w, out := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"tenant","id":"`+tid+`","builder":"auto","session_end":true}`, mgr)
	if w.Code != http.StatusAccepted || out["builder"] != "auto" {
		t.Fatalf("auto build = %d: %s", w.Code, w.Body)
	}
	id, _ := out["campaign_id"].(string)
	got := waitState(t, f, mgr, id, "done")
	if got["recommendation"] != "insufficient_data" {
		t.Fatalf("two requests: %v", got)
	}
	waitBuildsIdle(t)
	if _, spec, _, err := readCampaignFiles(dir, id); err != nil || spec.Policy.Kind != "rules" {
		t.Fatalf("the auto spec does not load: %v", err)
	}
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"shadow"}`, mgr); w.Code != http.StatusConflict {
		t.Errorf("shadow on insufficient data = %d, want 409", w.Code)
	}
}

// refreshFixture records 80 sessions x 30 requests for the tenant (returning gap after each response) and for a fleet tenant (900 s, which only K3
// or more bridges, so the fleet plan is K3), and puts a live auto campaign with plan K5 in the given mode.
func refreshFixture(t *testing.T, gap int64, mode string) (f *mgrFixture, tid, base string) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	_, tid = f.signUpJar(t, "user@ibm.com")
	t0 := time.Now().Add(-20 * 24 * time.Hour).UnixMilli()
	var evs []*dash.Event
	for _, tc := range []struct {
		tenant string
		gap    int64
	}{{tid, gap}, {"fleet-tenant", 900e3}} {
		for s := 0; s < 80; s++ {
			ts := t0 + int64(s)*3*3600e3
			for i := 0; i < 30; i++ {
				evs = append(evs, &dash.Event{TenantID: tc.tenant, SessionID: fmt.Sprintf("s%d", s), TS: ts, Model: "m1", Status: 200, Mode: dash.ModeActive,
					TokenAccounting: dash.AccountingComplete, CacheRead: 50000, UpstreamMs: 1000})
				ts += 1000 + tc.gap
			}
		}
	}
	// straight to the store in one transaction: through the recorder's queue this fixture took most of a minute under -race
	if err := f.h.opts.Dashboard.DB().InsertEvents(evs); err != nil {
		t.Fatal(err)
	}
	id := "cmp-0123456789ab"
	base = filepath.Join(dir, id)
	_ = os.MkdirAll(base, 0o700)
	now := time.Now()
	_ = writeJSONFile(filepath.Join(base, "meta.json"), campaignMeta{ID: id, Tenant: tid, ScopeType: "tenant", Mode: mode})
	_ = writeJSONFile(filepath.Join(base, "request.json"), map[string]any{"since": now.Add(-30 * 24 * time.Hour).UnixMilli(), "until": now.UnixMilli(), "builder": "auto"})
	old := map[string]any{"schema": "cg.cache.campaign/2", "builder": map[string]any{"engine": "auto"}, "menu": map[string]any{"K5": map[string]any{"gaps_s": []float64{270, 270, 270, 270, 270}}},
		"policy": map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "all", "when": []any{}, "rule": map[string]any{"kind": "plan", "plan": "K5"}}}}}
	_ = writeJSONFile(filepath.Join(base, "spec.json"), old)
	if err := f.h.predRT().load(id); err != nil {
		t.Fatal(err)
	}
	return f, tid, base
}

func livePlan(f *mgrFixture, tid string) (int, string) {
	pc := f.h.predRT().forTenant(tid)
	pc.mu.Lock()
	defer pc.mu.Unlock()
	k, _ := pc.spec.PlanOf(pc.spec.RuleFor(&predictive.Facts{}))
	return k, pc.meta.Mode
}

func TestAutoCampaignRefreshRebuildsTheLivePlanInPlace(t *testing.T) {
	// the tenant returns 400 s after each response: one ping is enough and K1 wastes the fewest pings after a session's last response,
	// which its own held-out rows show clearly, so the rebuild ships its own K1
	f, tid, base := refreshFixture(t, 400e3, campaignModeShadow)
	f.h.refreshAutoCampaigns(time.Now())
	if k, mode := livePlan(f, tid); k != 1 || mode != campaignModeShadow {
		t.Fatalf("after the refresh: K%d in mode %s, want the tenant's own K1, still in shadow (status %v)", k, mode, readStatus(base))
	}
	if st := readStatus(base); st["refreshed_at"] == nil || st["recommendation"] != "ship" {
		t.Errorf("the refresh left no trace in the status: %v", st)
	}
}

// A rebuild that did not pass the ship gate never changes what an ACTIVE campaign runs (going active needs ship or force; a refresh must not
// get around that).
func TestAutoCampaignRefreshNeverChangesAnActivePlanWithoutShip(t *testing.T) {
	f, tid, base := refreshFixture(t, 900e3, campaignModeActive) // the tenant looks like the fleet: the rebuild says baseline
	f.h.refreshAutoCampaigns(time.Now())
	if k, mode := livePlan(f, tid); k != 5 || mode != campaignModeActive {
		t.Fatalf("after a non-ship refresh an active campaign runs K%d in mode %s, want its previous K5", k, mode)
	}
	if st := readStatus(base); st["refresh_kept_previous_plan"] != "baseline" {
		t.Errorf("status does not say the previous plan was kept: %v", st)
	}
}

// A spec the runtime would refuse never replaces a working one.
func TestWriteSpecValidatedKeepsTheOldSpecOnABadOne(t *testing.T) {
	dir := t.TempDir()
	good := map[string]any{"schema": "cg.cache.campaign/2", "menu": map[string]any{}, "policy": map[string]any{"kind": "rules", "segments": []any{}, "fallback": map[string]any{"kind": "none"}}}
	if err := writeSpecValidated(dir, good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{"schema": "cg.cache.campaign/2", "menu": map[string]any{}, "policy": map[string]any{"kind": "rules", "segments": []any{
		map[string]any{"id": "all", "rule": map[string]any{"kind": "plan", "plan": "K9"}}}}}
	if err := writeSpecValidated(dir, bad); err == nil {
		t.Fatal("a spec naming a missing plan was accepted")
	}
	var got map[string]any
	if err := readJSONFile(filepath.Join(dir, "spec.json"), &got); err != nil || len(got["policy"].(map[string]any)["segments"].([]any)) != 0 {
		t.Errorf("the good spec was overwritten: %v %v", got, err)
	}
}

// The refresh's last step re-reads the mode under the campaign's lock: one that went active while its rebuild ran keeps its plan, in memory and
// in spec.json (what a restart reloads), unless the rebuild was "ship".
func TestAutoRefreshSwapRechecksTheModeUnderTheLock(t *testing.T) {
	for _, c := range []struct {
		mode, rec string
		want      bool
	}{{campaignModeShadow, "baseline", true}, {campaignModeActive, "baseline", false}, {campaignModeActive, "ship", true}} {
		dir := t.TempDir()
		_ = writeJSONFile(filepath.Join(dir, "spec.json"), map[string]any{"v": "old"})
		old, ps := &predictive.Spec{}, &predictive.Spec{}
		pc := &predCampaign{meta: campaignMeta{Mode: c.mode}, spec: old} // e.g. a mode change that landed while the rebuild was running
		got := pc.swapSpec(dir, map[string]any{"v": "new"}, ps, c.rec)
		var disk map[string]any
		_ = readJSONFile(filepath.Join(dir, "spec.json"), &disk)
		if got != c.want || (pc.spec == ps) != c.want || (disk["v"] == "new") != c.want {
			t.Errorf("mode %s, rec %s: swapped %v (memory %v, disk %v), want %v", c.mode, c.rec, got, pc.spec == ps, disk["v"], c.want)
		}
	}
}

// The auto builder's tenant rows do not ride on the capped fleet read: accounts with 4 and 3 requests, older than another tenant's 6 that fill a
// cap of 6, each get all their own rows; a row in both reads counts once; one tenant's rows never leak into another's build (each tenant's
// read is dropped before the next); and the build says the fleet read was truncated.
func TestAutoRowsForReadsTheTenantsOwnRowsPastTheFleetCap(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	_, tid := f.signUpJar(t, "user@ibm.com")
	t0 := time.Now().Add(-5 * time.Hour).UnixMilli()
	for i := 0; i < 4; i++ {
		f.record(t, tid, "mine", &dash.Event{TS: t0 + int64(i)*60e3, Model: "m1", Status: 200, CacheRead: 50000, UpstreamMs: 1000, CostUSD: 0.01})
	}
	for i := 0; i < 3; i++ {
		f.record(t, "second-tenant", "theirs", &dash.Event{TS: t0 + 600e3 + int64(i)*60e3, Model: "m1", Status: 200, CacheRead: 50000, UpstreamMs: 1000, CostUSD: 0.01})
	}
	for i := 0; i < 6; i++ {
		f.record(t, "other-tenant", fmt.Sprintf("s%d", i), &dash.Event{TS: t0 + 3600e3 + int64(i)*60e3, Model: "m1", Status: 200, CacheRead: 50000, UpstreamMs: 1000, CostUSD: 0.01})
	}
	since, until := t0-60e3, time.Now().UnixMilli()
	count := func(rows []predictive.AutoRow) map[string]int {
		n := map[string]int{}
		for _, r := range rows {
			n[r.Scope]++
		}
		return n
	}
	// under the default cap the fleet read already holds the tenant's rows: they must count once, not twice
	d, err := f.h.autoFleet(since, until)
	if err != nil {
		t.Fatal(err)
	}
	rows, cut, err := f.h.autoRowsFor(d, tid, flatPrice, false, until)
	if err != nil || cut {
		t.Fatal(err, cut)
	}
	if n := count(rows.rows); n[tid] != 4 || n["second-tenant"] != 3 || n["other-tenant"] != 6 {
		t.Fatalf("uncapped rows by tenant %v; want 4 / 3 / 6, each counted once", n)
	}
	defer dash.SetKVCacheMaxRows(6)()
	if d, err = f.h.autoFleet(since, until); err != nil {
		t.Fatal(err)
	}
	if rows, _, err = f.h.autoRowsFor(d, "second-tenant", flatPrice, false, until); err != nil {
		t.Fatal(err)
	}
	if rows, _, err = f.h.autoRowsFor(d, tid, flatPrice, false, until); err != nil {
		t.Fatal(err)
	}
	if n := count(rows.rows); n[tid] != 4 || n["second-tenant"] != 0 || n["other-tenant"] != 6 {
		t.Fatalf("capped rows by tenant %v; want the tenant's 4 plus the fleet read's 6, and none of the tenant read before it", n)
	}
	spec := map[string]any{"evidence": map[string]any{}}
	d.note(spec, false)
	if data := spec["evidence"].(map[string]any)["data"].(map[string]any); data["fleet_truncated"] != true || data["fleet_rows_in_window"] != int64(13) {
		t.Errorf("evidence.data = %v; want the fleet read reported as truncated (6 of 13)", data)
	}
	if cv, _ := spec["caveats"].([]string); len(cv) != 1 {
		t.Errorf("caveats %v; want one saying the fleet prior was cut", cv)
	}
}
