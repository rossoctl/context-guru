package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

type stubPrices struct{}

func (stubPrices) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
}

// newCampaignFixture is a manager fixture whose campaign directory is a temp dir and whose "builder" is a shell stand-in for the Python pipeline.
func newCampaignFixture(t *testing.T, rec, modelFile string) (*mgrFixture, string) {
	t.Helper()
	dir := t.TempDir()
	model, err := filepath.Abs("../predictive/testdata/" + modelFile)
	if err != nil {
		t.Fatal(err)
	}
	script, _ := filepath.Abs("testdata/fake_campaign_build.sh")
	t.Setenv("CG_CAMPAIGN_DIR", dir)
	t.Setenv("CG_CAMPAIGN_PYTHON", "sh")
	t.Setenv("CG_CAMPAIGN_SCRIPT", script)
	t.Setenv("CG_CAMPAIGN_TEST_MODEL", model)
	t.Setenv("CG_CAMPAIGN_TEST_REC", rec)
	t.Setenv(envRegisterMode, "open")
	var r *dash.Recorder
	f := newHostedFixtureOpts(t, "up", "anthropic", "TEST_UPSTREAM_KEY", func(o *Options) {
		rr, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rr.Close() })
		o.Dashboard, o.Prices = rr, stubPrices{}
		r = rr
	})
	f.h.API().SetAuth(f.h.DashAuth())
	f.h.API().SetWhoami(f.h.DashWhoami())
	return &mgrFixture{hostedFixture: f, rec: r}, dir
}

func waitState(t *testing.T, f *mgrFixture, jar []*http.Cookie, id, want string) map[string]any {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		_, out := f.do(t, http.MethodGet, "/api/cache/campaigns/"+id, "", jar)
		if out["state"] == want {
			return out
		}
		if out["state"] == "failed" {
			t.Fatalf("build failed: %v", out)
		}
	}
	t.Fatalf("campaign %s never reached %s", id, want)
	return nil
}

