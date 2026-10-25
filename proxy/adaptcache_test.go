package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/rossoctl/context-guru/tenant"
)

func TestRowFactsDefaultGates(t *testing.T) {
	ok := rowFacts{turn: 1, prefix: 80000, ping: 0.5}
	if ok.defaultK() != 2 {
		t.Fatalf("an eligible row gets %d pings, want 2", ok.defaultK())
	}
	for name, f := range map[string]rowFacts{"first request": {turn: 0, prefix: 80000}, "small prefix": {turn: 1, prefix: 79999}, "dear ping": {turn: 1, prefix: 80000, ping: 0.51}} {
		if f.defaultK() != 0 {
			t.Errorf("%s: %d pings, want 0", name, f.defaultK())
		}
	}
	// gateway dollars are scale 1: a 400 s return rescued by the first ping is worth one rewrite minus one read, minus the ping
	f := rowFacts{u: 2, gap: 400, returned: true, toEnd: 3600, stake: 0.3375, ping: 0.03}
	if g := f.gain(2, autoX, true, 1, 1); math.Abs(g-(0.3375-0.03)) > 1e-12 {
		t.Errorf("K2 on a 400 s return = %v", g)
	}
	// SessionEnd cancels the pings after a session's last response
	last := rowFacts{u: 2, toEnd: 0, ping: 0.03}
	if g := last.gain(3, autoX, true, 1, 1); g != 0 {
		t.Errorf("with SessionEnd the last response costs %v, want 0", g)
	}
	if g := last.gain(3, autoX, false, 1, 1); math.Abs(g+0.09) > 1e-12 {
		t.Errorf("without SessionEnd the last response costs %v, want -0.09", g)
	}
}

// segRows: everyone else favours K3. The target's "tool_use" responses come back after 1000 s (K3 rescues them) and its "end_turn" ones after
// three hours (every ping is wasted), so no single plan suits it and a plan per stop reason does.
func segRows(target string) ([]predictive.AutoRow, []rowFacts) {
	const t0 = int64(1_786_000_000_000)
	var rows []predictive.AutoRow
	var facts []rowFacts
	add := func(scope string, sessions int, gapFor func(i int) (float64, string)) {
		for s := 0; s < sessions; s++ {
			for i := 0; i < 40; i++ {
				gap, stop := gapFor(i)
				f := rowFacts{u: 2, gap: gap, returned: true, toEnd: math.Inf(1), stake: 0.3, ping: 0.03, turn: i, prefix: 100000, stop: stop, model: "m"}
				g := make([]float64, autoK+1)
				for k := 1; k <= autoK; k++ {
					g[k] = f.gain(k, autoX, true, cfScale, cfPingScale)
				}
				rows = append(rows, predictive.AutoRow{Scope: scope, Session: fmt.Sprintf("%s-%d", scope, s), EndMs: t0 + int64(i)*3600e3 + int64(s)*1000, Gain: g})
				facts = append(facts, f)
			}
		}
	}
	for u := 0; u < 4; u++ {
		add(fmt.Sprintf("fleet-%d", u), 30, func(int) (float64, string) { return 1000, "tool_use" })
	}
	add(target, 50, func(i int) (float64, string) {
		if i%2 == 0 {
			return 1000, "tool_use"
		}
		return 3 * 3600, "end_turn"
	})
	return rows, facts
}

func TestAutoSpecSegmentedShipsAPlanPerSegment(t *testing.T) {
	rows, facts := segRows("me")
	until := int64(1_786_000_000_000) + 50*3600e3
	spec, rec := autoSpec(rows, facts, "me", 0, until, true, "")
	if rec == "ship" {
		t.Fatalf("one plan for a tenant that needs two cannot beat K3 on held-out data: %v", spec["evidence"])
	}
	spec, rec = autoSpec(rows, facts, "me", 0, until, true, "stop_reason")
	if rec != "ship" {
		t.Fatalf("a plan per stop reason: rec %s, evidence %v", rec, spec["evidence"])
	}
	b, _ := json.Marshal(spec)
	ps, err := predictive.ParseSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	for stop, want := range map[string]int{"tool_use": 3, "end_turn": 0, "max_tokens": 3} { // an unseen value takes the fleet plan
		k, _ := ps.PlanOf(ps.RuleFor(&predictive.Facts{StopReason: stop}))
		if k < want || (want == 0 && k != 0) {
			t.Errorf("stop_reason %s runs %d pings, want %d", stop, k, want)
		}
	}
	// the default is in the gate
	found := false
	for _, p := range spec["evidence"].(map[string]any)["paired_vs_baselines"].([]any) {
		found = found || p.(map[string]any)["vs"] == "default(K2+SessionEnd)"
	}
	if !found {
		t.Error("the shipped default is not one of the gate's baselines")
	}
}

