package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
)

// savingsUpstream is a fixed Anthropic-shaped response with cache tokens big enough to
// clear kaPolicy()'s (lowered) MinPrefixTokens, so a real request through it both prices
// out to a nonzero saving AND is eligible for keep-alive tracking.
func savingsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":12,"output_tokens":34,
		           "cache_read_input_tokens":9000,"cache_creation_input_tokens":1500}}`))
	}))
}

// savingsTestHandler wires a real pipeline + recorder + a low-threshold keep-alive policy
// (so two turns on a session land it in the keeper's live set) in front of a fake upstream.
func savingsTestHandler(t *testing.T, up string) (*Handler, *dash.Recorder) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up,
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
		Cache: CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
			MaxUSDPerPing: 0.25, MinPrefixTokens: 100},
	})
	t.Cleanup(h.Close)
	return h, rec
}

func sendReal(t *testing.T, srv *httptest.Server, session string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", session)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d for session %s", resp.StatusCode, session)
	}
}

// TestStatsSavingsThreeScopesAgreeWithOverview drives real traffic on two sessions and
// checks /stats' three savings scopes against dash.DB.SavingsTotals computed independently
// with the equivalent Filter, so the surface and the underlying query cannot silently
// diverge.
func TestStatsSavingsThreeScopesAgreeWithOverview(t *testing.T) {
	up := savingsUpstream(t)
	defer up.Close()
	h, rec := savingsTestHandler(t, up.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// Two turns each, so both sessions clear the keeper's turn>=1 gate and land in k.live.
	// Session b goes last, so it — and only it — is "current".
	sendReal(t, srv, "a")
	sendReal(t, srv, "a")
	sendReal(t, srv, "b")
	sendReal(t, srv, "b")
	waitForRows(t, rec, 4)

	if live := h.keeper.LiveSessionKeys(); len(live) != 2 {
		t.Fatalf("keeper tracks %d sessions, want 2 (a and b): %v", len(live), live)
	}

	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Savings *struct {
			Current *SavingsScope `json:"current"`
			Live    *SavingsScope `json:"live"`
			All     *SavingsScope `json:"all"`
		} `json:"savings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("/stats is not the expected shape: %v", err)
	}
	if got.Savings == nil {
		t.Fatal("/stats has no savings block")
	}
	if got.Savings.Current == nil || got.Savings.Live == nil || got.Savings.All == nil {
		t.Fatalf("a savings scope is missing: %+v", got.Savings)
	}

	wantCurrent, err := rec.DB().SavingsTotals(dash.Filter{TenantAll: true, Session: "b"})
	if err != nil {
		t.Fatal(err)
	}
	wantAll, err := rec.DB().SavingsTotals(dash.Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	// The same grouped query /stats' "live" scope itself runs (Filter.SessionIn) — summing
	// two independently-computed per-session queries in Go can legitimately land on a
	// different last float64 bit than one SQL SUM over both sessions' rows together.
	wantLive, err := rec.DB().SavingsTotals(dash.Filter{TenantAll: true, SessionIn: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}

	if got.Savings.Current.TotalSavedUSD != wantCurrent.TotalSavedUSD {
		t.Errorf("current.total_saved_usd = %v, want session b's own %v",
			got.Savings.Current.TotalSavedUSD, wantCurrent.TotalSavedUSD)
	}
	if got.Savings.Live.TotalSavedUSD != wantLive.TotalSavedUSD {
		t.Errorf("live.total_saved_usd = %v, want a+b grouped = %v",
			got.Savings.Live.TotalSavedUSD, wantLive.TotalSavedUSD)
	}
	if got.Savings.All.TotalSavedUSD != wantAll.TotalSavedUSD {
		t.Errorf("all.total_saved_usd = %v, want the whole DB's %v",
			got.Savings.All.TotalSavedUSD, wantAll.TotalSavedUSD)
	}
	if got.Savings.All.TotalSavedUSD == 0 {
		t.Fatal("fixture produced no savings at all — the equality assertions above prove nothing")
	}
}

// TestStatsSavingsCurrentAbsentBeforeAnyRealRequest pins the nil-vs-zero rule: a handler
// that has served nothing yet reports no "current" scope, not a zeroed one.
func TestStatsSavingsCurrentAbsentBeforeAnyRealRequest(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: rec})
	defer h.Close()

	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	var got struct {
		Savings *struct {
			Current *SavingsScope `json:"current"`
			Live    *SavingsScope `json:"live"`
		} `json:"savings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Savings == nil {
		t.Fatal("with a dashboard and no traffic, /stats should still report an (empty) savings block")
	}
	if got.Savings.Current != nil {
		t.Errorf("current = %+v, want absent — no real request has arrived", got.Savings.Current)
	}
	if got.Savings.Live != nil {
		t.Errorf("live = %+v, want absent — the keeper tracks nothing yet", got.Savings.Live)
	}
}

// TestStatsSavingsAbsentWithoutADashboard pins the fail-open shape: no rec means no
// savings block at all, never one full of zeros that would misrepresent "no data" as
// "measured zero".
func TestStatsSavingsAbsentWithoutADashboard(t *testing.T) {
	h := New(nil, nil, metrics.NewAggregator(), Options{})
	defer h.Close()
	w := httptest.NewRecorder()
	h.stats(w, httptest.NewRequest("GET", "/stats", nil))
	var got struct {
		Savings json.RawMessage `json:"savings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Savings != nil {
		t.Errorf("savings = %s, want the field entirely absent with no dashboard wired", got.Savings)
	}
}

// TestStatsSavingsAbsentInHostedMode pins the guard shared with snap.Pipeline: a
// service-wide savings figure through the request path belongs on the manager dashboard,
// not on a tenant's own /stats.
func TestStatsSavingsAbsentInHostedMode(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	t.Setenv(envMailDevSink, filepath.Join(t.TempDir(), "mail.txt"))
	reg, err := tenant.Open("", tenant.Options{ManagerEmail: "boss@ibm.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: rec, Tenants: NewTenantSource(reg, nil, nil, 0)})
	defer h.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/stats", nil)
	r.RemoteAddr = "127.0.0.1:5555" // loopback, so statsTrusted lets the request through
	h.stats(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/stats from loopback in hosted mode = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got struct {
		Savings json.RawMessage `json:"savings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Savings != nil {
		t.Errorf("savings = %s, want absent in hosted mode", got.Savings)
	}
}

// TestLiveSessionKeysIsACopy pins the doc comment's own claim: the slice LiveSessionKeys
// returns is a copy, so a caller mutating it can never corrupt the keeper's live map.
func TestLiveSessionKeysIsACopy(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: rec})
	defer h.Close()
	k := h.keeper
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 100}
	recordOne(t, k, pol, kaBody, time.Now(), upstream{base: "http://example.invalid"})

	keys := k.LiveSessionKeys()
	if len(keys) != 1 {
		t.Fatalf("got %d live sessions, want 1", len(keys))
	}
	keys[0] = "corrupted"
	again := k.LiveSessionKeys()
	if len(again) != 1 || again[0] == "corrupted" {
		t.Errorf("mutating the returned slice reached the keeper's own state: %v", again)
	}
}