func TestCacheCampaignBuildLifecycle(t *testing.T) {
	f, dir := newCampaignFixture(t, "shadow", "parity.model.json")
	mgr, _ := f.signUpJar(t, "boss@ibm.com")
	user, tid := f.signUpJar(t, "user@ibm.com")

	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"user","id":"`+tid+`"}`, user); w.Code != http.StatusForbidden {
		t.Fatalf("a plain user's build = %d, want 403", w.Code)
	}
	for _, bad := range []string{`{"scope":"global","id":"x"}`, `{"scope":"user","id":"a b"}`, `{"scope":"user","id":"x","objective":"latency"}`, `{"scope":"user","id":"x","window":"0d"}`, `{"scope":"user","id":"x","window":{"days":9999}}`} {
		if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/build", bad, mgr); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"user","id":"`+tid+`","window":"7d"}`, mgr); w.Code != http.StatusNotFound {
		t.Fatalf("build for a tenant with no traffic = %d, want 404", w.Code)
	}
	f.record(t, tid, "s1", &dash.Event{Model: "m1", Status: 200})

	w, out := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"user","id":"`+tid+`","window":{"days":7},"objective":"net_usd"}`, mgr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("build = %d: %s", w.Code, w.Body)
	}
	id, _ := out["campaign_id"].(string)
	got := waitState(t, f, mgr, id, "done")
	if got["recommendation"] != "shadow" || got["mode"] != "off" || got["live"] != false {
		t.Fatalf("unexpected view: %v", got)
	}
	if _, ok := got["spec"].(map[string]any); !ok {
		t.Fatalf("no spec in the view: %v", got)
	}
	sum, _ := got["summary"].(map[string]any)
	if vs, _ := sum["vs_best_baseline"].(map[string]any); vs["diff_usd"] != 1.0 {
		t.Errorf("summary lost the paired comparison: %v", sum)
	}

	// active needs a "ship" recommendation or force; shadow does not
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"active"}`, mgr); w.Code != http.StatusConflict {
		t.Fatalf("active on a shadow recommendation = %d, want 409", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"shadow"}`, mgr); w.Code != http.StatusOK {
		t.Fatalf("shadow = %d: %s", w.Code, w.Body)
	}
	pc := f.h.keeper.pred.forTenant(tid)
	if pc == nil {
		t.Fatal("shadow campaign is not live")
	}

	// A big opus response decided on live traffic: the model chooses K; in shadow nothing about the policy changes.
	body := []byte(`{"model":"aws/claude-opus-4","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a"}]}`)
	u := Usage{CacheRead: 150000, CacheWrite: 2000, FreshInput: 5, Output: 300, StopReason: "end_turn"}
	arr := f.h.keeper.pred.arrive(tid, "s1", "", time.Now().Add(-20*time.Second))
	d, ok := f.h.keeper.predictiveDecide(arr, false, tid, "s1", "", "anthropic", time.Now().Add(-20*time.Second), body, "claude-cli/1.0", u)
	if !ok || d.active {
		t.Fatalf("shadow decision = %+v ok=%v", d, ok)
	}
	pol := CachePolicy{KeepAlive: false}
	if d.apply(pol) != pol {
		t.Error("a shadow campaign changed the policy")
	}
	if s, mode := pc.snapshot(); s.Decisions != 1 || mode != campaignModeShadow {
		t.Fatalf("stats after one decision: %+v mode %s", s, mode)
	}
	// the next request on that thread arrives 40 s later: the shadow decision is scored
	f.h.keeper.pred.arrive(tid, "s1", "", time.Now().Add(20*time.Second))
	if s, _ := pc.snapshot(); s.Resolved != 1 || s.Returned != 1 {
		t.Errorf("the return was not scored: %+v", s)
	}

	// force to active: the decision is written into the policy
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"active","force":true}`, mgr); w.Code != http.StatusOK {
		t.Fatalf("forced active = %d: %s", w.Code, w.Body)
	}
	arr = f.h.keeper.pred.arrive(tid, "s2", "", time.Now())
	d, _ = f.h.keeper.predictiveDecide(arr, false, tid, "s2", "", "anthropic", time.Now(), body, "claude-cli/1.0", u)
	if !d.active {
		t.Fatal("active campaign decided passively")
	}
	got2 := d.apply(CachePolicy{KeepAlive: true, Idle: 270 * time.Second, MaxPings: 9, MinPrefixTokens: 1})
	if d.k > 0 && (!got2.KeepAlive || got2.MaxPings != d.k || got2.Idle != 270*time.Second) { // the model's 280 s, capped at the configured 270 s
		t.Errorf("k=%d but policy %+v", d.k, got2)
	}
	if d.k == 0 && got2.KeepAlive {
		t.Errorf("k=0 left keep-alive on: %+v", got2)
	}

	// survives a restart; a model file that is not the one evaluated is refused
	if rt := newPredRuntime(dir); rt.forTenant(tid) == nil {
		t.Error("the active campaign did not come back after a restart")
	}
	waitBuildsIdle(t) // the build goroutine re-validates the files after the script exits; do not tamper before it is done
	mp := filepath.Join(dir, id, "model.json")
	b, _ := os.ReadFile(mp)
	_ = os.WriteFile(mp, append(append([]byte(nil), b...), ' '), 0o600)
	if _, _, _, err := readCampaignFiles(dir, id); err == nil {
		t.Error("a model with the wrong digest was accepted")
	}

	// off removes it
	_ = os.WriteFile(mp, b, 0o600)
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"off"}`, mgr); w.Code != http.StatusOK {
		t.Fatalf("off = %d", w.Code)
	}
	if f.h.keeper.pred.forTenant(tid) != nil {
		t.Error("an off campaign is still live")
	}

	// erasing the tenant (purge / delete) removes the campaign from disk
	f.h.predRT().eraseTenant(tid)
	if fileExists(filepath.Join(dir, id)) {
		t.Error("an erased tenant's campaign survived on disk")
	}
}

func TestCacheCampaignSegmentsDecideThePlan(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	mgr, _ := f.signUpJar(t, "boss@ibm.com")
	_, tid := f.signUpJar(t, "user@ibm.com")
	f.record(t, tid, "s1", &dash.Event{Model: "m1", Status: 200})
	_, out := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"tenant","id":"`+tid+`"}`, mgr)
	id, _ := out["campaign_id"].(string)
	waitState(t, f, mgr, id, "done")
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"active"}`, mgr); w.Code != http.StatusOK {
		t.Fatalf("active on a ship recommendation = %d: %s", w.Code, w.Body)
	}
	body := []byte(`{"model":"aws/claude-haiku-4-5","messages":[]}`)
	arr := f.h.keeper.pred.arrive(tid, "s", "", time.Now())
	d, _ := f.h.keeper.predictiveDecide(arr, false, tid, "s", "", "anthropic", time.Now(), body, "claude-cli", Usage{CacheRead: 200000, StopReason: "end_turn"})
	if d.k != 0 {
		t.Errorf("the haiku segment says never ping, got k=%d", d.k)
	}
	body = []byte(`{"model":"aws/claude-opus-4","messages":[]}`)
	arr = f.h.keeper.pred.arrive(tid, "s", "t", time.Now())
	d, _ = f.h.keeper.predictiveDecide(arr, false, tid, "s", "t", "anthropic", time.Now(), body, "claude-cli", Usage{CacheRead: 500, StopReason: "end_turn"})
	if d.k != 2 || d.idleS != 280 {
		t.Errorf("small opus prefix should take the fixed K2 plan, got k=%d idle=%v", d.k, d.idleS)
	}
}

// The same lifecycle with the LightGBM hazard model: the spec's model reference resolves to it, shadow decisions are made with the live 72-input row, and an
// unseen model name or a first request (no history) does not break the decision.
func TestCacheCampaignWithTheLightGBMModel(t *testing.T) {
	f, _ := newCampaignFixture(t, "shadow", "lgb.model.json")
	mgr, _ := f.signUpJar(t, "boss@ibm.com")
	_, tid := f.signUpJar(t, "user@ibm.com")
	f.record(t, tid, "s1", &dash.Event{Model: "m1", Status: 200})
	_, out := f.do(t, http.MethodPost, "/api/cache/campaigns/build", `{"scope":"tenant","id":"`+tid+`"}`, mgr)
	id, _ := out["campaign_id"].(string)
	waitState(t, f, mgr, id, "done")
	if w, _ := f.do(t, http.MethodPost, "/api/cache/campaigns/"+id+"/mode", `{"mode":"shadow"}`, mgr); w.Code != http.StatusOK {
		t.Fatalf("shadow = %d: %s", w.Code, w.Body)
	}
	body := []byte(`{"model":"aws/claude-never-seen","stream":true,"max_tokens":8192,"messages":[{"role":"user","content":"x"}],"tools":[{"name":"a"}]}`)
	for i, sess := range []string{"s1", "s1", "s2"} {
		arr := f.h.keeper.pred.arrive(tid, sess, "", time.Now().Add(time.Duration(i-5)*time.Second))
		d, ok := f.h.keeper.predictiveDecide(arr, false, tid, sess, "", "anthropic", time.Now().Add(time.Duration(i-5)*time.Second), body, "claude-cli/1.0",
			Usage{CacheRead: 120000, CacheWrite: 800, FreshInput: 5, Output: 300, StopReason: "tool_use", Live: &LiveFacts{Messages: 3, Tools: 1, MaxTokens: 8192, Effort: "high", CacheTTL: "ephemeral_5m", Agent: "claude-cli", Stream: true}})
		if !ok || d.k < 0 || d.k > 4 {
			t.Fatalf("decision %d = %+v ok=%v", i, d, ok)
		}
	}
	if s, _ := f.h.keeper.pred.forTenant(tid).snapshot(); s.Decisions != 3 {
		t.Errorf("stats %+v", s)
	}
}

// The shadow scorer's constants are the ones the build's replay was calibrated with.
func TestShadowConstantsMatchTheEngineCalibration(t *testing.T) {
	b, err := os.ReadFile("../scripts/campaign/engine_calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Benefit float64 `json:"benefit_scale"`
		Ping    float64 `json:"ping_scale"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Benefit != cfScale || c.Ping != cfPingScale {
		t.Fatalf("calibration file has %v / %v, proxy/predictive.go has %v / %v", c.Benefit, c.Ping, cfScale, cfPingScale)
	}
}