func TestSegmentPredicatesSelectTheirRowsLive(t *testing.T) {
	f := rowFacts{hour: 7, dow: 2, prefix: 90000, model: "m1", agent: "claude-cli", stop: "end_turn"}
	live := predictive.Facts{HourUTC: 7, DowUTC: 2, PrefixTok: 90000, Model: "m1", Agent: "claude-cli", StopReason: "end_turn"}
	other := predictive.Facts{HourUTC: 8, DowUTC: 3, PrefixTok: 10000, Model: "m2", Agent: "codex", StopReason: "tool_use"}
	for name, seg := range segFeatures {
		_, when := seg(&f)
		spec := map[string]any{"schema": predictive.SpecSchema, "menu": map[string]any{"K1": map[string]any{"gaps_s": []float64{270}}},
			"policy": map[string]any{"kind": "rules", "segments": []any{map[string]any{"id": "x", "when": when, "rule": map[string]any{"kind": "plan", "plan": "K1"}}}}}
		b, _ := json.Marshal(spec)
		ps, err := predictive.ParseSpec(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if k, _ := ps.PlanOf(ps.RuleFor(&live)); k != 1 {
			t.Errorf("%s: the segment does not select its own request live", name)
		}
		if k, _ := ps.PlanOf(ps.RuleFor(&other)); k != 0 {
			t.Errorf("%s: the segment selects another request", name)
		}
	}
}

func TestAdaptCacheReportAndApply(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	other, otherID := f.signUpJar(t, "other@ibm.com")
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no identity = %d, want 401", w.Code)
	}
	ts := time.Now().Add(-5 * time.Hour).UnixMilli()
	for i := 0; i < 4; i++ {
		e := &dash.Event{TS: ts + int64(i)*400e3, Model: "m1", Status: 200, CacheRead: 90000, CacheWrite: 1000, UpstreamMs: 1000, CostUSD: 0.03}
		e.StopReason = "end_turn"
		f.record(t, tid, "s1", e)
	}
	w, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", jar)
	if w.Code != http.StatusOK || rep["kind"] != "cache" || rep["version"] != float64(1) || rep["basis_id"] == "" {
		t.Fatalf("report = %d %s", w.Code, w.Body)
	}
	if win := rep["window"].(map[string]any); win["requests"] != float64(4) || win["sessions"] != float64(1) {
		t.Errorf("window %v", win)
	}
	byID := map[string]map[string]any{}
	for _, s := range rep["options"].([]any) {
		byID[s.(map[string]any)["id"].(string)] = s.(map[string]any)
	}
	// each 400 s return is rescued by the first ping: 3 rescues of 91,000 x (3.75 - 0.3)e-6, one ping each; the last response pings nothing (SessionEnd)
	k1 := byID["K1"]["in_sample"].(map[string]any)
	// in expectation: a rescued request still misses 2.8% of the time (this model's natural rate), a first ping rewrites 0.89% of the time
	st, pg := 91000*(3.75e-6-3e-7), 91000*3e-7+7*3e-6+15e-6
	want := 3 * (st*(1-0.028) - pg - fidPFirst*st)
	if math.Abs(k1["net_usd"].(float64)-want) > 0.006 || math.Abs(k1["rescues"].(float64)-3*0.972) > 0.006 || k1["sessions_helped"] != float64(1) {
		t.Errorf("K1 %v, want net %.4f", k1, want)
	}
	if d := byID["default"]["in_sample"].(map[string]any); math.Abs(d["rescues"].(float64)-2*0.972) > 0.006 { // never after a thread's first request; 91K >= 80K
		t.Errorf("default %v", d)
	}
	if byID["auto"]["unavailable"] == nil || byID["by_hour"]["unavailable"] == nil || byID["auto"]["net_usd"] != nil {
		t.Errorf("4 requests cannot evaluate an adapted plan: %v", byID["auto"])
	}
	if rep["recommendation"] != nil || rep["summary"] == "" {
		t.Errorf("no held-out evidence, yet recommendation %v / summary %q", rep["recommendation"], rep["summary"])
	}
	// another account sees none of it
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", other); w.Code != http.StatusNotFound {
		t.Errorf("another account's report = %d, want 404 (no traffic of its own)", w.Code)
	}
	basis := rep["basis_id"].(string)
	if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"K3","basis":"`+basis+`"}`, jar); w.Code != http.StatusBadRequest {
		t.Errorf("a blanket plan is not an apply option: %d", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","basis":"stale"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("a stale basis = %d, want 409", w.Code)
	}
	if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","basis":"`+basis+`"}`, other); w.Code != http.StatusConflict {
		t.Errorf("another account applying this account's basis = %d, want 409", w.Code)
	}
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"default"}`, jar); w.Code != http.StatusOK || out["ok"] != true || !strings.Contains(out["note"].(string), "already") {
		t.Errorf("apply default with no campaign = %d %v", w.Code, out)
	}
	w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"by_stop_reason","basis":"`+basis+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != false || out["applied"] != "none" || out["recommendation"] != "insufficient_data" {
		t.Fatalf("apply by_stop_reason on 4 requests = %d %v", w.Code, out)
	}
	if v, _ := f.h.campaignView(out["campaign_id"].(string), false); v["mode"] != campaignModeOff {
		t.Errorf("an insufficient build was switched on: %v", v["mode"])
	}
	// a non-manager's ?tenant= is ignored: the other account still reads only its own (empty) history, and this one keeps its own
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=2&tenant="+tid, "", other); w.Code != http.StatusNotFound {
		t.Errorf("a non-manager naming another account = %d, want 404 (its own empty history)", w.Code)
	}
	if w, rep2 := f.do(t, http.MethodGet, "/api/adapt/cache?days=2&tenant="+otherID, "", jar); w.Code != http.StatusOK || rep2["window"].(map[string]any)["requests"] != float64(4) {
		t.Errorf("?tenant= changed a non-manager's scope: %d", w.Code)
	}
}

