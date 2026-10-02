package dash

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
)

// kapReq is a terse builder for one hand-built row, mirroring kvcache_test.go's own req()
// (unexported there, so this is a second, deliberately identical copy rather than an import of
// a test helper across packages).
func kapReq(id int64, user, conv string, tsMs int64, model, stopReason string) *kvcache.Request {
	return &kvcache.Request{ID: id, User: user, ConversationID: conv, TS: tsMs, Model: model,
		HourUTC: time.UnixMilli(tsMs).UTC().Hour(), Bucket: kvcache.BucketAt(tsMs),
		CachedContext: 100_000, InputTokens: 100, OutputTokens: 50, TTL: kvcache.TTL5m,
		TTLSource: kvcache.TTLSourceConfigured, MissReason: "hit", Hit: true,
		StopReason: stopReason}
}

const kapBase = int64(1_786_967_311_185)

// kapSpy is a kvcache.Strategy that records every Observation it is asked to Decide on, for the
// same reflection-based leak check kvcache's own TestStrategiesCannotSeeTheFuture uses.
type kapSpy struct{ seen []kvcache.Observation }

func (s *kapSpy) Name() string { return "spy" }
func (s *kapSpy) Decide(o kvcache.Observation) kvcache.Action {
	s.seen = append(s.seen, o)
	return kvcache.ActionExpire
}

// TestKeepAlivePagePredictorsCannotSeeTheFuture is kvcache's own TestStrategiesCannotSeeTheFuture,
// applied to what THIS file builds: kaBuildDecisions's Observation, not Simulate's.
//
// A field-by-field reflection check rather than a comment, for the reason the domain package's
// own test gives: the leak it prevents is invisible on screen. This one additionally poisons
// EVERY int64/int field with a later row's value directly (not only the successor's), because
// kaBuildDecisions walks every model's own previous-row pointer rather than a single linear
// LAG — a bug here would more plausibly read the wrong PAST row than the immediate successor.
func TestKeepAlivePagePredictorsCannotSeeTheFuture(t *testing.T) {
	rows := []*kvcache.Request{
		kapReq(11, "u", "s", kapBase, "m", "tool_use"),
		kapReq(12, "u", "s", kapBase+1_234_567, "m", "end_turn"),
		kapReq(13, "u", "s", kapBase+1_234_567+7_654_321, "m", "stop"),
		kapReq(14, "u", "s", kapBase+1_234_567+7_654_321+9_876_543, "m", "max_tokens"),
	}
	kvcache.Derive(rows)

	priceList := kvcache.NewPriceList(context.Background(), modelsOf(rows), stubPricer{}, kvcache.Multipliers{}, nil)
	spy := &kapSpy{}
	decisions, _ := kaBuildDecisions(rows, priceList, []kvcache.Strategy{spy}, map[string]kvcache.StrategySpec{},
		map[int64]kaCreditInfo{})
	if len(decisions) != len(rows) || len(spy.seen) != len(rows) {
		t.Fatalf("got %d decisions and %d observations, want %d of each",
			len(decisions), len(spy.seen), len(rows))
	}
	for i, o := range spy.seen {
		future := map[int64]string{}
		for _, later := range rows[i+1:] {
			future[later.TS] = "a later request's timestamp"
			future[later.ID] = "a later request's id"
			if later.IdleMs != nil {
				future[*later.IdleMs] = "a later request's idle duration"
			}
		}
		if r := rows[i]; r.IdleMs != nil {
			future[*r.IdleMs] = "THIS request's own idle duration, which is its future"
		}
		for _, ok := range []int64{rows[i].TS, rows[i].ID, int64(rows[i].HourUTC), rows[i].CachedContext} {
			delete(future, ok)
		}
		v := reflect.ValueOf(o)
		for f := 0; f < v.NumField(); f++ {
			fv := v.Field(f)
			if fv.Kind() != reflect.Int64 && fv.Kind() != reflect.Int {
				continue
			}
			if what, bad := future[fv.Int()]; bad {
				t.Errorf("decision %d field %s = %d, which is %s", i, v.Type().Field(f).Name, fv.Int(), what)
			}
		}
	}
}

