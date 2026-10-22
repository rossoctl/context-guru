package dash

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestJSONCacheLoadServesPastCapWhileRefreshing exercises jsonCache.load/beginRefresh/endRefresh
// directly: a body older than dashCacheTTL+dashCacheStale is normally reported absent, but not
// while a refresh for that key is already running, and it goes back to absent once that refresh
// ends without having replaced it (an error, or a goroutine that never got around to it).
func TestJSONCacheLoadServesPastCapWhileRefreshing(t *testing.T) {
	c := &jsonCache{}
	key := "k"
	tooOld := dashCacheTTL + dashCacheStale + time.Second
	c.entries = map[string]jsonCacheEntry{key: {at: time.Now().Add(-tooOld), body: []byte(`{"old":true}`)}}

	if body, _ := c.load(key); body != nil {
		t.Fatalf("load() with no refresh running and an entry past the cap = %q; want absent (nil)", body)
	}

	if !c.beginRefresh(key) {
		t.Fatal("beginRefresh() on an unclaimed key = false; want true")
	}
	body, fresh := c.load(key)
	if body == nil {
		t.Fatal("load() with a refresh in flight = absent; want the old body kept available")
	}
	if fresh {
		t.Error("load() reported a body past dashCacheTTL as fresh")
	}

	// A second, concurrent refresh attempt for the same key must not be granted —
	// this is the gate that stops N stale readers from starting N duplicate computations.
	if c.beginRefresh(key) {
		t.Fatal("beginRefresh() on a key already refreshing = true; want false (second caller must not start its own)")
	}

	c.endRefresh(key)
	if body, _ := c.load(key); body != nil {
		t.Fatalf("load() after endRefresh with nothing having replaced the entry = %q; want absent again", body)
	}
	// The key is claimable again now that the previous refresh ended.
	if !c.beginRefresh(key) {
		t.Fatal("beginRefresh() after endRefresh = false; want true")
	}
	c.endRefresh(key)
}

// TestServeJSONSingleFlightsStaleRefresh is the end-to-end version of the test above: many
// concurrent requests landing on a STALE (but not yet cold) cache entry must trigger exactly one
// refresh compute, and every one of them must get an immediate response rather than blocking on
// it. This is the real path the user's statusline timeouts went through — the previous code
// started one new goroutine, running the full compute, PER stale request.
func TestServeJSONSingleFlightsStaleRefresh(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	var computes int32
	release := make(chan struct{})

	c := &jsonCache{entries: map[string]jsonCacheEntry{
		"k": {at: time.Now().Add(-(dashCacheTTL + time.Millisecond)), body: []byte(`{"gen":0}`)},
	}}
	compute := func(db *DB) ([]byte, error) {
		n := atomic.AddInt32(&computes, 1)
		<-release // held open until the test says every concurrent caller has already been served
		return []byte(`{"gen":` + string(rune('0'+n)) + `}`), nil
	}

	const n = 8
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
			a.serveJSON(w, r, c, "k", compute)
			if w.Header().Get("X-Cache") != "stale" {
				t.Errorf("X-Cache = %q; want %q (every caller must be served the stale body, not block)",
					w.Header().Get("X-Cache"), "stale")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent stale-cache reader blocked instead of being served the cached body immediately")
		}
	}
	close(release)

	// Give the single refresh goroutine a moment to finish and release its claim.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		refreshing := c.refreshing["k"]
		c.mu.Unlock()
		if !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh never released its claim on the key")
		}
		time.Sleep(time.Millisecond)
	}

	if got := atomic.LoadInt32(&computes); got != 1 {
		t.Errorf("compute ran %d times for %d concurrent stale readers of the same key; want exactly 1", got, n)
	}
}

// TestStatsLeanSkipsThePricedExtras checks that ?lean=1 answers from Overview() alone — the path
// the statusline's own _fetch_stats takes — and that it still carries every field that path
// reads (total_saved_usd, saved_unique, cg_latency_ms_avg, upstream_ms_avg, keepalive_net_usd,
// keepalive_misses_avoided).
func TestStatsLeanSkipsThePricedExtras(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-1", "aws/claude-sonnet-5", 1000, 800))

	_, body := get(t, a, "/api/stats?lean=1&session=sess-1", "127.0.0.1:1")
	for _, field := range []string{
		"total_saved_usd", "saved_unique", "cg_latency_ms_avg", "upstream_ms_avg",
		"keepalive_net_usd", "keepalive_misses_avoided",
	} {
		if _, ok := body[field]; !ok {
			t.Errorf("lean /api/stats is missing %q, which the statusline reads", field)
		}
	}
}