func waitBuildsIdle(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		buildMu.Lock()
		n := len(buildActive)
		buildMu.Unlock()
		if n == 0 {
			return
		}
	}
	t.Fatal("a build never finished")
}

// A SessionEnd from the client cancels the pings a campaign had planned: the shadow score of an ended session pays only for pings that fired before the end,
// where an unsignalled no-return pays for every planned ping. This is the campaign's side of the keeper retire in clientsignal.go.
func TestSessionEndCancelsPlannedPingsInTheCampaignScore(t *testing.T) {
	pc := &predCampaign{pending: map[string][]predPending{}}
	p := predPending{endMs: 1_000_000, startMs: 990_000, k: 4, x: 280, stake: 10, ping: 0.5}
	pc.resolveLocked(p, 0, false)
	if pc.stats.WouldPings != 4 {
		t.Fatalf("no signal: %d pings, want all 4", pc.stats.WouldPings)
	}
	pc.stats = shadowStats{}
	pc.resolveStopLocked(p, 0, false, 100) // the client said the session ended 100 s after the response: the first ping (about 270 s) never goes out
	if pc.stats.WouldPings != 0 || pc.stats.EstNetUSD != 0 {
		t.Fatalf("signalled at 100 s: %+v", pc.stats)
	}
	pc.stats = shadowStats{}
	pc.resolveStopLocked(p, 0, false, 600) // ended after two pings were out
	if pc.stats.WouldPings != 2 {
		t.Fatalf("signalled at 600 s: %d pings, want 2", pc.stats.WouldPings)
	}
	// through the runtime: open decisions of that session only
	rt := &predRuntime{byTenant: map[string]*predCampaign{"T": pc}}
	pc.stats = shadowStats{}
	pc.pending["S1\x00"], pc.pending["S2\x00"], pc.npend = []predPending{p}, []predPending{p}, 2
	rt.sessionEnded("T", "S1", time.UnixMilli(p.endMs+50_000), time.Time{})
	if _, still := pc.pending["S2\x00"]; !still || len(pc.pending) != 1 || pc.stats.Resolved != 1 || pc.stats.WouldPings != 0 {
		t.Fatalf("sessionEnded touched the wrong state: pending %d, %+v", len(pc.pending), pc.stats)
	}
	// with a cutoff (a late compact event): a decision opened after the cutoff stays open
	late := predPending{endMs: 2_000_000, startMs: 1_990_000, k: 4, x: 280, stake: 10, ping: 0.5}
	pc.pending["S2\x00"], pc.npend = []predPending{p, late}, 2
	rt.sessionEnded("T", "S2", time.UnixMilli(1_991_000), time.UnixMilli(1_989_000))
	if got := pc.pending["S2\x00"]; len(got) != 1 || got[0].startMs != late.startMs || pc.npend != 1 {
		t.Fatalf("cutoff: pending %+v, npend %d; want only the later decision", got, pc.npend)
	}
	(*predRuntime)(nil).sessionEnded("T", "S1", time.Now(), time.Time{}) // nil-safe
}

