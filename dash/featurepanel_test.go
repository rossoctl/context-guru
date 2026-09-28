package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/kvcache/predictor"
)

// fpReq mirrors kapReq (keepalivepage_test.go's own hand-built row), a deliberately separate
// copy for the same reason kapReq itself gives: no test helper crosses a package boundary.
func fpReq(id int64, user, conv string, tsMs int64, model, stopReason string, cachedTokens int64) *kvcache.Request {
	return &kvcache.Request{ID: id, User: user, ConversationID: conv, TS: tsMs, Model: model,
		HourUTC: time.UnixMilli(tsMs).UTC().Hour(), Bucket: kvcache.BucketAt(tsMs),
		CachedContext: cachedTokens, InputTokens: 100, OutputTokens: 50, TTL: kvcache.TTL5m,
		TTLSource: kvcache.TTLSourceConfigured, MissReason: "hit", Hit: true,
		StopReason: stopReason}
}

const fpBase = int64(1_786_967_311_185)

// ── feature browser ──────────────────────────────────────────────────────────

// TestFeaturePanelCatalogListsAbsentFeaturesNeverHides checks the one rule requirement 1 turns
// on: a feature this schema cannot build is listed with Buildable=false and a reason, in the
// SAME slice as the buildable ones — never dropped from it. It exercises the registry's own
// built-in Impossible features (FeatureNextTS/FeatureIdleMs) rather than inventing a fixture,
// so a real registry change that removed them would fail this test too.
func TestFeaturePanelCatalogListsAbsentFeaturesNeverHides(t *testing.T) {
	freg := predictor.DefaultFeatures()
	cat := FeaturePanelCatalog(freg)
	if len(cat) == 0 {
		t.Fatal("empty catalog; predictor.DefaultFeatures registered nothing")
	}
	var sawBuildable, sawAbsent bool
	for _, f := range cat {
		if f.ID == predictor.FeatureNextTS || f.ID == predictor.FeatureIdleMs {
			if f.Buildable {
				t.Errorf("%s: Buildable=true, want false — it is an Impossible feature", f.ID)
			}
			if f.AbsentReason == "" {
				t.Errorf("%s: no AbsentReason — an absent feature must say why", f.ID)
			}
			sawAbsent = true
		}
		if f.ID == predictor.FeatureCachedTokens {
			if !f.Buildable {
				t.Errorf("%s: Buildable=false, want true — it is a Live feature", f.ID)
			}
			if f.AbsentReason != "" {
				t.Errorf("%s: AbsentReason=%q on a buildable feature", f.ID, f.AbsentReason)
			}
			sawBuildable = true
		}
	}
	if !sawAbsent || !sawBuildable {
		t.Fatalf("catalog did not exercise both cases (absent seen=%v, buildable seen=%v)", sawAbsent, sawBuildable)
	}
}

// TestFeaturePanelCatalogWithAHandBuiltNeedsInstrumentationFeature checks the
// NeedsInstrumentation class specifically, which predictor.DefaultFeatures does not register
// today (feat-impl is expected to add the ~19 genuinely unbuildable features this schema
// lacks — see this file's own top-of-package comment). A local registry stands in rather than
// waiting on that PR to land.
func TestFeaturePanelCatalogWithAHandBuiltNeedsInstrumentationFeature(t *testing.T) {
	freg := predictor.NewFeatures()
	if err := freg.Register(predictor.Feature{
		ID: "subagent_marker", Description: "Whether this row belongs to a subagent conversation.",
		AvailableAt: "no parent/child session link is stored", Availability: predictor.NeedsInstrumentation,
		Missing: predictor.MissingSkip, Privacy: predictor.PrivacyAggregate, Cost: predictor.CostFieldRead,
		Extract: func(kvcache.Observation, []*kvcache.Request, predictor.StatsContext) predictor.Value {
			return predictor.Value{}
		},
	}); err != nil {
		t.Fatal(err)
	}
	cat := FeaturePanelCatalog(freg)
	if len(cat) != 1 {
		t.Fatalf("got %d features, want 1", len(cat))
	}
	if cat[0].Buildable {
		t.Error("NeedsInstrumentation feature reported Buildable=true")
	}
	if cat[0].AbsentReason != "no parent/child session link is stored" {
		t.Errorf("AbsentReason = %q, want the registry's own AvailableAt text, not a restated copy", cat[0].AbsentReason)
	}
}