func TestAdaptApplyShipsActiveAndShadowsTheRest(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	_, tid := f.signUpJar(t, "user@ibm.com")
	// two finished campaigns on disk, as the builder leaves them; the callback the apply route hands the builder decides their mode
	for _, c := range []struct {
		rec   string
		force bool
		want  string
	}{{"ship", false, campaignModeActive}, {"baseline", false, campaignModeShadow}, {"baseline", true, campaignModeActive}, {"insufficient_data", true, campaignModeOff}} {
		rec, want := c.rec, c.want
		id := newCampaignID()
		spec, _ := autoSpec(nil, nil, tid, 0, 1, true, "")
		spec["policy"] = map[string]any{"kind": "rules", "segments": []any{}, "fallback": map[string]any{"kind": "none"}} // the fleet plan of a non-ship build
		spec["menu"] = map[string]any{"K4": map[string]any{"gaps_s": []float64{270, 270, 270, 270}, "max_pings": 4}}
		spec["own_policy"], spec["own_plan"] = map[string]any{"kind": "rules", "segments": []any{}, "fallback": map[string]any{"kind": "plan", "plan": "K4"}}, "K4"
		if err := os.MkdirAll(dir+"/"+id, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = writeJSONFile(dir+"/"+id+"/meta.json", campaignMeta{ID: id, Tenant: tid, ScopeType: "tenant", Mode: campaignModeOff})
		_ = writeJSONFile(dir+"/"+id+"/spec.json", spec)
		_ = writeJSONFile(dir+"/"+id+"/status.json", map[string]any{"state": "done", "recommendation": rec})
		f.h.adaptAfterBuild("u", c.force, nil)(id)
		if v, _ := f.h.campaignView(id, false); v["mode"] != want {
			t.Errorf("recommendation %s, force %v -> mode %v, want %s", rec, c.force, v["mode"], want)
		}
		_, ps, _, err := readCampaignFiles(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := ps.PlanOf(ps.Policy.Fallback); (k == 4) != (c.force && rec == "baseline") {
			t.Errorf("recommendation %s, force %v: runs %d pings; apply anyway must run the chosen plan (K4), anything else the policy as built", rec, c.force, k)
		}
	}
}

// The report's builder_gate is the builder's own verdict: same rows, same units, same intervals. A tenant whose tool_use responses come back after
// 1000 s (K3 rescues them) and whose end_turn ones come back after three hours (every ping wasted) passes it with a plan per stop reason, and the
// apply then switches that campaign on.
func TestAdaptReportGateMatchesTheBuilder(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	seedStopReasonHistory(t, f, tid)
	w, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=30", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body)
	}
	gate := map[string]bool{}
	for _, o := range rep["options"].([]any) {
		o := o.(map[string]any)
		if ho, ok := o["held_out"].(map[string]any); ok && ho["builder_gate"] != nil {
			gate[o["id"].(string)] = ho["builder_gate"].(map[string]any)["pass"].(bool)
		}
	}
	if !gate["by_stop_reason"] || gate["auto"] {
		t.Fatalf("gate %v: a plan per stop reason must pass and one plan must not; summary %q", gate, rep["summary"])
	}
	if r, _ := rep["recommendation"].(map[string]any); r == nil || r["option"] != "by_stop_reason" {
		t.Errorf("recommendation %v, want by_stop_reason", rep["recommendation"])
	}
	// the save-more fields: savings to date (observed), the gain on the held-out basis, a Mon..Sun x 00..23 grid that adds up to that gain
	if r, _ := rep["recommendation"].(map[string]any); r != nil && (r["per_week_usd"] == nil || r["ci95"] == nil) {
		t.Errorf("recommendation without per_week_usd / ci95: %v", r)
	}
	if td, _ := rep["to_date"].(map[string]any); td == nil || td["class"] != "observed" || td["saved_usd"] == nil {
		t.Errorf("to_date %v", rep["to_date"])
	}
	gain, _ := rep["gain"].(map[string]any)
	hm, _ := rep["heatmap"].(map[string]any)
	if gain == nil || hm == nil || gain["option"] != "by_stop_reason" || hm["option"] != "by_stop_reason" {
		t.Fatalf("gain %v / heatmap option %v", gain, hm["option"])
	}
	if rep["display_tz"] != tenant.DefaultStrategyTZ {
		t.Errorf("display_tz %v", rep["display_tz"])
	}
	if note, _ := rep["display_tz_note"].(string); !strings.Contains(note, "offset as of "+time.Now().In(func() *time.Location { l, _ := time.LoadLocation(tenant.DefaultStrategyTZ); return l }()).Format("2006-01-02")) {
		t.Errorf("display_tz_note %q does not name the offset's date", note)
	}
	for _, o := range rep["options"].([]any) {
		o := o.(map[string]any)
		if o["id"] == "by_hour" && (o["plan"] == nil || strings.Contains(fmt.Sprint(o["label"], o["plan"]), "UTC:")) {
			t.Errorf("by_hour shown as %v / %v", o["label"], o["plan"])
		}
	}
	if r, _ := rep["recommendation"].(map[string]any); r != nil && !strings.Contains(r["change"].(string), "plan per stop reason") {
		t.Errorf("the change line names the id, not the option: %v", r["change"])
	}
	if hm["tz"] != tenant.DefaultStrategyTZ || rep["heatmap_tz"] != tenant.DefaultStrategyTZ {
		t.Errorf("heatmap tz %v / %v, want the account's strategy zone %s", hm["tz"], rep["heatmap_tz"], tenant.DefaultStrategyTZ)
	}
	grid := hm["values"].([]any)
	total := 0.0
	for _, row := range grid {
		cells := row.([]any)
		if len(cells) != 24 {
			t.Fatalf("a heatmap row has %d hours", len(cells))
		}
		for _, c := range cells {
			total += c.(float64)
		}
	}
	if want := gain["vs_default"].(map[string]any)["usd"].(float64); len(grid) != 7 || math.Abs(total-want) > 0.01*24*7 {
		t.Errorf("the grid sums to %.2f, the gain vs the default is %.2f", total, want)
	}
	if _, ok := rep["option_heatmaps"].(map[string]any)["K3"]; !ok {
		t.Error("no per-option heatmap for K3")
	}
	for _, opt := range []string{"auto", "by_stop_reason"} {
		w, rep = f.do(t, http.MethodGet, "/api/adapt/cache?days=30", "", jar)
		w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"`+opt+`","basis":"`+rep["basis_id"].(string)+`"}`, jar)
		if w.Code != http.StatusOK || out["ok"] != gate[opt] || (out["recommendation"] == "ship") != gate[opt] {
			t.Errorf("apply %s: report gate %v, builder %d %v", opt, gate[opt], w.Code, out)
		}
	}
	if pc := f.h.predRT().forTenant(tid); pc == nil || pc.meta.Mode != campaignModeActive {
		t.Fatal("the shipped campaign is not live")
	}
	// apply anyway: "auto" fails the gate, and forced it goes live with the tenant's own plan, not the fleet plan
	_, rep = f.do(t, http.MethodGet, "/api/adapt/cache?days=30", "", jar)
	w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","basis":"`+rep["basis_id"].(string)+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != false || out["force_available"] != true {
		t.Fatalf("auto without force = %d %v", w.Code, out)
	}
	w, out = f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","force":true,"basis":"`+rep["basis_id"].(string)+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != true || !strings.Contains(out["note"].(string), "plan you chose") {
		t.Fatalf("auto forced = %d %v", w.Code, out)
	}
	pc := f.h.predRT().forTenant(tid)
	if pc == nil || pc.meta.Mode != campaignModeActive || pc.meta.ID != out["campaign_id"] {
		t.Fatal("the forced campaign is not the live one")
	}
	var spec map[string]any
	_ = readJSONFile(filepath.Join(f.h.campaignDir(), pc.meta.ID, "spec.json"), &spec)
	if b1, _ := json.Marshal(spec["policy"]); string(b1) != func() string { b2, _ := json.Marshal(spec["own_policy"]); return string(b2) }() {
		t.Errorf("forced policy %s is not the tenant's own plan %v", b1, spec["own_policy"])
	}
	// revert switches it off
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"revert"}`, jar); w.Code != http.StatusOK || out["ok"] != true {
		t.Errorf("revert = %d %v", w.Code, out)
	}
	if pc := f.h.predRT().forTenant(tid); pc != nil {
		t.Errorf("revert left campaign %s %s", pc.meta.ID, pc.meta.Mode)
	}
}

