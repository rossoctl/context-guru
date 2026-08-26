package dash

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestComponentsCached guards the fix: /api/components had no cache at all (unlike
// /api/stats and /api/facets), so every load re-ran all five of its queries cold —
// measured ~4.4s on a corpus matching production scale. A second call within
// dashCacheTTL must be served from cache, not re-run against the store: seed one
// component row, read it, seed a second, and confirm the immediate re-read still
// reports only the first (the cached body), while a read past the TTL sees both.
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

	// Past the TTL, the same call must see the fresh data. Single-tenant scope() always
	// returns Principal{Manager: true} (see API.scope), so cacheKey's own rule gives a
	// deterministic key to backdate.
	key := cacheKey(Principal{Manager: true}, httptest.NewRequest(http.MethodGet, "/api/components", nil))
	a.componentsCache.mu.Lock()
	entry := a.componentsCache.entries[key]
	entry.at = time.Now().Add(-dashCacheTTL - time.Second)
	a.componentsCache.entries[key] = entry
	a.componentsCache.mu.Unlock()

	_, body = get(t, a, "/api/components", "127.0.0.1:1")
	rows, _ = body["components"].([]any)
	if len(rows) != 2 {
		t.Fatalf("call past the TTL: got %d component rows, want 2 (fresh)", len(rows))
	}
}