// stubPricer is a minimal modelinfo.Pricer for tests that don't care about real rates.
type stubPricer struct{}

func (stubPricer) Price(_ context.Context, _ string) (modelinfo.Price, bool) {
	return modelinfo.Price{}, false
}

// TestKeepAlivePageModelSwitchResetsState: a session that switches model mid-way must not let
// the first model's entry, turn count or gap leak into the second's decisions — the exact
// defect night0928/kadash-work fixed in addressableCTE/pingSpans, replayed here against this
// file's own per-row walker instead of the SQL window functions.
func TestKeepAlivePageModelSwitchResetsState(t *testing.T) {
	rows := []*kvcache.Request{
		kapReq(1, "u", "s", kapBase, "opus", "end_turn"),
		kapReq(2, "u", "s", kapBase+30_000, "opus", "end_turn"),
		kapReq(3, "u", "s", kapBase+35_000, "sonnet", "end_turn"), // switches model 5s later
	}
	kvcache.Derive(rows)
	priceList := kvcache.NewPriceList(context.Background(), modelsOf(rows), stubPricer{}, kvcache.Multipliers{}, nil)
	decisions, models := kaBuildDecisions(rows, priceList, nil, map[string]kvcache.StrategySpec{}, nil)
	if len(decisions) != 3 {
		t.Fatalf("got %d decisions, want 3", len(decisions))
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want 2 (opus, sonnet)", models)
	}
	sonnetRow := decisions[2]
	if sonnetRow.Turn != 1 {
		t.Errorf("sonnet's first request has turn=%d, want 1 — opus's turn count leaked across the model boundary", sonnetRow.Turn)
	}
	if sonnetRow.SinceLastMs != 0 {
		t.Errorf("sonnet's first request has since_last_ms=%d, want 0 — it inherited opus's gap", sonnetRow.SinceLastMs)
	}
	if sonnetRow.TTLInForce != "none" {
		t.Errorf("sonnet's first request has ttl_in_force=%q, want %q — opus's cache entry cannot transfer between models", sonnetRow.TTLInForce, "none")
	}
	if !sonnetRow.ModelSwitch {
		t.Error("the first sonnet row should be marked model_switch")
	}
	if decisions[1].ModelSwitch {
		t.Error("the second opus row must not be marked model_switch")
	}
}

// TestKeepAlivePagePhantomRowsAreNotAddressable: cache_write = 0 on a ttl_expiry row means
// nothing was written — no prefix to protect and no charge a ping could have avoided. See
// keepalive.go's own comment: 385 of 742 such rows in the production corpus are exactly this.
func TestKeepAlivePagePhantomRowsAreNotAddressable(t *testing.T) {
	real := kapReq(1, "u", "s", kapBase, "m", "end_turn")
	real.CacheRead, real.CacheWrite = 50_000, 0
	phantom := kapReq(2, "u", "s", kapBase+600_000, "m", "end_turn")
	phantom.MissReason, phantom.CacheRead, phantom.CacheWrite = "ttl_expiry", 0, 0
	genuine := kapReq(3, "u", "s", kapBase+1_200_000, "m", "end_turn")
	genuine.MissReason, genuine.CacheRead, genuine.CacheWrite = "ttl_expiry", 0, 50_000
	rows := []*kvcache.Request{real, phantom, genuine}
	kvcache.Derive(rows)
	priceList := kvcache.NewPriceList(context.Background(), modelsOf(rows), stubPricer{}, kvcache.Multipliers{}, nil)
	decisions, _ := kaBuildDecisions(rows, priceList, nil, map[string]kvcache.StrategySpec{}, nil)
	if decisions[1].Addressable {
		t.Error("a ttl_expiry row with cache_write=0 must not be addressable — it is a phantom")
	}
	if !decisions[2].Addressable {
		t.Error("a ttl_expiry row with cache_write>0 must be addressable")
	}
}

