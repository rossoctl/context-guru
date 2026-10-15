package dash

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCacheStateServesTheLastCacheTouch(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	w1 := mkEvent(now-60_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	w1.Status, w1.FreshInput, w1.CacheWrite, w1.CacheWrite1h = 200, 10, 5000, 5000
	rd := mkEvent(now-30_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	rd.Status, rd.FreshInput, rd.CacheRead, rd.CacheWrite = 200, 20, 5000, 0
	other := mkEvent(now-1_000, "s2", "aws/claude-sonnet-5", 1000, 800)
	other.Status, other.CacheRead = 200, 99
	seed(t, rec, w1, rd, other)

	w, body := get(t, a, "/api/cachestate?session=s1", "")
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if body["last_ts"].(float64) != float64(now-30_000) {
		t.Errorf("last_ts = %v, want the read row", body["last_ts"])
	}
	if body["ttl_s"].(float64) != 3600 || body["prefix_tokens"].(float64) != 5020 {
		t.Errorf("ttl_s/prefix = %v/%v, want 3600 (last WRITE was 1h) / 5020", body["ttl_s"], body["prefix_tokens"])
	}
	if w, _ := get(t, a, "/api/cachestate", ""); w.Code != 400 {
		t.Errorf("unscoped = %d, want 400", w.Code)
	}
	if _, b := get(t, a, "/api/cachestate?session=nope", ""); b["none"] != true {
		t.Errorf("unknown session = %v, want none", b)
	}
}

func TestCacheStateListsThreadsAndTheResponseEnd(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	main := mkEvent(now-100_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	main.Status, main.CacheRead, main.UpstreamMs = 200, 5000, 20_000
	sub := mkEvent(now-40_000, "s1", "aws/claude-haiku-5", 1000, 800)
	sub.Status, sub.ThreadID, sub.CacheRead, sub.CacheWrite = 200, "a:X", 300, 0
	old := mkEvent(now-3*3600*1000, "s1", "aws/claude-haiku-5", 1000, 800)
	old.Status, old.ThreadID = 200, "a:old"
	seed(t, rec, main, sub, old)

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	ths, _ := body["threads"].([]any)
	if len(ths) != 2 {
		t.Fatalf("threads = %v, want main + one subagent (the 3h-old one is finished work)", body["threads"])
	}
	first := ths[0].(map[string]any)
	if first["id"] != "a:X" || first["main"] != false {
		t.Errorf("most recent thread first: got %v", first)
	}
	if last := ths[1].(map[string]any); last["main"] != true || last["end_ts"].(float64) != float64(now-80_000) {
		t.Errorf("main thread end_ts must be start + upstream ms: %v", last)
	}
}

// TestCacheStateShowsTheKeepAlivePlan: a thread the keeper still plans to ping reports how many
// pings are left and when its cache goes cold: the last ping's start plus the TTL.
func TestCacheStateShowsTheKeepAlivePlan(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	main := mkEvent(now-100_000, "s1", "aws/claude-sonnet-5", 1000, 800)
	main.Status, main.CacheRead = 200, 5000
	sub := mkEvent(now-40_000, "s1", "aws/claude-haiku-5", 1000, 800)
	sub.Status, sub.ThreadID, sub.CacheRead = 200, "a:X", 300
	seed(t, rec, main, sub)
	var asked string
	a.SetKeepAlivePlans(func(_ string, _ bool, session string) map[string]KeepAlivePlan {
		asked = session
		return map[string]KeepAlivePlan{"": {RefreshedMS: now - 100_000, NextPingMS: now + 170_000, PingsLeft: 2, IdleMS: 270_000}}
	})

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	if asked != "s1" {
		t.Errorf("plan asked for session %q, want s1", asked)
	}
	want := float64(now + 170_000 + 270_000 + 300_000)
	if body["held_until_ms"] != want || body["pings_left"] != 2.0 {
		t.Errorf("top level held_until_ms/pings_left = %v/%v, want %v/2", body["held_until_ms"], body["pings_left"], want)
	}
	for _, th := range body["threads"].([]any) {
		th := th.(map[string]any)
		if th["main"] == true && th["held_until_ms"] != want {
			t.Errorf("main thread held_until_ms = %v, want %v", th["held_until_ms"], want)
		}
		if th["main"] == false && th["held_until_ms"] != nil {
			t.Errorf("a thread with no plan must not claim one: %v", th)
		}
	}
}

// TestHeldUntilStartsFromTheLastRealRefresh: a planned ping only extends an entry that is still
// alive when it fires. After a refused ping the next one can come too late, and then the cache is
// cold at its own expiry, whatever is still planned.
func TestHeldUntilStartsFromTheLastRealRefresh(t *testing.T) {
	const ttl = 300
	for _, c := range []struct {
		name string
		p    KeepAlivePlan
		want int64
	}{
		{"every ping in time", KeepAlivePlan{RefreshedMS: 0, NextPingMS: 270_000, PingsLeft: 2, IdleMS: 270_000}, 840_000},
		// the sweep moved the schedule for a ping that was refused at 270 s: the next one, at
		// 540 s, comes after the entry refreshed at 0 expired at 300 s
		{"after a refused ping", KeepAlivePlan{RefreshedMS: 0, NextPingMS: 540_000, PingsLeft: 1, IdleMS: 270_000}, 300_000},
		{"no pings left", KeepAlivePlan{RefreshedMS: 1_000, NextPingMS: 271_000, PingsLeft: 0, IdleMS: 270_000}, 301_000},
	} {
		if got := c.p.heldUntil(ttl); got != c.want {
			t.Errorf("%s: heldUntil = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestCacheStateSplitsTheSessionsMoney: `saved $x of $y` for the main thread and for every other
// thread together, on one basis. y is the cost without context-guru (the baseline plus the split
// and keep-alive savings); x is y minus what was paid, so context-guru's own model calls and its
// pings lower x and never raise y. A side thread (p:) and a thread with no cache touch still count.
func TestCacheStateSplitsTheSessionsMoney(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	row := func(ago int64, thread string, cost, base, cgllm, split float64) *Event {
		e := mkEvent(now-ago, "s1", "aws/claude-sonnet-5", 1000, 800)
		e.Status, e.ThreadID, e.CacheRead = 200, thread, 5000
		e.CostUSD, e.BaselineCostUSD, e.CGLLMCostUSD, e.CachesplitSavedUSD = cost, base, cgllm, split
		return e
	}
	ping := row(30_000, "", 0.05, 0, 0, 0)
	ping.KeepAlive = true
	noTouch := row(10_000, "a:fresh", 0.20, 0.30, 0, 0)
	noTouch.CacheRead, noTouch.CacheWrite = 0, 0
	seed(t, rec,
		row(100_000, "", 1.00, 9.00, 0.10, 0.50), ping, // main: y = 9.50, x = 9.50 - 1.00 - 0.10 - 0.05
		row(60_000, "a:X", 0.40, 1.00, 0, 0),
		row(50_000, "p:side", 0.10, 0.10, 0, 0), noTouch)

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	money, _ := body["money"].(map[string]any)
	if money == nil {
		t.Fatalf("no money in %v", body)
	}
	check := func(name string, want map[string]float64) {
		m := money[name].(map[string]any)
		for k, v := range want {
			if got, _ := m[k].(float64); math.Abs(got-v) > 1e-9 {
				t.Errorf("%s.%s = %v, want %v", name, k, m[k], v)
			}
		}
		if m["saved_usd"].(float64) > m["baseline_usd"].(float64) {
			t.Errorf("%s: saved %v exceeds its baseline %v", name, m["saved_usd"], m["baseline_usd"])
		}
	}
	check("main", map[string]float64{"saved_usd": 8.35, "baseline_usd": 9.50, "threads": 1})
	check("others", map[string]float64{"saved_usd": 0.70, "baseline_usd": 1.40, "threads": 3})
	// The listed cache entries are unchanged: the no-touch thread is money only.
	for _, th := range body["threads"].([]any) {
		if th.(map[string]any)["id"] == "a:fresh" {
			t.Errorf("a thread with no cache touch must not be listed as a cache entry: %v", th)
		}
	}
	// A session whose only rows touched no cache still reports its money on the `none` answer.
	seed(t, rec, func() *Event {
		e := row(1_000, "", 0.10, 0.20, 0, 0)
		e.SessionID, e.CacheRead, e.CacheWrite = "s2", 0, 0
		return e
	}())
	_, b2 := get(t, a, "/api/cachestate?session=s2", "")
	if b2["none"] != true || b2["money"] == nil {
		t.Errorf("s2 = %v, want none with money", b2)
	}
}

// TestCacheThreadsQueryIsIndexed: the per-thread money and the per-thread TTL are each one indexed
// read of the session. Neither may look the TTL up again per thread: a correlated subquery that
// walks the session once for each thread made /api/cachestate take 1.6 s at 300 threads, longer
// than the status line waits for it (#570).
func TestCacheThreadsQueryIsIndexed(t *testing.T) {
	db := openTestDB(t)
	seedLeanFixture(t, db)
	for name, build := range map[string]func(Filter) (string, []any){
		"cache threads": cacheThreadsQuery, "cache ttl": cacheTTLQuery,
	} {
		q, args := build(Filter{Session: "s-target"})
		plan := explainPlan(t, db, q, args)
		assertNoWholeTableScan(t, name, plan)
		assertSessionOrTimeIndexed(t, name, plan)
		for _, p := range plan {
			if strings.Contains(p, "SEARCH w ") {
				t.Errorf("%s: a per-thread TTL subquery is back in the plan: %q", name, plan)
			}
		}
	}
}

// TestCacheThreadsTakesEachThreadsTTLFromItsLatestWrite: each thread's TTL is the tier of its own
// latest cache write, pings included, and 300 when it never wrote.
func TestCacheThreadsTakesEachThreadsTTLFromItsLatestWrite(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	row := func(ago int64, thread string, write, write1h int64) *Event {
		e := mkEvent(now-ago, "s1", "aws/claude-sonnet-5", 1000, 1000)
		e.Status, e.ThreadID, e.CacheRead, e.CacheWrite, e.CacheWrite1h = 200, thread, 5000, write, write1h
		return e
	}
	ping := row(20_000, "a:P", 900, 900)
	ping.KeepAlive = true
	seed(t, rec,
		// main: a 1h write, then a 5m write: 300
		row(100_000, "", 900, 900), row(50_000, "", 900, 0), row(10_000, "", 0, 0),
		// a:H: one 1h write, then reads: 3600
		row(90_000, "a:H", 900, 900), row(15_000, "a:H", 0, 0),
		// a:N: never wrote: 300
		row(80_000, "a:N", 0, 0),
		// a:P: the latest write is a 1h ping: 3600
		row(70_000, "a:P", 900, 0), ping, row(12_000, "a:P", 0, 0),
	)
	_, body := get(t, a, "/api/cachestate?session=s1", "")
	want := map[string]float64{"": 300, "a:H": 3600, "a:N": 300, "a:P": 3600}
	got := map[string]float64{}
	for _, x := range body["threads"].([]any) {
		th := x.(map[string]any)
		got[th["id"].(string)] = th["ttl_s"].(float64)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("thread %q ttl_s = %v, want %v (all: %v)", id, got[id], w, got)
		}
	}
	// A time window on the request does not hide a:H's 1h write: the tier is the cache entry's.
	_, body = get(t, a, fmt.Sprintf("/api/cachestate?session=s1&since=%d", now-30_000), "")
	for _, x := range body["threads"].([]any) {
		if th := x.(map[string]any); th["id"] == "a:H" && th["ttl_s"] != 3600.0 {
			t.Errorf("with since=, a:H ttl_s = %v, want 3600 from its write before the window", th["ttl_s"])
		}
	}
	if !strings.Contains(fmt.Sprint(body["threads"]), "a:H") {
		t.Errorf("a:H (last touch 15 s ago) must be listed with since=30 s ago: %v", body["threads"])
	}
}

// TestCacheThreadsTakesItsCacheFieldsFromTheLatestTouch pins the one-MAX bare-column rule
// cacheThreadsQuery relies on. The thread has two pings, an older and a newer cache-touching
// request with different models and prefixes, and a later request that touched no cache. The
// cache fields must come from the newer touching row, and the money must sum every row: a
// second MAX (on the ping cost, say) would make SQLite's bare columns ambiguous and the ping sum
// one ping's cost.
func TestCacheThreadsTakesItsCacheFieldsFromTheLatestTouch(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	now := time.Now().UnixMilli()
	row := func(ago int64, model string, read int64) *Event {
		e := mkEvent(now-ago, "s1", model, 1000, 1000)
		e.Status, e.ThreadID, e.FreshInput, e.CacheRead, e.CacheWrite = 200, "a:T", 0, read, 0
		e.CostUSD, e.BaselineCostUSD = 0.10, 0.10
		return e
	}
	ping := func(ago int64) *Event {
		e := row(ago, "m-ping", 7000)
		e.KeepAlive, e.CostUSD, e.BaselineCostUSD = true, 0.01, 0
		return e
	}
	seed(t, rec, row(100_000, "m-old", 1000), ping(90_000), ping(80_000), row(50_000, "m-new", 3000), row(10_000, "m-none", 0))

	_, body := get(t, a, "/api/cachestate?session=s1", "")
	var th map[string]any
	for _, x := range body["threads"].([]any) {
		if x.(map[string]any)["id"] == "a:T" {
			th = x.(map[string]any)
		}
	}
	if th == nil {
		t.Fatalf("thread a:T not listed: %v", body["threads"])
	}
	if th["model"] != "m-new" || th["prefix_tokens"] != 3000.0 || th["last_ts"] != float64(now-50_000) {
		t.Errorf("cache fields = model %v prefix %v last_ts %v, want the latest touching row (m-new, 3000, %d)",
			th["model"], th["prefix_tokens"], th["last_ts"], now-50_000)
	}
	// y = 0.30 (three agent rows); x = y - 0.30 paid - 0.02 for BOTH pings.
	if got := th["saved_usd"].(float64); math.Abs(got-(-0.02)) > 1e-9 {
		t.Errorf("saved_usd = %v, want -0.02 (both pings deducted)", got)
	}
	if got := th["baseline_usd"].(float64); math.Abs(got-0.30) > 1e-9 {
		t.Errorf("baseline_usd = %v, want 0.30", got)
	}
}
