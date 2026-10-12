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