// TestKeepAlivePageOptimalIsMarkedUnreachable: the ceiling arm must carry Unreachable through
// to the predictor decision, so nothing downstream can render it as a real result.
func TestKeepAlivePageOptimalIsMarkedUnreachable(t *testing.T) {
	rows := []*kvcache.Request{kapReq(1, "u", "s", kapBase, "m", "end_turn")}
	kvcache.Derive(rows)
	priceList := kvcache.NewPriceList(context.Background(), modelsOf(rows), stubPricer{}, kvcache.Multipliers{}, nil)
	optimal, err := kvcache.NewStrategy(kvcache.StrategyOptimal, rows, kvcache.Config{Prices: priceList})
	if err != nil {
		t.Fatal(err)
	}
	specByName := map[string]kvcache.StrategySpec{}
	for _, spec := range kvcache.Registry() {
		specByName[spec.Name] = spec
	}
	decisions, _ := kaBuildDecisions(rows, priceList, []kvcache.Strategy{optimal}, specByName, nil)
	if len(decisions[0].Predictors) != 1 || !decisions[0].Predictors[0].Unreachable {
		t.Errorf("optimal's predictor decision = %+v, want Unreachable=true", decisions[0].Predictors)
	}
}

// TestKeepAlivePageFeatureCatalogListsAbsentFeaturesRatherThanHidingThem: requirement 3 states
// this explicitly — an absent feature must be listed with its reason, not omitted.
func TestKeepAlivePageFeatureCatalogListsAbsentFeaturesRatherThanHidingThem(t *testing.T) {
	found := false
	for _, f := range kaFeatureCatalog() {
		if f.Availability == availNeedsInstrumentation {
			found = true
			if !f.Missing || f.MissingReason == "" {
				t.Errorf("feature %q is needs-instrumentation but Missing=%v reason=%q",
					f.Name, f.Missing, f.MissingReason)
			}
		}
		if f.Name == "" || f.Source == "" || f.Explanation == "" {
			t.Errorf("feature %+v is missing a name, source or explanation", f)
		}
	}
	if !found {
		t.Fatal("the catalog lists no needs-instrumentation feature; this check needs rewriting")
	}
}