// TestFeaturePanelExampleIsLeakFree checks that the example filled in for one session's last
// decision point rests on real, in-scope history — turn 3, not something invented — and never
// panics reaching past the end of the slice, the same boundary predictor.Features.Extract
// itself enforces via the three-index past slice.
func TestFeaturePanelExampleIsLeakFree(t *testing.T) {
	rows := []*kvcache.Request{
		fpReq(1, "T01", "s", fpBase, "m", "tool_use", 50_000),
		fpReq(2, "T01", "s", fpBase+1_000_000, "m", "end_turn", 60_000),
		fpReq(3, "T01", "s", fpBase+3_000_000, "m", "stop_sequence", 70_000),
	}
	kvcache.Derive(rows)
	freg := predictor.DefaultFeatures()

	ex, err := FeaturePanelExample(rows, freg)
	if err != nil {
		t.Fatal(err)
	}
	if got := ex[predictor.FeatureTurn]; got != "3" {
		t.Errorf("turn = %q, want 3 (the last row)", got)
	}
	if got := ex[predictor.FeatureCachedTokens]; got != "70000" {
		t.Errorf("cached_tokens = %q, want 70000 (the last row's own value)", got)
	}
	if got := ex[predictor.FeatureStopReason]; got != "stop_sequence" {
		t.Errorf("stop_reason = %q, want stop_sequence", got)
	}
	// Impossible features must never carry an example value: there is nothing to extract.
	if _, ok := ex[predictor.FeatureNextTS]; ok {
		t.Errorf("next_ts has an example value; it is Impossible and must never be computed")
	}

	// Empty/nil input degrades to no examples, never an error or a panic.
	if ex2, err := FeaturePanelExample(nil, freg); err != nil || ex2 != nil {
		t.Errorf("FeaturePanelExample(nil, ...) = (%v, %v), want (nil, nil)", ex2, err)
	}
}