// SessionEnd cancels every ping after the session's last response, whichever model's conversation the ping belongs to.
func TestSessionEndStopsPingsAcrossModels(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	a := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "m1", TS: t0, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	b := &kvcache.Request{ID: 2, User: "u", ConversationID: "s", Model: "m2", TS: t0 + 300e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	_, facts := autoRowsX([]*kvcache.Request{a, b}, flatPrice, true, t0+10*3600e3)
	// a's model never comes back; the session's last response ends 300 s after a's: only a's first ping (at ~270 s) is sent
	if fired, _ := facts[0].outcome(3, autoX, true); fired != 1 {
		t.Errorf("a: %d pings with SessionEnd, want 1", fired)
	}
	if fired, _ := facts[0].outcome(3, autoX, false); fired != 3 {
		t.Errorf("a: %d pings without SessionEnd, want 3", fired)
	}
	if fired, _ := facts[1].outcome(3, autoX, true); fired != 0 {
		t.Errorf("b, the last response: %d pings with SessionEnd, want 0", fired)
	}
}

func TestExpectedReplayChargesFailuresAndOtherSessionsHits(t *testing.T) {
	f := rowFacts{u: 2, gap: 400, returned: true, toEnd: 3600, span: math.Inf(1), prefix: 100000, nextPrefix: 60000, pRead: 3e-7, pWrite: 3.75e-6, ping: 0.03, model: "claude-sonnet-x"}
	p := &fidParams{h: 0.2, head: map[string]float64{"claude-sonnet-x": 10000}}
	rescue, ping, pings, rescues := f.expected(1, autoX, true, p)
	stake := (60000 - 10000) * (3.75e-6 - 3e-7) // the next request reuses 60K, of which a miss would still read the 10K head
	if want := stake * (1 - 0.013 - 0.2); math.Abs(rescue-want) > 1e-12 || math.Abs(rescues-(1-0.013-0.2)) > 1e-12 {
		t.Errorf("rescue %v (%v), want %v", rescue, rescues, want)
	}
	if want := 0.03 + fidPFirst*100000*(3.75e-6-3e-7); math.Abs(ping-want) > 1e-12 || pings != 1 {
		t.Errorf("ping %v x %v, want %v x 1", ping, pings, want)
	}
	// a return past the cliff of the last touch is no rescue; one before the cliff of the response needs no ping
	for _, gap := range []float64{271 + 2 + 290, 200} {
		g := f
		g.gap = gap
		if r, _, _, _ := g.expected(1, autoX, true, p); r != 0 {
			t.Errorf("gap %v: rescue %v, want 0", gap, r)
		}
	}
}

// A single-tenant proxy answers /api/adapt/cache only to a direct loopback caller: not to a remote address, not through a relay.
func TestAdaptCacheLocalIsLoopbackOnly(t *testing.T) {
	h := localAdaptHandler(t, filepath.Join(t.TempDir(), "d.db.adapt.json"))
	mux := h.Mux()
	for _, c := range []struct{ method, path, body string }{{"GET", "/api/adapt/cache", ""}, {"POST", "/api/adapt/cache/apply", `{"option":"default"}`}} {
		for _, addr := range []string{"198.51.100.7:4000", "relayed:127.0.0.1:4000", "127.0.0.1:4000"} {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			r.Header.Set("content-type", "application/json")
			if after, ok := strings.CutPrefix(addr, "relayed:"); ok {
				addr = after
				r.Header.Set("X-Forwarded-For", "203.0.113.9")
			}
			r.RemoteAddr = addr
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if refused := w.Code == http.StatusForbidden; refused != (addr != "127.0.0.1:4000" || r.Header.Get("X-Forwarded-For") != "") {
				t.Errorf("%s %s from %s (fwd %q) = %d %s", c.method, c.path, addr, r.Header.Get("X-Forwarded-For"), w.Code, w.Body)
			}
		}
	}
}

// A manager may read another account with ?tenant= (a non-manager may not, see TestAdaptCacheReportAndApply), and doing so leaves that
// account's own basis alone. An apply needs the caller's own basis, unexpired.
func TestAdaptCacheManagerScopeAndBasisRules(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	mgr, _ := f.signUpJar(t, "boss@ibm.com")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	ts := time.Now().Add(-5 * time.Hour).UnixMilli()
	for i := 0; i < 3; i++ {
		f.record(t, tid, "s1", &dash.Event{TS: ts + int64(i)*400e3, Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	}
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", mgr); w.Code != http.StatusNotFound {
		t.Errorf("the manager's own (empty) history = %d, want 404", w.Code)
	}
	_, own := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", jar)
	w, viaMgr := f.do(t, http.MethodGet, "/api/adapt/cache?days=2&tenant="+tid, "", mgr)
	if w.Code != http.StatusOK || viaMgr["window"].(map[string]any)["requests"] != float64(3) {
		t.Fatalf("a manager reading the account = %d %v", w.Code, viaMgr["window"])
	}
	basis := own["basis_id"].(string)
	for name, body := range map[string]string{"no basis": `{"option":"auto"}`, "the manager's basis": `{"option":"auto","basis":"` + viaMgr["basis_id"].(string) + `"}`} {
		if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", body, jar); w.Code != http.StatusConflict {
			t.Errorf("%s = %d, want 409", name, w.Code)
		}
	}
	// expired
	adaptBasisMu.Lock()
	for k, b := range adaptBasis {
		if b.id == basis {
			b.at = b.at.Add(-adaptBasisTTL - time.Minute)
			adaptBasis[k] = b
		}
	}
	adaptBasisMu.Unlock()
	if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","basis":"`+basis+`"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("an expired basis = %d, want 409", w.Code)
	}
}

