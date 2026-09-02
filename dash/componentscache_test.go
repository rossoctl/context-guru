package dash

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestComponentsCached guards the fix: /api/components had no cache at all (unlike /api/stats and
// /api/facets), so every load re-ran all five of its queries cold — measured ~4.4s on a corpus
// matching production scale, ~6s on the live one.
//
// It now guards the STALE-WHILE-REVALIDATE contract too, which is a deliberate change from "past
// the TTL, block and return fresh". A TTL of 5s in front of a 6s query cached nothing — every
// reader missed and paid full price, and nineteen of them at once turned a query that fits inside
// the handler timeout into 503s all day. So past the TTL a reader is now handed the previous body
// immediately (X-Cache: stale) while the replacement is computed behind the response, and only
// past dashCacheStale on top of that does anyone wait again. The staleness cap is the honest half
// and is asserted here: without it a quiet deployment would serve its last body forever while the
// page reported it as current.
func TestComponentsCached(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	e := mkEvent(time.Now().UnixMilli(), "sess-1", "aws/claude-sonnet-5", 1000, 800)
	e.Components = []CompRow{{Component: "toon", Kind: "reformat", Acted: true}}
	seed(t, rec, e)

	w, body := get(t, a, "/api/components", "127.0.0.1:1")
	if w.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", w.Code, w.Body.String())
	}
	rows, _ := body["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("first call: got %d component rows, want 1", len(rows))
	}

	// Seed a second, distinct component. A cache HIT must not see it yet.
	e2 := mkEvent(time.Now().UnixMilli(), "sess-2", "aws/claude-sonnet-5", 1000, 800)
	e2.Components = []CompRow{{Component: "cachesplit", Kind: "reformat", Acted: true}}
	seed(t, rec, e2)

	_, body = get(t, a, "/api/components", "127.0.0.1:1")
	rows, _ = body["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("second call within the TTL: got %d component rows, want 1 (cached) -- the cache did not hit", len(rows))
	}

	// Single-tenant scope() always returns Principal{Manager: true} (see API.scope), so cacheKey's
	// own rule gives a deterministic key to backdate.
	key := cacheKey(Principal{Manager: true}, httptest.NewRequest(http.MethodGet, "/api/components", nil))
	backdate := func(d time.Duration) {
		a.componentsCache.mu.Lock()
		entry := a.componentsCache.entries[key]
		entry.at = time.Now().Add(-d)
		a.componentsCache.entries[key] = entry
		a.componentsCache.mu.Unlock()
	}

	// Just past the TTL: the reader is handed the STALE body rather than made to wait, and the
	// refresh happens behind the response.
	backdate(dashCacheTTL + time.Second)
	w, body = get(t, a, "/api/components", "127.0.0.1:1")
	if got := w.Header().Get("X-Cache"); got != "stale" {
		t.Errorf("just past the TTL: X-Cache = %q, want \"stale\" — the reader should not wait for a "+
			"recompute when a servable body exists", got)
	}
	rows, _ = body["components"].([]any)
	if len(rows) != 1 {
		t.Errorf("just past the TTL: got %d component rows, want 1 (the stale body served immediately)", len(rows))
	}

	// ...and that background refresh must actually land, or "stale" would be permanent.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body = get(t, a, "/api/components", "127.0.0.1:1")
		rows, _ = body["components"].([]any)
		if len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background refresh never replaced the stale body: still %d rows after 5s. "+
				"A refresh that cannot complete makes every reader permanently stale, which is worse "+
				"than the blocking recompute this replaced.", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Past the staleness cap, a reader waits for fresh numbers instead. Seed a third component so
	// "fresh" is distinguishable from whatever is cached.
	e3 := mkEvent(time.Now().UnixMilli(), "sess-3", "aws/claude-sonnet-5", 1000, 800)
	e3.Components = []CompRow{{Component: "dedup", Kind: "reformat", Acted: true}}
	seed(t, rec, e3)
	backdate(dashCacheTTL + dashCacheStale + time.Second)
	w, body = get(t, a, "/api/components", "127.0.0.1:1")
	if got := w.Header().Get("X-Cache"); got != "miss" {
		t.Errorf("past dashCacheStale: X-Cache = %q, want \"miss\" — beyond the cap a body is too old "+
			"to serve, and the freshness line the page prints would be a lie", got)
	}
	rows, _ = body["components"].([]any)
	if len(rows) != 3 {
		t.Errorf("past dashCacheStale: got %d component rows, want 3 (recomputed)", len(rows))
	}
}
