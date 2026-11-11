package dash

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A body older than the 30 s freshness window but inside the stale cap is served at once, labelled
// stale, with its real age, and exactly one refresh runs behind it. Before the cap was widened a
// reader who came back after a minute found nothing to serve and paid the whole query.
func TestServeJSONServesAStaleBodyWithItsAge(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	c := &jsonCache{entries: map[string]jsonCacheEntry{
		"k": {at: time.Now().Add(-5 * time.Minute), body: []byte(`{"gen":0}`)},
	}}
	var computes int32
	refreshed := make(chan struct{})
	compute := func(*DB) ([]byte, error) {
		atomic.AddInt32(&computes, 1)
		close(refreshed)
		return []byte(`{"gen":1}`), nil
	}
	w := httptest.NewRecorder()
	a.serveJSON(w, httptest.NewRequest(http.MethodGet, "/api/stats", nil), c, "k", compute)

	if got := w.Header().Get("X-Cache"); got != "stale" {
		t.Fatalf("X-Cache = %q, want stale: a 5-minute-old body is inside the cap and must be served, not recomputed", got)
	}
	if w.Body.String() != `{"gen":0}` {
		t.Errorf("body = %q, want the cached one", w.Body.String())
	}
	age, err := strconv.Atoi(w.Header().Get("Age"))
	if err != nil || age < 299 || age > 305 {
		t.Errorf("Age = %q, want about 300 seconds", w.Header().Get("Age"))
	}
	select {
	case <-refreshed:
	case <-time.After(3 * time.Second):
		t.Fatal("no refresh was started behind the stale answer")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if body, fresh := c.load("k"); fresh && string(body) == `{"gen":1}` {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refreshed body never replaced the stale one")
		}
		time.Sleep(time.Millisecond)
	}
	if n := atomic.LoadInt32(&computes); n != 1 {
		t.Errorf("compute ran %d times, want 1", n)
	}
}

// Past the cap nothing is served: the reader waits for current figures, and the answer says Age 0.
func TestServeJSONComputesWhenPastTheCap(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	c := &jsonCache{entries: map[string]jsonCacheEntry{
		"k": {at: time.Now().Add(-(dashCacheTTL + dashCacheStale + time.Minute)), body: []byte(`{"gen":0}`)},
	}}
	w := httptest.NewRecorder()
	a.serveJSON(w, httptest.NewRequest(http.MethodGet, "/api/stats", nil), c, "k",
		func(*DB) ([]byte, error) { return []byte(`{"gen":2}`), nil })
	if w.Body.String() != `{"gen":2}` || w.Header().Get("X-Cache") != "miss" || w.Header().Get("Age") != "0" {
		t.Errorf("got %q X-Cache=%q Age=%q, want a fresh compute labelled miss with Age 0",
			w.Body.String(), w.Header().Get("X-Cache"), w.Header().Get("Age"))
	}
}

// The page must show the age of the data. These are source checks like the neighbouring UI tests:
// the Age header is read into state.ageMs and Overview subtracts it from "Updated N ago".
func TestOverviewFreshnessUsesTheAgeOfTheData(t *testing.T) {
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"res.headers.get('Age')",
		"state.loadedAt = Date.now() - dataAge",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js no longer contains %q; \"Updated N ago\" would claim fresher data than it shows", want)
		}
	}
}

// InvalidateStateCaches drops the cached tool reads at once, so a write shows on the next load; a read
// that began before the write and finishes after it must not put its old body back.
func TestInvalidateStateCachesDropsReadsAndIgnoresALateOldCompute(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	a.toolFilterCache.set("k", []byte(`{"v":"before the click"}`))
	a.routeCache.set("r", []byte(`{"v":"route"}`))

	gen := a.toolFilterCache.generation() // a compute begins now, reading the old state
	a.InvalidateStateCaches("account")
	a.toolFilterCache.setIfCurrent("k", []byte(`{"v":"computed from the old state"}`), gen)

	if body, _ := a.toolFilterCache.load("k"); body != nil {
		t.Errorf("after the write the cache still holds %q", body)
	}
	if body, _ := a.routeCache.load("r"); body == nil {
		t.Error("an account write dropped the route cache, which holds no account state")
	}
	a.InvalidateStateCaches("strategy")
	if body, _ := a.routeCache.load("r"); body != nil {
		t.Errorf("a strategy write left the route cache holding %q", body)
	}
}

// The live routes go through serveJSON, so this is the guard on the real path: a clear that lands while
// a compute is running must not be undone by that compute finishing, in the foreground flight and in the
// background refresh alike. The next read after the write recomputes from the new state.
func TestAClearDuringASlowComputeIsNotUndoneByIt(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	c := &a.toolFilterCache

	// foreground flight: a cold read starts, the write lands, the read finishes.
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.serveJSON(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/toolfilter", nil), c, "k",
			func(*DB) ([]byte, error) { close(started); <-release; return []byte(`{"v":"old state"}`), nil })
	}()
	<-started
	a.InvalidateStateCaches("account")
	close(release)
	<-done
	if body, _ := c.load("k"); body != nil {
		t.Fatalf("a compute that began before the write stored %q after it", body)
	}

	// background refresh: a stale entry, the refresh starts, the write lands, the refresh finishes.
	c.set("k2", []byte(`{"v":"before"}`))
	c.mu.Lock()
	e := c.entries["k2"]
	e.at = time.Now().Add(-2 * time.Minute)
	c.entries["k2"] = e
	c.mu.Unlock()
	release2 := make(chan struct{})
	started2 := make(chan struct{})
	w := httptest.NewRecorder()
	a.serveJSON(w, httptest.NewRequest(http.MethodGet, "/api/toolfilter", nil), c, "k2",
		func(*DB) ([]byte, error) { close(started2); <-release2; return []byte(`{"v":"old state"}`), nil })
	<-started2
	a.InvalidateStateCaches("account")
	close(release2)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		refreshing := c.refreshing["k2"]
		c.mu.Unlock()
		if !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh never finished")
		}
		time.Sleep(time.Millisecond)
	}
	if body, _ := c.load("k2"); body != nil {
		t.Fatalf("a refresh that began before the write stored %q after it", body)
	}
}