// The ship gate is one rule for the build and the report: tying blanket K3 fails it even when the plan beats everything else.
func TestShipGateNeedsToBeatK3TheFleetAndTheDefault(t *testing.T) {
	sess := make([]string, 200)
	sub, worse := make([]float64, 200), make([]float64, 200)
	for i := range sub {
		sess[i] = fmt.Sprintf("s%d", i%40)
		sub[i], worse[i] = 1, 0.5
	}
	if ok, _ := shipGate(sub, gateLegs(worse, worse, worse, 3), sess); !ok {
		t.Error("a plan that beats every leg must pass")
	}
	for leg := 0; leg < 3; leg++ {
		v := [3][]float64{worse, worse, worse}
		v[leg] = sub
		if ok, failed := shipGate(sub, gateLegs(v[0], v[1], v[2], 3), sess); ok || len(failed) != 1 {
			t.Errorf("tying leg %d passes (failed %v)", leg, failed)
		}
	}
	if n := len(gateLegs(worse, worse, nil, 3)); n != 2 {
		t.Errorf("without a default the gate has %d legs, want K3 and the fleet plan", n)
	}
}

// seedRules writes a finished campaign for tenant whose mode was last set by by, and loads it.
func seedRules(t *testing.T, f *mgrFixture, dir, tenantID, by, mode, rec string) string {
	t.Helper()
	id := newCampaignID()
	spec, _ := autoSpec(nil, nil, tenantID, 0, 1, true, "")
	spec["policy"] = map[string]any{"kind": "rules", "segments": []any{}, "fallback": map[string]any{"kind": "none"}}
	if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = writeJSONFile(filepath.Join(dir, id, "meta.json"), campaignMeta{ID: id, Tenant: tenantID, ScopeType: "tenant", Mode: mode, Actor: by, ModeBy: by})
	_ = writeJSONFile(filepath.Join(dir, id, "spec.json"), spec)
	_ = writeJSONFile(filepath.Join(dir, id, "status.json"), map[string]any{"state": "done", "recommendation": rec})
	if err := f.h.predRT().load(id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A campaign a manager set is the manager's: the account, by session or by its proxy token, can neither apply over it, force, switch it off nor
// revert it, and nothing changes. A manager can.
func TestAdaptCacheNeverOverridesAManagersCampaign(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	mgr, mgrID := f.signUpJar(t, "boss@ibm.com")
	w, verify := f.signUp(t, "user@ibm.com", "laptop")
	jar := w.Result().Cookies()
	tid := verify["tenant"].(map[string]any)["id"].(string)
	tok, _ := verify["token"].(string)
	ts := time.Now().Add(-5 * time.Hour).UnixMilli()
	for i := 0; i < 3; i++ {
		f.record(t, tid, "s1", &dash.Event{TS: ts + int64(i)*400e3, Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	}
	id := seedRules(t, f, dir, tid, mgrID, campaignModeActive, "ship")
	_, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", jar)
	basis := rep["basis_id"].(string)
	for _, body := range []string{`{"option":"auto","basis":"` + basis + `"}`, `{"option":"auto","force":true,"basis":"` + basis + `"}`, `{"option":"default"}`, `{"option":"revert"}`} {
		if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", body, jar); w.Code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "account admin") {
			t.Errorf("session %s = %d %v", body, w.Code, out)
		}
		if tok != "" {
			r := httptest.NewRequest(http.MethodPost, "/api/adapt/cache/apply", strings.NewReader(body))
			r.Header.Set("content-type", "application/json")
			r.Header.Set(TokenHeader, tok)
			tw := httptest.NewRecorder()
			f.mux.ServeHTTP(tw, r)
			if tw.Code != http.StatusConflict {
				t.Errorf("token %s = %d %s", body, tw.Code, tw.Body)
			}
		}
	}
	if tok == "" {
		t.Error("the fixture gave no proxy token; the token half of this test did not run")
	}
	if pc := f.h.predRT().forTenant(tid); pc == nil || pc.meta.ID != id || pc.meta.Mode != campaignModeActive {
		t.Fatal("the manager's campaign changed")
	}
	// the manager may switch it off for the account
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply?tenant="+tid, `{"option":"default"}`, mgr); w.Code != http.StatusOK || out["ok"] != true {
		t.Errorf("manager default = %d %v", w.Code, out)
	}
	if pc := f.h.predRT().forTenant(tid); pc != nil {
		t.Error("the manager's switch-off did not apply")
	}
}

// A campaign the account set itself may be forced by it; the audit trail says the gate failed. Revert undoes only the caller's own adapt
// campaign.
func TestAdaptCacheForceOnTheAccountsOwnCampaignIsAudited(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	seedRules(t, f, dir, tid, tid, campaignModeShadow, "baseline") // the account's own, not an adapt apply
	if w, _ := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"revert"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("revert of a campaign no adapt apply started = %d, want 409", w.Code)
	}
	seedStopReasonHistory(t, f, tid)
	_, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=30", "", jar)
	w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","force":true,"basis":"`+rep["basis_id"].(string)+`"}`, jar)
	if w.Code != http.StatusOK || out["ok"] != true {
		t.Fatalf("forced auto on the account's own campaign = %d %v", w.Code, out)
	}
	entries, err := f.h.registry().Audit(tid, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || (e.Field == "adapt" && strings.Contains(e.After, "forced, gate failed") && strings.Contains(e.After, "via dashboard session"))
	}
	if !found {
		t.Errorf("no audit row says the apply was forced over a failed gate: %+v", entries)
	}
	// now the live campaign is this caller's adapt apply: revert switches it off
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"revert"}`, jar); w.Code != http.StatusOK || out["applied"] != "revert" {
		t.Errorf("revert of the caller's own adapt campaign = %d %v", w.Code, out)
	}
	if pc := f.h.predRT().forTenant(tid); pc != nil {
		t.Error("revert left a live campaign")
	}
}

// The keeper holds one entry per thread of a session: a request on another thread does not end this thread's pings.
func TestPingSpanIsPerThread(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	a := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "m", TS: t0, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	b := &kvcache.Request{ID: 2, User: "u", ConversationID: "s", ThreadID: "a:sub", Model: "m", TS: t0 + 100e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	c := &kvcache.Request{ID: 3, User: "u", ConversationID: "s", Model: "m", TS: t0 + 2000e3, UpstreamMs: 1000, CachedContext: 100000, CostKnown: true}
	_, facts := autoRowsX([]*kvcache.Request{a, b, c}, flatPrice, false, t0+10*3600e3)
	if facts[0].span != 1999 {
		t.Errorf("the main thread's span = %v s, want 1999 (its own next request, not the sub-agent's at 99 s)", facts[0].span)
	}
	p := &fidParams{head: map[string]float64{}}
	if _, _, pings, _ := facts[0].expected(3, autoX, false, p); pings < 2.9 {
		t.Errorf("the sub-agent's request stopped the main thread's pings: %v", pings)
	}
}

// A manager who sets a campaign while an adapt build runs wins: the build's campaign is not switched on over it. A campaign written before
// mode_by existed counts as set by its creator.
func TestAdaptCacheManagerGuardHoldsAcrossTheBuildAndForOldCampaigns(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	_, mgrID := f.signUpJar(t, "boss@ibm.com")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	user, err := f.h.registry().Get(tid)
	if err != nil {
		t.Fatal(err)
	}
	who := adaptWho{TenantID: tid, actor: user}
	ours := seedRules(t, f, dir, tid, tid, campaignModeOff, "ship")      // the adapt build, finished, not yet live
	mgrs := seedRules(t, f, dir, tid, mgrID, campaignModeActive, "ship") // the manager's, set while it built
	f.h.adaptAfterBuild(tid, false, func() bool {
		pc := f.h.predRT().forTenant(tid)
		return pc != nil && f.h.managedCampaign(pc, who)
	})(ours)
	if pc := f.h.predRT().forTenant(tid); pc == nil || pc.meta.ID != mgrs || pc.meta.Mode != campaignModeActive {
		t.Fatal("the adapt build replaced the manager's campaign")
	}
	// an old meta with no mode_by: its actor (the manager) set it
	var m campaignMeta
	if err := readJSONFile(filepath.Join(dir, mgrs, "meta.json"), &m); err != nil {
		t.Fatal(err)
	}
	m.ModeBy = ""
	_ = writeJSONFile(filepath.Join(dir, mgrs, "meta.json"), m)
	f.h.predRT().byTenant[tid].meta.ModeBy = ""
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"default"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("default over a manager's pre-mode_by campaign = %d %v", w.Code, out)
	}
}

// Over HTTP: a manager sets a campaign while the adapt build runs; the apply answers 409 and the manager's campaign stays live. And a campaign
// with neither mode_by nor actor is treated as a manager's (fail closed).
func TestAdaptCacheApplyRefusesWhenAManagerActsMidBuild(t *testing.T) {
	f, dir := newCampaignFixture(t, "ship", "parity.model.json")
	t.Setenv("CG_CAMPAIGN_SCRIPT", "/nonexistent")
	_, mgrID := f.signUpJar(t, "boss@ibm.com")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	seedStopReasonHistory(t, f, tid)
	_, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=30", "", jar)
	var mgrs string
	adaptMidBuild = func() { mgrs = seedRules(t, f, dir, tid, mgrID, campaignModeActive, "ship") }
	defer func() { adaptMidBuild = nil }()
	w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"auto","force":true,"basis":"`+rep["basis_id"].(string)+`"}`, jar)
	if pc := f.h.predRT().forTenant(tid); pc == nil || pc.meta.ID != mgrs || pc.meta.Mode != campaignModeActive {
		t.Fatalf("the manager's mid-build campaign did not survive the apply (%d %v)", w.Code, out)
	}
	if w.Code != http.StatusConflict {
		t.Errorf("apply over a mid-build manager campaign = %d %v, want 409", w.Code, out)
	}
	adaptMidBuild = nil
	// fail closed
	var m campaignMeta
	_ = readJSONFile(filepath.Join(dir, mgrs, "meta.json"), &m)
	m.ModeBy, m.Actor = "", ""
	_ = writeJSONFile(filepath.Join(dir, mgrs, "meta.json"), m)
	pc := f.h.predRT().byTenant[tid]
	pc.mu.Lock()
	pc.meta.ModeBy, pc.meta.Actor = "", ""
	pc.mu.Unlock()
	if w, out := f.do(t, http.MethodPost, "/api/adapt/cache/apply", `{"option":"default"}`, jar); w.Code != http.StatusConflict {
		t.Errorf("default over a campaign nobody is recorded as setting = %d %v, want 409", w.Code, out)
	}
}

