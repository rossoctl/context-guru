package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
)

// fhEvents turns synthetic fit rows into request rows of one tenant, the newest an hour ago.
func fhEvents(rows []FiveHourRow, tenant string, sub bool) []*Event {
	if len(rows) == 0 {
		return nil
	}
	shift := time.Now().Add(-time.Hour).UnixMilli() - rows[len(rows)-1].TsMs
	var out []*Event
	for _, r := range rows {
		u, reset := r.Util, r.Reset+shift/1000
		out = append(out, &Event{TS: r.TsMs + shift, TenantID: tenant, SessionID: "s", Model: "m", Provider: "anthropic",
			AuthSubscription: sub, FreshInput: r.Input, CacheRead: r.CacheRead, CacheWrite: r.Write5m + r.Write1h,
			CacheWrite1h: r.Write1h, OutputTokens: r.Output,
			Limits: &Limits{FiveHour: LimitWindow{Utilization: &u, ResetS: &reset, Status: "allowed"}}})
	}
	return out
}

// GET /api/me/fivehour-weights: the learned weights of the caller's own subscription, cached; {}
// for an API key; "not enough data" below the gate; and the window's pings with since.
func TestMeFiveHourWeightsEndpoint(t *testing.T) {
	db := openTestDB(t)
	rows := fhSim{w: realW, windows: 8, perWindow: 400, seed: 21}.rows()
	evs := append(fhEvents(rows, "a", true), fhEvents(rows, "c", false)...)
	evs = append(evs, fhEvents(fhSim{w: realW, windows: 1, perWindow: 100, seed: 22}.rows(), "thin", true)...)
	ping := &Event{TS: time.Now().Add(-30 * time.Minute).UnixMilli(), TenantID: "a", SessionID: "s", Model: "m",
		Provider: "anthropic", KeepAlive: true, CacheRead: 90_000, OutputTokens: 1}
	evs = append(evs, ping)
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{modelinfo.Price{Input: 3e-6, Output: 15e-6}})
	mux := http.NewServeMux()
	tenant := "a"
	api.SetAuth(func(r *http.Request) (Principal, bool) { return Principal{TenantID: tenant, Manager: true}, true })
	api.Mount(mux)
	get := func(q string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/me/fivehour-weights"+q, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET = %d %s", rr.Code, rr.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A manager's ?tenant=* still reads their own account.
	got := get("?tenant=*")
	if got["ok"] != true {
		t.Fatalf("tenant a: %v", got)
	}
	ws, _ := got["weights"].(map[string]any)
	if e := fhRelErr(ws["cache_write_5m"].(float64), realW[fhWrite5m]); e > 0.15 {
		t.Errorf("cache_write_5m weight off by %.0f%%: %v", 100*e, ws)
	}
	if _, ok := got["window_ping"]; ok {
		t.Error("window_ping without since")
	}
	since := time.Now().Add(-5 * time.Hour).UnixMilli()
	got = get("?since=" + jsonNum(since))
	wp, _ := got["window_ping"].(map[string]any)
	if wp["cache_read"] != float64(90_000) || wp["output"] != float64(1) {
		t.Errorf("window_ping = %v, want the ping's 90,000 rd and 1 output", got["window_ping"])
	}
	// Cached: a new row does not change the answer within the cache's life.
	before := got["requests"]
	if err := db.insertBatch(fhEvents(fhSim{w: realW, windows: 2, perWindow: 400, seed: 23}.rows(), "a", true)); err != nil {
		t.Fatal(err)
	}
	if after := get("")["requests"]; after != before {
		t.Errorf("the fit ran again inside the cache's life: %v then %v", before, after)
	}
	tenant = "c"
	if got := get(""); len(got) != 0 {
		t.Errorf("an API key: %v, want {}", got)
	}
	tenant = "thin"
	got = get("?since=" + jsonNum(since))
	if got["ok"] != false || got["reason"] == "" || got["weights"] != nil || got["window_ping"] != nil {
		t.Errorf("thin history: %v, want not enough data and no weights", got)
	}
}

func jsonNum(v int64) string { b, _ := json.Marshal(v); return string(b) }

// The forecast at the learned weights: the same replay, scaled. Scale 1 is the API-price forecast;
// costly pings turn a yes into a refusal; a fit below the gate adds nothing.
func TestRecommendationLearnedForecast(t *testing.T) {
	fx := newKAFixture(t, kaSpread(t, 40, 700, 1.0)...)
	f, pol := Filter{TenantAll: true}, keepalive.Policy{}
	plain, err := fx.db.KeepAliveRecommend(f, pol)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Refused != "" || plain.Learned != nil {
		t.Fatalf("the fixture must give a recommendation: %+v", plain)
	}
	fit := FiveHourFit{OK: true, Requests: 900, Windows: 4}
	fit.Forecast = &ForecastScale{Save5m: 1, Save1h: 1, Ping: 1}
	same, err := fx.db.KeepAliveRecommendLearned(f, pol, fit)
	if err != nil {
		t.Fatal(err)
	}
	if l := same.Learned; l == nil || l.Refused != "" || l.LoUSD != plain.LoUSD || l.HiUSD != plain.HiUSD ||
		l.MaxPings != plain.MaxPings || l.Requests != 900 || l.Windows != 4 {
		t.Errorf("scale 1 gave %+v, want the API-price range [%v, %v]", same.Learned, plain.LoUSD, plain.HiUSD)
	}
	if same.LoUSD != plain.LoUSD || same.HiUSD != plain.HiUSD {
		t.Error("the learned forecast changed the API-price one")
	}
	fit.Forecast = &ForecastScale{Save5m: 0.01, Save1h: 0.01, Ping: 50}
	costly, err := fx.db.KeepAliveRecommendLearned(f, pol, fit)
	if err != nil {
		t.Fatal(err)
	}
	if l := costly.Learned; l == nil || !strings.Contains(l.Refused, "COST") || l.LoUSD != 0 || l.MaxPings != 0 {
		t.Errorf("pings that weigh 50 times their price: %+v, want a COST refusal and no range", costly.Learned)
	}
	if costly.Refused != "" {
		t.Error("the learned refusal leaked into the API-price forecast")
	}
	for _, bad := range []FiveHourFit{{}, {OK: true}} {
		r, err := fx.db.KeepAliveRecommendLearned(f, pol, bad)
		if err != nil || r.Learned != nil {
			t.Errorf("fit %+v: learned %+v (%v), want none", bad, r.Learned, err)
		}
	}
}