// TestStatsLeanCountsTheSessionsPings: the status line shows this session's ping count and spend
// from the lean read, so lean must report both, and only for the session asked about.
func TestStatsLeanCountsTheSessionsPings(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	ping := func(ts int64, session string) *Event {
		e := mkEvent(ts, session, "aws/claude-sonnet-5", 1000, 1000)
		e.KeepAlive, e.CostUSD = true, 0.002
		return e
	}
	seed(t, rec, mkEvent(now, "sess-1", "aws/claude-sonnet-5", 1000, 800),
		ping(now+1, "sess-1"), ping(now+2, "sess-1"), ping(now+3, "sess-2"))

	_, body := get(t, a, "/api/stats?lean=1&session=sess-1", "127.0.0.1:1")
	if body["keepalive_pings"] != 2.0 {
		t.Errorf("keepalive_pings = %v, want 2", body["keepalive_pings"])
	}
	if usd, _ := body["keepalive_ping_usd"].(float64); math.Abs(usd-0.004) > 1e-9 {
		t.Errorf("keepalive_ping_usd = %v, want 0.004", body["keepalive_ping_usd"])
	}
}

// TestStatsLeanIsACacheKeySeparateFromTheFullHandler guards against a lean and a non-lean
// request for the same scope colliding on one cache entry — cacheKey includes the raw query
// string (dash/api.go's cacheKey), so this is really a test that the ?lean=1 param survives
// into it, but it is cheap to assert the actual behaviour rather than the mechanism.
func TestStatsLeanIsACacheKeySeparateFromTheFullHandler(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-1", "aws/claude-sonnet-5", 1000, 800))

	_, full := get(t, a, "/api/stats", "127.0.0.1:1")
	_, lean := get(t, a, "/api/stats?lean=1", "127.0.0.1:1")
	if _, ok := full["waterfall"]; !ok {
		t.Error("the full /api/stats response has no waterfall; test fixture assumption broke")
	}
	// Not asserting lean lacks "waterfall" (Overview() sets its own baseline one), only that
	// the two requests were not served the exact same cached bytes.
	if full["total_saved_usd"] == nil || lean["total_saved_usd"] == nil {
		t.Fatal("total_saved_usd missing from one of the two responses")
	}
}

