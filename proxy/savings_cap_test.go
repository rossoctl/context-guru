package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// oneMillionWindow resolves every model to a 1M window, exactly, so the cap can run.
type oneMillionWindow struct{}

func (oneMillionWindow) Window(context.Context, string) (int, bool) { return 1_000_000, true }
func (oneMillionWindow) WindowExact(context.Context, string) (int, bool, bool) {
	return 1_000_000, true, true
}

// cappedSession writes a session whose offloader credit runs past the cap: billed 500k plus 500k
// removed reaches the 1M ceiling from the second turn on.
func cappedSession(session string, n int) []*dash.Event {
	var evs []*dash.Event
	for i := 0; i < n; i++ {
		evs = append(evs, &dash.Event{
			TS: int64(86_400_000 + i + 1), SessionID: session, Model: "aws/claude-sonnet-5", Provider: "anthropic",
			Status: 200, TokensBefore: 800_000 + i*10_000, TokensAfter: 300_000 + i*10_000,
			FreshInput: 10, CacheRead: 500_000, CostUSD: 1, BaselineCostUSD: 2,
			TokenAccounting: dash.AccountingComplete, CacheMissReason: dash.CacheHit,
			Components: []dash.CompRow{{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true,
				SavedGross: 500_000, SavedUSD: 1, Events: map[string]int{offload.EventFreshSummary: 0}}},
		})
	}
	return evs
}

func capTestHandler(t *testing.T, windows bool) (*Handler, *dash.Recorder) {
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
	opts := Options{Preset: cfg.Preset, Dashboard: rec, Prices: fixedPricer{}}
	if windows {
		opts.Windows = oneMillionWindow{}
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, opts)
	t.Cleanup(h.Close)
	return h, rec
}

type statsScopes struct {
	Savings struct {
		Current *SavingsScope `json:"current"`
		All     *SavingsScope `json:"all"`
	} `json:"savings"`
}

func getStats(t *testing.T, srv *httptest.Server) statsScopes {
	t.Helper()
	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s statsScopes
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The status line's session scope is capped, the lifetime scope says it is not, and the session scope
// equals what the dashboard's own query gives for the same rows (#586).
func TestStatsSavingsCapsTheSessionScopeAndLabelsTheLifetimeScope(t *testing.T) {
	h, rec := capTestHandler(t, true)
	if err := rec.DB().InsertEvents(cappedSession("b", 5)); err != nil {
		t.Fatal(err)
	}
	h.setLastSession("b")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	got := getStats(t, srv)
	if got.Savings.Current == nil || got.Savings.All == nil {
		t.Fatalf("a scope is missing: %+v", got.Savings)
	}
	want, err := rec.DB().SavingsTotalsCapped(dash.Filter{TenantAll: true, Session: "b"}, oneMillionWindow{}, fixedPricer{})
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := rec.DB().SavingsTotals(dash.Filter{TenantAll: true, Session: "b"})
	if want.CapCorrectionUSD >= 0 {
		t.Fatalf("fixture has no credit past the cap (correction %v) — the test proves nothing", want.CapCorrectionUSD)
	}
	cur := got.Savings.Current
	if cur.CapNotApplied || cur.CapCorrectionUSD != want.CapCorrectionUSD || cur.TotalSavedUSD != want.TotalSavedUSD {
		t.Errorf("current = {corr %v total %v not_applied %v}, want the capped {corr %v total %v}",
			cur.CapCorrectionUSD, cur.TotalSavedUSD, cur.CapNotApplied, want.CapCorrectionUSD, want.TotalSavedUSD)
	}
	if !(cur.TotalSavedUSD < plain.TotalSavedUSD) {
		t.Errorf("capped total %v is not below the uncapped %v", cur.TotalSavedUSD, plain.TotalSavedUSD)
	}
	// The lifetime scope keeps the stored credit and says so.
	if !got.Savings.All.CapNotApplied || got.Savings.All.CapCorrectionUSD != 0 {
		t.Errorf("all = {corr %v not_applied %v}, want 0 and true", got.Savings.All.CapCorrectionUSD, got.Savings.All.CapNotApplied)
	}
	if got.Savings.All.TotalSavedUSD != plain.TotalSavedUSD {
		t.Errorf("all total %v, want the stored %v", got.Savings.All.TotalSavedUSD, plain.TotalSavedUSD)
	}
}

// Without a window resolver the cap cannot run: the session scope keeps the stored figure and says so.
func TestStatsSavingsLabelsTheSessionScopeWithoutAWindowResolver(t *testing.T) {
	h, rec := capTestHandler(t, false)
	if err := rec.DB().InsertEvents(cappedSession("b", 5)); err != nil {
		t.Fatal(err)
	}
	h.setLastSession("b")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	cur := getStats(t, srv).Savings.Current
	if cur == nil || !cur.CapNotApplied || cur.CapCorrectionUSD != 0 {
		t.Errorf("current = %+v, want cap_not_applied and 0", cur)
	}
}

// The correction is reused for capCacheTTL, so a status line that refreshes often does not stream the
// session's rows every time; the totals under it stay live.
func TestStatsSavingsReusesTheCorrectionButNotTheTotals(t *testing.T) {
	h, rec := capTestHandler(t, true)
	if err := rec.DB().InsertEvents(cappedSession("b", 5)); err != nil {
		t.Fatal(err)
	}
	h.setLastSession("b")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	first := getStats(t, srv).Savings.Current
	// A new capped turn lands. Cost is live, the correction is the cached one.
	more := cappedSession("b", 6)[5:]
	if err := rec.DB().InsertEvents(more); err != nil {
		t.Fatal(err)
	}
	second := getStats(t, srv).Savings.Current
	if second.CostUSD <= first.CostUSD {
		t.Errorf("cost_usd %v did not move with the new turn (was %v): the totals must stay live", second.CostUSD, first.CostUSD)
	}
	if second.CapCorrectionUSD != first.CapCorrectionUSD {
		t.Errorf("correction changed inside the TTL: %v then %v", first.CapCorrectionUSD, second.CapCorrectionUSD)
	}
	// Past the TTL it is recomputed.
	h.capCache.mu.Lock()
	for k, e := range h.capCache.entries {
		e.at = e.at.Add(-2 * capCacheTTL)
		h.capCache.entries[k] = e
	}
	h.capCache.mu.Unlock()
	third := getStats(t, srv).Savings.Current
	if third.CapCorrectionUSD == first.CapCorrectionUSD {
		t.Errorf("correction still %v after the TTL with a new capped turn", third.CapCorrectionUSD)
	}
}

func TestCapKeyIgnoresTheOrderOfLiveSessions(t *testing.T) {
	a := capKey(dash.Filter{SessionIn: []string{"x", "y", "z"}})
	b := capKey(dash.Filter{SessionIn: []string{"z", "x", "y"}})
	if a != b {
		t.Errorf("same set, different keys: %q vs %q", a, b)
	}
	in := []string{"z", "x"}
	capKey(dash.Filter{SessionIn: in})
	if in[0] != "z" {
		t.Error("capKey sorted the caller's slice")
	}
}