// A compact signal resolves the main thread's open decisions only (only the main agent compacts): a subagent thread's decision stays open and is
// not scored as never-returned. SessionEnd still resolves every thread.
func TestCompactSignalLeavesASubagentThreadsDecisionOpen(t *testing.T) {
	pc := &predCampaign{pending: map[string][]predPending{}}
	rt := &predRuntime{byTenant: map[string]*predCampaign{"T": pc}}
	old := predPending{endMs: 1_000_000, startMs: 990_000, k: 4, x: 280, stake: 10, ping: 0.5}
	pc.pending["S\x00"], pc.pending["S\x00a:sub"], pc.npend = []predPending{old}, []predPending{old}, 2
	rt.sessionEnded("T", "S", time.UnixMilli(1_100_000), time.UnixMilli(1_098_000)) // SessionStart compact
	if _, open := pc.pending["S\x00a:sub"]; !open || pc.npend != 1 || pc.stats.Resolved != 1 {
		t.Fatalf("compact: pending %v, npend %d, %+v; want only the main thread resolved", pc.pending, pc.npend, pc.stats)
	}
	if _, open := pc.pending["S\x00"]; open {
		t.Fatal("compact left the main thread's pre-compaction decision open")
	}
	rt.sessionEnded("T", "S", time.UnixMilli(1_200_000), time.Time{}) // SessionEnd
	if len(pc.pending) != 0 || pc.npend != 0 || pc.stats.Resolved != 2 {
		t.Fatalf("SessionEnd: pending %v, npend %d, %+v; want every thread resolved", pc.pending, pc.npend, pc.stats)
	}
}
