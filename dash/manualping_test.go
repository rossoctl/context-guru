package dash

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// manualRow is a ping the USER asked for: a keep-alive row tagged "manual", with the cache write
// it avoided stored beside the cost it paid.
func manualRow(ts int64, session string, cost, saved float64) *Event {
	e := mkEvent(ts, session, "aws/claude-sonnet-5", 100, 100)
	e.KeepAlive, e.ManualPing, e.KeepAliveStrategyID = true, true, "manual"
	e.Status, e.CostUSD, e.BaselineCostUSD, e.ManualSavedUSD = 200, cost, 0, saved
	e.CacheRead, e.CacheWrite = 5000, 0
	return e
}

func autoPingRow(ts int64, session string, cost float64) *Event {
	e := mkEvent(ts, session, "aws/claude-sonnet-5", 100, 100)
	e.KeepAlive, e.KeepAliveStrategyID = true, "strat-1"
	e.Status, e.CostUSD, e.BaselineCostUSD = 200, cost, 0
	e.CacheRead, e.CacheWrite = 5000, 0
	return e
}

func TestManualPingSurvivesTheDatabase(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{manualRow(1000, "s1", 0.01, 0.15)}); err != nil {
		t.Fatal(err)
	}
	p, err := db.Requests(Filter{TenantAll: true}, 0, 10)
	if err != nil || len(p.Requests) != 1 {
		t.Fatalf("rows = %v, err %v", p, err)
	}
	e := p.Requests[0]
	if !e.KeepAlive || !e.ManualPing || !near(e.ManualSavedUSD, 0.15) {
		t.Errorf("round trip lost the manual ping: %+v", e)
	}
	// The strategy id is a SQL-only column (nothing scans it back), so read it there.
	var id string
	if err := db.sql.QueryRow(`SELECT keepalive_strategy_id FROM requests`).Scan(&id); err != nil || id != "manual" {
		t.Errorf("stored strategy id = %q (%v), want \"manual\"", id, err)
	}
}

// A manual ping is its own category. It must reach /api/stats as manual_pings, leave the
// automatic keep-alive ledger alone, and add its NET (saved minus cost) to the total exactly once.
func TestStatsReportsManualPingsApartFromTheKeepAlive(t *testing.T) {
	for _, path := range []string{"/api/stats?session=s1", "/api/stats?lean=1&session=s1"} {
		t.Run(path, func(t *testing.T) {
			a, rec := newTestAPI(t, Options{})
			now := time.Now().UnixMilli()
			real := mkEvent(now-60_000, "s1", "aws/claude-sonnet-5", 100, 90) // cost .01, baseline .02 -> net .01
			real.Status = 200
			seed(t, rec, real,
				autoPingRow(now-50_000, "s1", 0.02),
				manualRow(now-40_000, "s1", 0.01, 0.15),
				manualRow(now-30_000, "s1", 0.01, 0.05),
				manualRow(now-20_000, "other", 0.5, 9)) // another session: out of scope
			_, body := get(t, a, path, "")
			mp, _ := body["manual_pings"].(map[string]any)
			if mp == nil {
				t.Fatalf("no manual_pings in %v", body)
			}
			if mp["count"].(float64) != 2 || !near(mp["ping_usd"].(float64), 0.02) || !near(mp["saved_usd"].(float64), 0.18) {
				t.Errorf("manual_pings = %v, want count 2, ping_usd 0.02, saved_usd 0.18 (net of the pings)", mp)
			}
			if mp["writes_avoided"].(float64) != 2 {
				t.Errorf("manual_pings.writes_avoided = %v, want 2: both pings avoided a write", mp["writes_avoided"])
			}
			if body["keepalive_pings"].(float64) != 1 || !near(body["keepalive_ping_usd"].(float64), 0.02) {
				t.Errorf("the automatic keep-alive ledger counted manual pings: pings %v usd %v",
					body["keepalive_pings"], body["keepalive_ping_usd"])
			}
			// net .01 + keep-alive net (0 - .02) + manual net (saved_usd .18, already net of its .02)
			if got := body["total_saved_usd"].(float64); !near(got, 0.17) {
				t.Errorf("total_saved_usd = %v, want 0.17", got)
			}
		})
	}
}

// With no manual ping the field is still there, zeroed: a plugin reads one shape.
func TestStatsAlwaysCarriesManualPings(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	e := mkEvent(time.Now().UnixMilli(), "s1", "aws/claude-sonnet-5", 100, 90)
	e.Status = 200
	seed(t, rec, e)
	_, body := get(t, a, "/api/stats?lean=1&session=s1", "")
	mp, ok := body["manual_pings"].(map[string]any)
	if !ok || mp["count"].(float64) != 0 || mp["ping_usd"].(float64) != 0 || mp["saved_usd"].(float64) != 0 {
		t.Errorf("manual_pings = %v, want zeros", body["manual_pings"])
	}
}