// seedStopReasonHistory records 50 sessions in which tool_use responses come back after 1000 s and end_turn ones after three hours, so a plan per
// stop reason beats every single plan.
func seedStopReasonHistory(t *testing.T, f *mgrFixture, tid string) {
	t.Helper()
	// a small fixture: 16 sessions of 10 requests, with the build's evidence floors lowered to match (the full floors need ~2,000 rows, which
	// costs about a minute per test under -race)
	oldTst, oldSes := autoMinTst, autoMinSes
	autoMinTst, autoMinSes = 30, 8
	t.Cleanup(func() { autoMinTst, autoMinSes = oldTst, oldSes })
	const sessions, perSession = 16, 10
	start := time.Now().Add(-28 * 24 * time.Hour).UnixMilli()
	for s := 0; s < sessions; s++ {
		ts := start + int64(s)*600e3
		for i := 0; i < perSession; i++ {
			e := &dash.Event{TenantID: tid, SessionID: fmt.Sprintf("s%d", s), TS: ts, Model: "m1", Status: 200, CacheRead: 100000, UpstreamMs: 2000, CostUSD: 0.03,
				Mode: dash.ModeActive, TokenAccounting: dash.AccountingComplete}
			e.StopReason = "tool_use"
			ts += 2000 + 1000e3
			if i%2 == 1 {
				e.StopReason = "end_turn"
				ts += 3*3600e3 - 1000e3
			}
			f.rec.Record(e)
		}
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if p, _ := f.rec.DB().Requests(dash.Filter{Tenant: tid}, 0, 1); p != nil && p.Total >= sessions*perSession {
			return
		}
		if time.Now().After(end) {
			t.Fatal("the writer did not persist the rows")
		}
	}
}