// TestFeaturePanelWithExampleMergesIntoTheCatalog checks the merge itself: every catalog entry
// keeps its static fields, and only a feature the example replay actually answered gets
// Example/ExampleHonesty filled in.
func TestFeaturePanelWithExampleMergesIntoTheCatalog(t *testing.T) {
	rows := []*kvcache.Request{fpReq(1, "T02", "s", fpBase, "m", "end_turn", 12_345)}
	kvcache.Derive(rows)
	freg := predictor.DefaultFeatures()

	out, err := FeaturePanelWithExample(rows, freg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range out {
		if f.ID == predictor.FeatureCachedTokens {
			found = true
			if f.Example != "12345" {
				t.Errorf("example = %q, want 12345", f.Example)
			}
			if f.ExampleHonesty != FPObserved {
				t.Errorf("example_honesty = %q, want %q", f.ExampleHonesty, FPObserved)
			}
		}
		if f.ID == predictor.FeatureNextTS && f.Example != "" {
			t.Errorf("next_ts carries an example (%q); it is Impossible", f.Example)
		}
	}
	if !found {
		t.Fatal("cached_tokens missing from the merged catalog")
	}
}

// ── correlation view ─────────────────────────────────────────────────────────

// TestFeaturePanelCorrelationsDegradesGracefullyWhenAbsent is requirement 2's explicit
// contract: a sibling job may not have produced its file yet, and this panel must say so
// plainly rather than render an empty table that looks like a zero result.
func TestFeaturePanelCorrelationsDegradesGracefullyWhenAbsent(t *testing.T) {
	c := FeaturePanelCorrelations(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if c.Computed {
		t.Error("Computed=true for a file that does not exist")
	}
	if c.Note == "" {
		t.Error("no Note explaining why this panel is empty")
	}
	if len(c.Features) != 0 {
		t.Error("Features non-empty despite Computed=false")
	}
}

// TestFeaturePanelCorrelationsParsesTheSweepsOwnFile checks the happy path against a
// hand-written file in this sweep's own documented shape, including the optional interval.
func TestFeaturePanelCorrelationsParsesTheSweepsOwnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "correlations.json")
	body := `{"generated_at":"2026-09-28T00:00:00Z","features":[
		{"feature":"stop_reason_cluster","vs_next_bucket":0.42,"low":0.30,"high":0.54,
		 "vs_strategy_outcome":[{"strategy":"keepalive-5m","correlation":0.11}]},
		{"feature":"turn","vs_next_bucket":-0.05}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := FeaturePanelCorrelations(path)
	if !c.Computed {
		t.Fatal("Computed=false for a real file")
	}
	if len(c.Features) != 2 {
		t.Fatalf("got %d features, want 2", len(c.Features))
	}
	if c.Features[0].VsNextBucket != 0.42 || !c.Features[0].Ranged || c.Features[0].High != 0.54 {
		t.Errorf("first feature parsed wrong: %+v", c.Features[0])
	}
	if c.Features[1].Ranged {
		t.Errorf("second feature has no low/high in the input but Ranged=true")
	}
	if len(c.Features[0].VsStrategyOutcome) != 1 || c.Features[0].VsStrategyOutcome[0].Strategy != "keepalive-5m" {
		t.Errorf("vs_strategy_outcome not parsed: %+v", c.Features[0])
	}
}

// TestFeaturePanelCorrelationsInvalidConfigFallsBackNotErrors is the invalid-config-fallback
// requirement: a file that exists but is not valid JSON must degrade the same way a missing
// file does, never panic and never claim Computed=true over garbage.
func TestFeaturePanelCorrelationsInvalidConfigFallsBackNotErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "correlations.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := FeaturePanelCorrelations(path)
	if c.Computed {
		t.Error("Computed=true over unparseable content")
	}
	if !strings.Contains(c.Note, "did not parse") {
		t.Errorf("Note = %q, want it to say the file did not parse", c.Note)
	}
}

// ── predictor comparison panel ───────────────────────────────────────────────

// TestWilsonIntervalIsSaneAtTheEdges checks the calibration interval helper directly: it must
// never run outside [0,1], must widen with n=0 reported as absent, and must be exactly 0..1
// wide (not a point) at the extremes where a Wald interval degenerates to zero width.
func TestWilsonIntervalIsSaneAtTheEdges(t *testing.T) {
	if _, _, ok := wilsonInterval(0, 0); ok {
		t.Error("n=0 reported ok=true")
	}
	lo, hi, ok := wilsonInterval(0, 5)
	if !ok || lo < 0 || hi > 1 || hi <= 0 {
		t.Errorf("wilsonInterval(0,5) = (%v,%v,%v), want a non-degenerate interval inside [0,1]", lo, hi, ok)
	}
	lo, hi, ok = wilsonInterval(5, 5)
	if !ok || lo >= 1 || hi > 1 {
		t.Errorf("wilsonInterval(5,5) = (%v,%v,%v), want low < 1 <= high", lo, hi, ok)
	}
}

// TestFeaturePanelCalibrationBinsExcludeCensoredRows checks the one leakage rule this
// function's own doc comment states: a censored decision point's Outcome is meaningless and
// must never be scored as a miss it never was.
func TestFeaturePanelCalibrationBinsExcludeCensoredRows(t *testing.T) {
	dps := []predictor.DecisionPoint{
		{Predictions: map[string]predictor.Prediction{"p@v1@5m": {P: 0.9, OK: true}},
			Outcome: predictor.Outcome{Censored: true, Within5m: false}}, // would look like a confident miss if counted
		{Predictions: map[string]predictor.Prediction{"p@v1@5m": {P: 0.9, OK: true}},
			Outcome: predictor.Outcome{Within5m: true}},
	}
	bins := fpCalibrationBins(dps, "p", "v1", "5m")
	if len(bins) != 1 {
		t.Fatalf("got %d bins, want 1 (the censored row must be excluded)", len(bins))
	}
	if bins[0].N != 1 || bins[0].ObservedRate != 1 {
		t.Errorf("bin = %+v, want N=1 ObservedRate=1 (only the non-censored hit counted)", bins[0])
	}
}

// fpFixture seeds one tenant's worth of kv-cache-shaped rows so predictor.Replay and
// dash.KVCacheSimulate both have real gaps to work with.
func fpFixture(t *testing.T) (*Recorder, *DB) {
	t.Helper()
	rec, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	now := time.Now().UnixMilli() - 3_600_000
	for i, stop := range []string{"tool_use", "end_turn", "stop_sequence", "end_turn"} {
		e := mkEvent(now+int64(i)*180_000, "sess", "aws/claude-sonnet-5", 100_000, 90_000)
		e.TenantID = "T03"
		e.CacheRead, e.CacheWrite = 80_000, 10_000
		e.CacheTTL = "ephemeral_5m"
		e.StopReason = stop
		e.CostUSD = 0.05
		if err := rec.DB().insertBatch([]*Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	return rec, rec.DB()
}

// TestFeaturePanelPredictorsScoresBothBaselines is requirement 3's core check: a registered
// predictor's net dollars must be reported against BOTH kvcache.StrategyFixed5m (never-ping)
// and kvcache.StrategyKeepAlive5m (ping-everyone) — never only the flattering one — reusing
// dash.KVCacheSimulate rather than a second cost computation.
func TestFeaturePanelPredictorsScoresBothBaselines(t *testing.T) {
	_, db := fpFixture(t)
	freg := predictor.DefaultFeatures()
	preg := FeaturePanelDefaultPredictors()

	out, err := db.FeaturePanelPredictors(Filter{Tenant: "T03"}, KVCacheOptions{},
		staticPricer{p: modelinfo.Price{Input: 3e-6, Output: 1.5e-5, CacheRead: 3e-7, CacheWrite: 3.75e-6}},
		freg, preg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d predictor comparisons, want 1 (reuse-model)", len(out))
	}
	p := out[0]
	if p.ID != "reuse-model" || !p.Validated || !p.Enforceable {
		t.Errorf("predictor = %+v, want reuse-model, validated, enforceable", p)
	}
	if p.VsNeverPing == nil || p.VsNeverPing.Baseline != kvcache.StrategyFixed5m {
		t.Fatalf("VsNeverPing missing or wrong baseline: %+v", p.VsNeverPing)
	}
	if p.VsPingEveryone == nil || p.VsPingEveryone.Baseline != kvcache.StrategyKeepAlive5m {
		t.Fatalf("VsPingEveryone missing or wrong baseline: %+v", p.VsPingEveryone)
	}
	if p.VsNeverPing.Honesty != FPReplayed || p.VsPingEveryone.Honesty != FPReplayed {
		t.Errorf("net-dollar honesty tags = %q / %q, want %q on both",
			p.VsNeverPing.Honesty, p.VsPingEveryone.Honesty, FPReplayed)
	}
	if !p.VsNeverPing.Known || !p.VsPingEveryone.Known {
		t.Errorf("priced traffic in scope but Known=false: never-ping=%v ping-everyone=%v",
			p.VsNeverPing.Known, p.VsPingEveryone.Known)
	}
}

// TestFeaturePanelPredictorsEmptyRegistryDegradesGracefully checks the other half of the
// invalid-config-fallback requirement: no registered predictor must return an empty slice,
// never an error and never a panic reaching into an empty registry.
func TestFeaturePanelPredictorsEmptyRegistryDegradesGracefully(t *testing.T) {
	_, db := fpFixture(t)
	out, err := db.FeaturePanelPredictors(Filter{Tenant: "T03"}, KVCacheOptions{}, staticPricer{},
		predictor.DefaultFeatures(), predictor.NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("got %v, want nil for an empty predictor registry", out)
	}
	if out, err := db.FeaturePanelPredictors(Filter{Tenant: "T03"}, KVCacheOptions{}, staticPricer{},
		predictor.DefaultFeatures(), nil); err != nil || out != nil {
		t.Errorf("nil registry: got (%v, %v), want (nil, nil)", out, err)
	}
}

// ── the route ─────────────────────────────────────────────────────────────────

// TestFeaturePanelRouteServesNoTranscriptContent checks the constraint every route on this
// dashboard is held to (see keepAlivePageRoutes's own comment): numbers, enum labels and this
// file's own static prose, never prompt text or a transcript.
func TestFeaturePanelRouteServesNoTranscriptContent(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: ":memory:", CaptureContent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	e := mkEvent(time.Now().UnixMilli(), "T04:sess", "aws/claude-sonnet-5", 1000, 800)
	const secret = "THE-ACTUAL-PROMPT-TEXT-MUST-NEVER-APPEAR"
	e.TenantID = "T04"
	e.Content = []ContentRow{{Path: "messages.0", Before: secret, After: "x"}}
	if err := rec.DB().insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}

	a := NewAPI(rec)
	a.SetAuth(func(*http.Request) (Principal, bool) { return Principal{TenantID: "T04"}, true })
	m := http.NewServeMux()
	a.Mount(m)
	req := httptest.NewRequest(http.MethodGet, "/api/featurepanel", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/featurepanel = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response carries transcript content:\n%s", w.Body.String())
	}
	var payload FeaturePanelPayload
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response did not decode as FeaturePanelPayload: %v\n%s", err, w.Body.String())
	}
	if len(payload.Features) == 0 {
		t.Error("no features served")
	}
}

// TestFeaturePanelRouteWithSessionFillsExamplesInScope checks that a session's rows, read
// through the SAME tenant-scoped Filter every other route uses (KVCacheDataset), are enough to
// fill a real example — it cannot reach across tenants, because Filter.where() is the one
// boundary every dataset read on this dashboard already goes through.
func TestFeaturePanelRouteWithSessionFillsExamplesInScope(t *testing.T) {
	_, db := fpFixture(t)
	f := Filter{Tenant: "T03", Session: "sess"}
	reqs, _, err := db.KVCacheDataset(f, KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) == 0 {
		t.Fatal("fixture session not found by its own tenant+session filter")
	}
	features, err := FeaturePanelWithExample(reqs, predictor.DefaultFeatures())
	if err != nil {
		t.Fatal(err)
	}
	var sawExample bool
	for _, f := range features {
		if f.ID == predictor.FeatureTurn && f.Example != "" {
			sawExample = true
		}
	}
	if !sawExample {
		t.Error("turn has no example despite a real in-scope session")
	}

	// The other tenant's rows must never surface through this same call: a session id is
	// client-supplied text, and Filter.Tenant is what pins it to the caller who owns it.
	other := Filter{Tenant: "T04", Session: "sess"}
	if reqs, _, err := db.KVCacheDataset(other, KVCacheOptions{}); err != nil {
		t.Fatal(err)
	} else if len(reqs) != 0 {
		t.Errorf("T04 sees %d rows of T03's session under its own filter", len(reqs))
	}
}