// The strategy ledgers and the keep-alive tab are about the CACHE STRATEGY. A user's own ping is
// not that strategy's spend, and must not lower its net.
func TestManualPingsStayOutOfTheStrategyLedgers(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	if err := db.insertBatch([]*Event{
		autoPingRow(now-3000, "s1", 0.02),
		manualRow(now-2000, "s1", 0.30, 1.0),
	}); err != nil {
		t.Fatal(err)
	}
	led, err := db.KeepAliveLedger(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if led.Pings != 1 || !near(led.PingUSD, 0.02) {
		t.Errorf("keep-alive ledger = %d pings, $%v; want the one automatic ping, $0.02", led.Pings, led.PingUSD)
	}
	for _, id := range []string{"strat-1", "manual"} {
		v, err := db.StrategyLedger(id)
		if err != nil {
			t.Fatal(err)
		}
		if id == "strat-1" && (v.Pings != 1 || !near(v.PingUSD, 0.02)) {
			t.Errorf("strat-1 ledger = %+v", v)
		}
		if id == "manual" && v.Pings != 0 {
			t.Errorf("\"manual\" is not a strategy and has no ledger, got %+v", v)
		}
	}
	rows, _, err := db.Sessions(Filter{TenantAll: true}, 10, 0)
	if err == nil {
		for _, s := range rows {
			if s.SessionID == "s1" && (s.KeepAlivePings != 1 || !near(s.KeepAlivePingUSD, 0.02)) {
				t.Errorf("session row counts the manual ping as keep-alive: %+v", s)
			}
		}
	}
}

// SavingsTotals (the proxy's /stats block) and Overview (the dashboard) must still agree.
func TestSavingsTotalsAgreesWithOverviewOnManualPings(t *testing.T) {
	db := openTestDB(t)
	real := mkEvent(1, "s", "m", 100, 90)
	if err := db.insertBatch([]*Event{real, autoPingRow(2, "s", 0.02), manualRow(3, "s", 0.01, 0.15)}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := db.SavingsTotals(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !near(o.TotalSavedUSD, s.TotalSavedUSD) || !near(o.ManualPings.SavedUSD, 0.14) || !near(s.ManualPings.SavedUSD, 0.14) ||
		o.ManualPings.Count != 1 || s.ManualPings.Count != 1 || !near(o.KeepAlivePingUSD, s.KeepAlivePingUSD) {
		t.Errorf("Overview %+v / SavingsTotals %+v disagree", o.ManualPings, s.ManualPings)
	}
}

// The session's `saved $x of $y` follows the same arithmetic, so pressing Ping moves it.
func TestCacheStateMoneyIncludesTheManualPingNet(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	real := mkEvent(now-60_000, "s1", "aws/claude-sonnet-5", 100, 100) // cost .01 baseline .02
	real.Status = 200
	seed(t, rec, real, manualRow(now-30_000, "s1", 0.01, 0.15))
	_, body := get(t, a, "/api/cachestate?session=s1", "")
	m := body["money"].(map[string]any)["main"].(map[string]any)
	// baseline .02 - cost .01 + (saved .15 - ping .01) = .15 ; y = .02 + .15
	if !near(m["saved_usd"].(float64), 0.15) || !near(m["baseline_usd"].(float64), 0.17) {
		t.Errorf("main money = %v, want saved 0.15 of 0.17", m)
	}
}

// §2: the next request's cost for the cached prefix, warm and cold, on the session and each thread.
func TestCacheStateCarriesNextRequestCosts(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	now := time.Now().UnixMilli()
	main := mkEvent(now-100_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	main.Status, main.FreshInput, main.CacheRead, main.CacheWrite = 200, 0, 5000, 0
	sub := mkEvent(now-40_000, "s1", "aws/claude-haiku-5", 1000, 800)
	sub.Status, sub.ThreadID, sub.FreshInput, sub.CacheRead, sub.CacheWrite, sub.CacheWrite1h = 200, "a:X", 0, 300, 700, 700
	seed(t, rec, main, sub)

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	// The session entry is the latest touch: the 1000-token subagent write on the 1h tier.
	if !near(body["next_warm_usd"].(float64), 1000*ibmSonnet.CacheRead) {
		t.Errorf("next_warm_usd = %v, want prefix x cache-read price", body["next_warm_usd"])
	}
	if body["next_cold_usd"] != body["rewrite_usd"] {
		t.Errorf("next_cold_usd %v must equal rewrite_usd %v", body["next_cold_usd"], body["rewrite_usd"])
	}
	for _, th := range body["threads"].([]any) {
		th := th.(map[string]any)
		prefix := 5000.0
		if th["id"] == "a:X" {
			prefix = 1000
		}
		if !near(th["next_warm_usd"].(float64), prefix*ibmSonnet.CacheRead) || th["next_cold_usd"] != th["rewrite_usd"] {
			t.Errorf("thread %v: warm %v cold %v rewrite %v", th["id"], th["next_warm_usd"], th["next_cold_usd"], th["rewrite_usd"])
		}
	}
}

func TestCacheStateOmitsNextCostsWhenUnpriced(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	a.SetPricer(staticPricer{ibmSonnet})
	e := mkEvent(time.Now().UnixMilli()-1000, "s1", "some/unmeasured-model", 1000, 800)
	e.Status = 200
	seed(t, rec, e)
	_, body := get(t, a, "/api/cachestate?session=s1", "")
	if _, ok := body["next_warm_usd"]; ok {
		t.Errorf("an unpriced model must not show next_warm_usd: %v", body)
	}
	if _, ok := body["next_cold_usd"]; ok {
		t.Errorf("an unpriced model must not show next_cold_usd: %v", body)
	}
	for _, th := range body["threads"].([]any) {
		if _, ok := th.(map[string]any)["next_warm_usd"]; ok {
			t.Errorf("unpriced thread shows next_warm_usd: %v", th)
		}
	}
}

// A database from before this feature gains the columns on open and keeps its rows.
func TestManualPingColumnsAreAddedToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1, "s", "m", 10, 10)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"manual_ping", "manual_saved_usd"} {
		if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the columns: %v", err)
	}
	defer db2.Close()
	var n int
	var saved float64
	if err := db2.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(manual_saved_usd),0) FROM requests WHERE manual_ping = 0`).Scan(&n, &saved); err != nil || n != 1 {
		t.Fatalf("rows after migration = %d, err %v", n, err)
	}
}

// pingable and ping_reason are stated per thread when the host can say, and left out when it cannot.
func TestCacheStateStatesWhichThreadsArePingableAndWhyNot(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	main := mkEvent(now-100_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	main.Status, main.CacheRead = 200, 5000
	sub := mkEvent(now-40_000, "s1", "aws/claude-haiku-5", 1000, 800)
	sub.Status, sub.ThreadID, sub.CacheRead = 200, "a:X", 300
	seed(t, rec, main, sub)

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	if _, ok := body["pingable"]; ok {
		t.Errorf("no keeper wired, yet pingable is stated: %v", body["pingable"])
	}
	if _, ok := body["ping_reason"]; ok {
		t.Errorf("no keeper wired, yet ping_reason is stated: %v", body["ping_reason"])
	}
	a.SetPingStates(func(_ string, _ bool, session string) map[string]PingState {
		return map[string]PingState{"": {Pingable: true}, "a:X": {Reason: "too large"}}
	})
	_, body = get(t, a, "/api/cachestate?session=s1", "")
	if body["pingable"] != true || body["ping_reason"] != nil {
		t.Errorf("session = pingable %v reason %v, want true and no reason", body["pingable"], body["ping_reason"])
	}
	for _, th := range body["threads"].([]any) {
		th := th.(map[string]any)
		if th["main"] == true && (th["pingable"] != true || th["ping_reason"] != nil) {
			t.Errorf("main thread = %v", th)
		}
		if th["id"] == "a:X" && (th["pingable"] != false || th["ping_reason"] != "too large") {
			t.Errorf("subagent = pingable %v reason %v, want false / too large", th["pingable"], th["ping_reason"])
		}
	}
	// A thread the host never heard of was never held.
	a.SetPingStates(func(string, bool, string) map[string]PingState { return map[string]PingState{} })
	_, body = get(t, a, "/api/cachestate?session=s1", "")
	if body["pingable"] != false || body["ping_reason"] != "no entry" {
		t.Errorf("unknown thread = pingable %v reason %v, want false / no entry", body["pingable"], body["ping_reason"])
	}
}

// A manual-only hold is not a scheduled ping: the status line draws "being pinged" from held_until_ms
// and pings_left, so /api/cachestate must leave both out when the host's plan source has nothing for the thread.
func TestCacheStateLeavesHeldUntilAndPingsLeftOutForAThreadWithNoPlan(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	main := mkEvent(now-100_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	main.Status, main.CacheRead = 200, 5000
	seed(t, rec, main)
	a.SetKeepAlivePlans(func(string, bool, string) map[string]KeepAlivePlan { return map[string]KeepAlivePlan{} }) // no plan: Plans skips it
	a.SetPingStates(func(string, bool, string) map[string]PingState { return map[string]PingState{"": {Pingable: true}} })
	_, body := get(t, a, "/api/cachestate?session=s1", "")
	if body["pingable"] != true {
		t.Fatalf("pingable = %v", body["pingable"])
	}
	for _, k := range []string{"held_until_ms", "pings_left"} {
		if _, ok := body[k]; ok {
			t.Errorf("session: %s = %v for a thread held only for the button", k, body[k])
		}
	}
	for _, th := range body["threads"].([]any) {
		for _, k := range []string{"held_until_ms", "pings_left"} {
			if v, ok := th.(map[string]any)[k]; ok {
				t.Errorf("thread: %s = %v", k, v)
			}
		}
	}
}
