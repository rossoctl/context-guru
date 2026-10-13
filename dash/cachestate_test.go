package dash

import (
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
