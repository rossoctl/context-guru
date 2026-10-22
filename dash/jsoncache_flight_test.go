package dash

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Two different caches asked for the same entry key at the same time must each run their own compute.
// /api/stats and /api/tools have the same key for the same account and filters; they shared one
// single-flight group keyed by the entry key alone, so a concurrent pair collapsed onto one computation
// and /api/stats answered with the tools report.
func TestConcurrentColdReadsOfDifferentCachesDoNotShareAComputation(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	var stats, tools jsonCache
	release := make(chan struct{})
	started := make(chan string, 2)
	mk := func(name string) func(*DB) ([]byte, error) {
		return func(*DB) ([]byte, error) {
			started <- name
			<-release
			return []byte(`{"from":"` + name + `"}`), nil
		}
	}
	got := make(chan [2]string, 2)
	for _, c := range []struct {
		name  string
		cache *jsonCache
	}{{"stats", &stats}, {"tools", &tools}} {
		go func(name string, cache *jsonCache) {
			w := httptest.NewRecorder()
			a.serveJSON(w, httptest.NewRequest(http.MethodGet, "/api/x", nil), cache, "same-key", mk(name))
			got <- [2]string{name, w.Body.String()}
		}(c.name, c.cache)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("the second cache's compute never started: it joined the first one's flight")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		r := <-got
		if r[1] != `{"from":"`+r[0]+`"}` {
			t.Errorf("%s answered %s", r[0], r[1])
		}
	}
}

// One cache, two different entry keys (two accounts, say), both cold and held open together, must each
// run their own compute. A flight key made of the cache pointer alone would join them.
func TestConcurrentColdReadsOfDifferentKeysInOneCacheDoNotShareAComputation(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	var cache jsonCache
	release := make(chan struct{})
	started := make(chan string, 2)
	mk := func(name string) func(*DB) ([]byte, error) {
		return func(*DB) ([]byte, error) {
			started <- name
			<-release
			return []byte(`{"from":"` + name + `"}`), nil
		}
	}
	got := make(chan [2]string, 2)
	for _, name := range []string{"key-a", "key-b"} {
		go func(name string) {
			w := httptest.NewRecorder()
			a.serveJSON(w, httptest.NewRequest(http.MethodGet, "/api/x", nil), &cache, name, mk(name))
			got <- [2]string{name, w.Body.String()}
		}(name)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("the second key's compute never started: it joined the first one's flight")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		r := <-got
		if r[1] != `{"from":"`+r[0]+`"}` {
			t.Errorf("%s answered %s", r[0], r[1])
		}
	}
}

// The same collision, through the real routes: /api/stats and /api/tools, same account, same (empty)
// filters, asked together. Each body must be its own report.
func TestStatsAndToolsRequestedTogetherEachGetTheirOwnReport(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "sess-1", "aws/claude-sonnet-5", 1000, 800))
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	type res struct {
		path, body string
	}
	out := make(chan res, 2)
	for _, p := range []string{"/api/stats", "/api/tools"} {
		go func(p string) {
			r, err := http.Get(srv.URL + p)
			if err != nil {
				out <- res{p, err.Error()}
				return
			}
			defer r.Body.Close()
			b, err := io.ReadAll(r.Body)
			if err != nil {
				out <- res{p, err.Error()}
				return
			}
			out <- res{p, string(b)}
		}(p)
	}
	for i := 0; i < 2; i++ {
		r := <-out
		isStats := strings.Contains(r.body, `"tokens_before"`)
		isTools := strings.Contains(r.body, `"servers"`) || strings.Contains(r.body, `"coverage"`)
		if r.path == "/api/stats" && !isStats {
			t.Errorf("/api/stats answered with the wrong report: %.120s", r.body)
		}
		if r.path == "/api/tools" && (isStats || !isTools) {
			t.Errorf("/api/tools did not answer with the tools report: %.120s", r.body)
		}
	}
}