// The analysis read keeps only the newest rows of the whole fleet. The report and the build read the caller's own rows on their own, so an account
// with 4 requests among a busier fleet still gets its full report.
func TestAdaptReportReadsTheCallersRowsPastTheFleetCap(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	_, other := f.signUpJar(t, "other@ibm.com")
	ts := time.Now().Add(-5 * time.Hour).UnixMilli()
	for i := 0; i < 4; i++ {
		f.record(t, tid, "s1", &dash.Event{TS: ts + int64(i)*400e3, Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	}
	for i := 0; i < 6; i++ {
		f.record(t, other, "o1", &dash.Event{TS: ts + 3600e3 + int64(i)*60e3, Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	}
	t.Cleanup(dash.SetKVCacheMaxRows(6))
	w, rep := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", jar)
	if w.Code != http.StatusOK || rep["window"].(map[string]any)["requests"] != float64(4) {
		t.Fatalf("report with the fleet over the cap = %d %v", w.Code, rep["window"])
	}
	found := false
	for _, x := range rep["warnings"].([]any) {
		found = found || strings.Contains(x.(string), "shared prior")
	}
	if !found {
		t.Error("a truncated fleet prior is not said")
	}
	// the account's own window over the cap: refused, not shortened
	t.Cleanup(dash.SetKVCacheMaxRows(3))
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=2", "", jar); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("own window over the cap = %d, want 422", w.Code)
	}
}

// Reports are bounded per account: adaptReportBurst an hour, then 429.
func TestAdaptCacheReportsAreRateLimitedPerAccount(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	otherJar, _ := f.signUpJar(t, "other@ibm.com")
	f.record(t, tid, "s1", &dash.Event{TS: time.Now().Add(-time.Hour).UnixMilli(), Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	for i := 0; i < adaptReportBurst; i++ {
		if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=1", "", jar); w.Code != http.StatusOK {
			t.Fatalf("report %d = %d", i+1, w.Code)
		}
	}
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=1", "", jar); w.Code != http.StatusTooManyRequests {
		t.Errorf("report %d = %d, want 429", adaptReportBurst+1, w.Code)
	}
	if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=1", "", otherJar); w.Code == http.StatusTooManyRequests {
		t.Error("one account's reports spent another's budget")
	}
}

// The heatmap is in the account's strategy timezone (tenant.DefaultStrategyTZ unless a strategy that targets the account says otherwise): a
// request at 22:30 UTC on a Tuesday in August is Wednesday 01:30 in Asia/Jerusalem.
func TestAdaptHeatmapIsInTheStrategyTimezone(t *testing.T) {
	var k *keeper
	if tz := k.strategyTZFor("t"); tz != tenant.DefaultStrategyTZ {
		t.Fatalf("no strategies: %s, want %s", tz, tenant.DefaultStrategyTZ)
	}
	loc, err := time.LoadLocation(tenant.DefaultStrategyTZ)
	if err != nil {
		t.Skip("no tz database: ", err)
	}
	ts := time.Date(2026, 8, 11, 22, 30, 0, 0, time.UTC) // a Tuesday
	facts := []rowFacts{{startMs: ts.UnixMilli()}}
	m := adaptHeatmap(facts, []int{0}, []float64{1.5}, []float64{0.5}, loc)
	if m[2][1] != 1 { // Mon=0, so Wednesday is row 2; 01:xx is column 1
		t.Errorf("Wednesday 01:00 cell = %v; grid %v", m[2][1], m)
	}
	if utc := adaptHeatmap(facts, []int{0}, []float64{1.5}, []float64{0.5}, time.UTC); utc[1][22] != 1 {
		t.Errorf("in UTC the request is Tuesday 22:00: %v", utc[1][22])
	}
	// a strategy that targets the account sets the zone
	k = &keeper{strategies: []tenant.Strategy{{Active: true, Target: tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t"}},
		Windows: []tenant.Window{{Start: "09:00", End: "17:00", TZ: "Europe/Berlin"}}}}}
	if tz := k.strategyTZFor("t"); tz != "Europe/Berlin" {
		t.Errorf("a strategy's zone: %s", tz)
	}
	if tz := k.strategyTZFor("other"); tz != tenant.DefaultStrategyTZ {
		t.Errorf("another account's strategy set this one's zone: %s", tz)
	}
}

// A by_hour / by_dow option runs on UTC hours and days, but the user reads it in the account's timezone: a 22:00 UTC segment shows as
// 01:00-02:00 in Asia/Jerusalem in August, a Tuesday (UTC) as Tuesday 03:00 to Wednesday 03:00.
func TestAdaptOptionsShowLocalHours(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Skip("no tz database: ", err)
	}
	ref := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if got := segLabel("hour", "22 UTC", loc, ref); got != "01:00-02:00" {
		t.Errorf("22 UTC shows as %q", got)
	}
	if got := segLabel("dow", "Tuesday", loc, ref); got != "Tue 03:00 - Wed 03:00" {
		t.Errorf("Tuesday (UTC) shows as %q", got)
	}
	facts := []rowFacts{{hour: 22, dow: 2}}
	rows := []predictive.AutoRow{{Scope: "me"}}
	if got := segPlanDisplay("hour", facts, "me", rows, func(string) int { return 3 }, loc, ref); got != "01:00-02:00: K3" {
		t.Errorf("by_hour plan shows as %q", got)
	}
	policy := map[string]any{"segments": []any{map[string]any{"id": "hour:22 UTC", "rule": map[string]any{"kind": "plan", "plan": "K2"}}},
		"fallback": map[string]any{"kind": "plan", "plan": "K3"}}
	if got := policyDisplay(policy, loc, ref); got != "01:00-02:00: K2; otherwise K3" {
		t.Errorf("the apply note's plan shows as %q", got)
	}
}

// A busy process answers 503 with Retry-After and spends none of the account's hourly report budget.
func TestAdaptCacheBusyAnswers503WithoutSpendingTheBudget(t *testing.T) {
	f, _ := newCampaignFixture(t, "ship", "parity.model.json")
	jar, tid := f.signUpJar(t, "user@ibm.com")
	f.record(t, tid, "s1", &dash.Event{TS: time.Now().Add(-time.Hour).UnixMilli(), Model: "m1", Status: 200, CacheRead: 90000, UpstreamMs: 1000, CostUSD: 0.03})
	adaptCacheMu.Lock()
	for i := 0; i < adaptReportBurst+3; i++ {
		if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=1", "", jar); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			adaptCacheMu.Unlock()
			t.Fatalf("busy report = %d (Retry-After %q), want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
		}
	}
	adaptCacheMu.Unlock()
	for i := 0; i < adaptReportBurst; i++ {
		if w, _ := f.do(t, http.MethodGet, "/api/adapt/cache?days=1", "", jar); w.Code != http.StatusOK {
			t.Fatalf("report %d after the busy answers = %d: the busy answers spent the budget", i+1, w.Code)
		}
	}
}

// #719 review: the adapt arms' text named "270 s" after the default moved to 240 s. The number
// now comes from autoX; this pins every spaced arm's text to it, so it cannot drift again.
func TestAdaptLabelsStateTheSpacingTheArmsRunAt(t *testing.T) {
	want := fmt.Sprintf("%d s", autoX)
	for _, id := range []string{"default", "K1", "K2", "K3"} {
		if d := adaptLabels[id][1]; !strings.Contains(d, want) {
			t.Errorf("adapt arm %s says %q, want it to name the %s spacing the arms run at", id, d, want)
		}
	}
}