// TestKeepAlivePageCreditReachability: a raw keepalive_saved_usd > 0 with no qualifying ping in
// its own window is a PHANTOM credit (kaSaved's own doc comment) and must render as
// CreditReachable=false, not as a saving.
func TestKeepAlivePageCreditReachability(t *testing.T) {
	unreachable := kaCredit(kapBase, "s1", 0.40) // no ping row anywhere in this fixture
	fx := newKAFixture(t, unreachable)
	credits, err := fx.db.kaPageCredits(Filter{TenantAll: true, Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := credits[unreachable.ID]; !ok {
		t.Fatal("the credited row should appear in the map even when its credit is unreachable")
	} else if c.usd != 0 {
		t.Errorf("saved_usd_credited = %v, want 0 — this credit has no ping to back it", c.usd)
	}
}

// TestKeepAlivePageSessionsTenantIsolation: a manager-scoped query for one tenant must not see
// another tenant's sessions in the cohort picker.
func TestKeepAlivePageSessionsTenantIsolation(t *testing.T) {
	a := kaAgent(kapBase, "s-a", 0.10)
	a.TenantID = "tenant-a"
	b := kaAgent(kapBase, "s-b", 0.20)
	b.TenantID = "tenant-b"
	fx := newKAFixture(t, a, b)
	rows, err := fx.db.KeepAlivePageSessions(Filter{Tenant: "tenant-a"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SessionID == "s-b" {
			t.Error("tenant-a's cohort list must not include tenant-b's session")
		}
	}
	found := false
	for _, r := range rows {
		if r.SessionID == "s-a" {
			found = true
		}
	}
	if !found {
		t.Error("tenant-a's own session is missing from its own cohort list")
	}
}

// TestKeepAlivePageSessionMissingSessionIsBadRequest: the HTTP handler must refuse rather than
// guess when no session is named — an empty Filter.Session would otherwise read as "every
// session", which on this route is a scope no caller asked for.
func TestKeepAlivePageSessionMissingSessionIsBadRequest(t *testing.T) {
	fx := newKAFixture(t, kaAgent(kapBase, "s1", 0.10))
	a := NewAPI(&Recorder{db: fx.db})
	req := httptest.NewRequest("GET", "/api/keepalive/page/session", nil)
	w := httptest.NewRecorder()
	a.keepAlivePageSession(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestKeepAlivePageSessionsInvalidLimitFallsBackToDefault: a garbage or out-of-range ?limit=
// must not error and must not silently return zero rows — it falls back to the shipped
// default, the same convention every other paged route on this dashboard uses (atoiDefault).
func TestKeepAlivePageSessionsInvalidLimitFallsBackToDefault(t *testing.T) {
	fx := newKAFixture(t, kaAgent(kapBase, "s1", 0.10))
	a := NewAPI(&Recorder{db: fx.db})
	for _, limit := range []string{"", "not-a-number", "-5", "99999"} {
		req := httptest.NewRequest("GET", "/api/keepalive/page/sessions?limit="+limit, nil)
		req.RemoteAddr = "127.0.0.1:1"
		w := httptest.NewRecorder()
		a.keepAlivePageSessions(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("limit=%q: status = %d, want 200: %s", limit, w.Code, w.Body.String())
			continue
		}
		var out struct {
			Sessions []*KAPageSessionRow `json:"sessions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("limit=%q: %v", limit, err)
		}
		if len(out.Sessions) != 1 {
			t.Errorf("limit=%q: got %d sessions, want 1 (the fixture's own session, under a "+
				"defaulted limit)", limit, len(out.Sessions))
		}
	}
}

// TestKeepAlivePageSessionUnknownSessionIsEmptyNotError: a session that does not exist (a typo,
// a session from before the window) is an empty page, not a 500 — the caller's mistake should
// read as "nothing here", the same convention every other filter on this dashboard uses.
func TestKeepAlivePageSessionUnknownSessionIsEmptyNotError(t *testing.T) {
	fx := newKAFixture(t, kaAgent(kapBase, "s1", 0.10))
	out, err := fx.db.KeepAlivePageSession(Filter{TenantAll: true, Session: "does-not-exist"},
		Filter{TenantAll: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Decisions) != 0 || out.Requests != 0 {
		t.Errorf("got %d decisions / %d requests for an unknown session, want 0/0", len(out.Decisions), out.Requests)
	}
}

// TestKeepAlivePageEndToEnd is the integration smoke test: a realistic fixture (agent traffic,
// pings, an addressable expiry) through the real HTTP route, checking the payload has the
// shape every other test above assumes piece by piece.
func TestKeepAlivePageEndToEnd(t *testing.T) {
	events := kaExpiry(kapBase+600_000, "s1", 610, 0.05, 50_000)
	events = append(events, kaPing(kapBase+890_000, "s1", 0.001, 5, 0))
	fx := newKAFixture(t, events...)
	a := NewAPI(&Recorder{db: fx.db})
	req := httptest.NewRequest("GET", "/api/keepalive/page/session?session=s1&tenant=*", nil)
	w := httptest.NewRecorder()
	a.keepAlivePageSession(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out KASession
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Decisions) != 2 {
		t.Fatalf("got %d decisions, want 2", len(out.Decisions))
	}
	if out.SessionStatsNote == "" {
		t.Error("the session-scoped-stats caveat must render unconditionally")
	}
	if out.Economics == nil || out.Economics.Session == nil || out.Economics.Cohort == nil {
		t.Fatal("economics is missing its session or cohort half")
	}
	sawUnreachable := false
	for _, arm := range out.Economics.Session.Arms {
		if arm.Name == kvcache.StrategyOptimal {
			if !arm.Unreachable {
				t.Error("optimal must be marked unreachable in the economics arm list")
			}
			sawUnreachable = true
		}
	}
	if !sawUnreachable {
		t.Error("optimal is not in the economics arm list at all")
	}
	if out.LedgerVsReplay == nil {
		t.Fatal("ledger_vs_replay is missing")
	}
}