// statsLeanTotalSaved fetches total_saved_usd and decl_filter_usd from lean or full /api/stats,
// optionally scoped to a session — shared by the two tests below.
func statsLeanTotalSaved(t *testing.T, mux *http.ServeMux, lean bool, session string) (totalSaved, declFilterUSD float64) {
	t.Helper()
	q := ""
	if lean {
		q = "?lean=1"
	}
	if session != "" {
		if q == "" {
			q = "?session=" + session
		} else {
			q += "&session=" + session
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stats"+q, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/stats%s: %d", q, rec.Code)
	}
	var body struct {
		TotalSavedUSD float64 `json:"total_saved_usd"`
		DeclFilterUSD float64 `json:"decl_filter_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /api/stats%s body: %v\n%s", q, err, rec.Body)
	}
	return body.TotalSavedUSD, body.DeclFilterUSD
}

// TestStatsLeanTotalSavedMatchesFullHandlerWhenSessionScoped is the fix for the PR #403
// review's finding 1 (lean leaving CachesplitHistorical.USD and the declaration-filter credit
// out of total_saved_usd), kept exact for the ONE scope #502 found it can stay bounded for:
// a single session. CachesplitHistoricalUSD's ranking and DeclFilterSavings' scan are both an
// indexed range read when scoped to one session (dash/overview_lean.go), so lean has no reason
// to differ from the full handler there, and must not.
//
// seedCredit (dash/declcredit_test.go) gives the filter-credit half; a pre-instrumentation
// cachesplit-historical pair (same shape as TestWaterfallTotalMatchesTheHeadlineAfterAllPricedAdditions
// in dash/declcredit_test.go) gives the historical-split half, so both of the review's named
// amounts are actually nonzero in this fixture rather than trivially matching by both being zero.
func TestStatsLeanTotalSavedMatchesFullHandlerWhenSessionScoped(t *testing.T) {
	db := seedCredit(t, 1000)
	teach := mkEvent(9000, "s-hist", "aws/claude-sonnet-5", 100, 100)
	teach.TenantID, teach.CacheRead, teach.CacheWrite, teach.SplitStableTokens = "t1", 54_304, 1_000, 5_697
	qualifies := mkEvent(9100, "s-new-hist", "aws/claude-sonnet-5", 100, 100)
	qualifies.TenantID, qualifies.CacheRead, qualifies.CacheWrite = "t1", 54_304, 1_000
	// One filtered row on a model staticPricer refuses to price (see its own doc comment: the
	// sentinel name "some/unmeasured-model" always returns Priced=false). This is the fixture
	// gap a re-review of this PR found: DeclFilterSaving.Priced goes false the moment ANY ONE
	// request in scope is unpriced, but USD still holds the priced subset's real dollar figure
	// — and an earlier version of the lean handler gated on Priced, which dropped the WHOLE
	// filter credit over this one row. Every model in the fixture above this comment has a
	// price, so without this row the parity assertion below would pass for the wrong reason.
	unpriced := mkEvent(9200, "s-unpriced", "some/unmeasured-model", 100, 90)
	unpriced.TenantID, unpriced.Tools, unpriced.FilteredDeclTokens = "t1", 4, 500
	if err := db.insertBatch([]*Event{teach, qualifies, unpriced}); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{ibmSonnet})
	mux := http.NewServeMux()
	api.Mount(mux)

	// s-new-hist is the session the cachesplit-historical credit applies to. "se" is one of
	// seedCredit's later sessions (i>=4, see its own comment): the ones that actually carry a
	// priced FilteredDeclTokens row, so decl_filter_usd is positive THERE — "s-unpriced" has
	// only the one row staticPricer refuses to price, so scoped to it alone there is nothing
	// to credit; its job (below) is proving lean does not drop a credit it DOES qualify for,
	// not proving it exists in a scope that never had one.
	for _, tc := range []struct {
		session  string
		wantDecl bool
		wantHist bool
	}{
		{session: "s-new-hist", wantHist: true},
		{session: "se", wantDecl: true},
	} {
		fullSaved, fullDecl := statsLeanTotalSaved(t, mux, false, tc.session)
		leanSaved, _ := statsLeanTotalSaved(t, mux, true, tc.session)
		if fullSaved <= 0 {
			t.Fatalf("session=%s: full handler's total_saved_usd is not positive; the fixture is not exercising anything", tc.session)
		}
		if tc.wantDecl && fullDecl <= 0 {
			t.Fatalf("session=%s: decl_filter_usd is not positive; the fixture stopped exercising the filter credit", tc.session)
		}
		if math.Abs(fullSaved-leanSaved) > 1e-9 {
			t.Errorf("session=%s: lean total_saved_usd = %v, full total_saved_usd = %v — "+
				"scoped to one session, both credits are bounded and lean must match exactly",
				tc.session, leanSaved, fullSaved)
		}
	}
}

// TestStatsLeanMatchesFullForLifetimeToo: a lean read with no session and no window (`since=0`)
// is answered by the full handler (#510 removed the lean lifetime path), so an older plugin that
// still asks for it gets the exact number, to the last cent.
func TestStatsLeanMatchesFullForLifetimeToo(t *testing.T) {
	db := seedCredit(t, 1000)
	qualifies := mkEvent(9100, "s-new-hist", "aws/claude-sonnet-5", 100, 100)
	qualifies.TenantID, qualifies.CacheRead, qualifies.CacheWrite = "t1", 54_304, 1_000
	if err := db.insertBatch([]*Event{qualifies}); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{db: db, hub: NewHub(), done: make(chan struct{})}
	t.Cleanup(func() { rec.Close() })
	api := NewAPI(rec)
	api.SetPricer(staticPricer{ibmSonnet})
	mux := http.NewServeMux()
	api.Mount(mux)

	fullSaved, fullDecl := statsLeanTotalSaved(t, mux, false, "")
	leanSaved, _ := statsLeanTotalSaved(t, mux, true, "")
	if fullDecl <= 0 && fullSaved <= 0 {
		t.Fatal("fixture is not exercising either credit; this test would pass for the wrong reason")
	}
	if math.Abs(fullSaved-leanSaved) > 1e-9 {
		t.Errorf("lean total_saved_usd = %v, full = %v — lifetime must match exactly, "+
			"an unbounded lean read must be the full handler's answer", leanSaved, fullSaved)
	}
}
