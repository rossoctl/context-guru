package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What `cg.py cachestate` adds beside the raw cachestate since the mod's band draws the status line (0.6.1): the
// `▲ update` marker, the session's manual pings and, on a subscription, the tokens this session saved in the 5-hour
// window. Each comes from a statusline.py helper; status_extras swallows every error, so a rename there would blank
// a field in silence. This test is what catches it.
func TestCGCachestateCarriesTheStatusLinesExtras(t *testing.T) {
	// The window's saving comes from `tokens` (#999), every feature, by tier; the old compaction-only fields lose.
	window := `{"saved_unique":85000,"replay_tokens":1200000,` +
		`"tokens":{"saved":{"input":12000,"cache_read":0,"cache_write_5m":85000,"cache_write_1h":0},` +
		`"caused":{"input":0,"cache_read":40000,"cache_write_5m":0,"cache_write_1h":0}},"tokens_unmeasured_requests":3}`
	doc := cachestateWithExtras(t, window)
	if doc["update"] != "▲ update v0.6.9" {
		t.Errorf("update = %v, want the status line's `▲ update v0.6.9`", doc["update"])
	}
	if mp, _ := doc["manual_pings"].(map[string]any); mp["saved_usd"] != 0.27 {
		t.Errorf("manual_pings = %v, want the session stats' figure", doc["manual_pings"])
	}
	// The tokens are this session's, not every session's in the window. The unmeasured count is not on the band.
	if doc["window_saved"] != "saved 85.0k wr · 12.0k in · cost 40.0k rd" {
		t.Errorf("window_saved = %v, want this session's tokens by tier (window asked since=%v, want %d)", doc["window_saved"], doc["window_asked"], cachestateWindowStart)
	}
}

// The band's token figure by case: a tier at 0 is left out, `cost` only when a tier was paid, and a proxy before #999
// (no `tokens`) keeps its compaction-only `rd / wr` figure.
func TestCGCachestateWindowTokensByCase(t *testing.T) {
	zero := `{"input":0,"cache_read":0,"cache_write_5m":0,"cache_write_1h":0}`
	for _, c := range []struct{ name, window, want string }{
		{"every tier, in order", `{"tokens":{"saved":{"input":5,"cache_read":1200000,"cache_write_5m":85000,"cache_write_1h":2000},"caused":{"input":0,"cache_read":0,"cache_write_5m":600,"cache_write_1h":0}}}`,
			"saved 85.0k wr · 2.0k wr1h · 1.2M rd · 5 in · cost 600 wr"},
		{"saved only", `{"tokens":{"saved":{"input":0,"cache_read":0,"cache_write_5m":85000,"cache_write_1h":0},"caused":` + zero + `}}`, "saved 85.0k wr"},
		{"cost only", `{"tokens":{"saved":` + zero + `,"caused":{"input":0,"cache_read":40000,"cache_write_5m":0,"cache_write_1h":0}}}`, "cost 40.0k rd"},
		{"nothing", `{"saved_unique":85000,"replay_tokens":1200000,"tokens":{"saved":` + zero + `,"caused":` + zero + `}}`, ""},
		{"proxy before #999", `{"saved_unique":85000,"replay_tokens":1200000}`, "saved 1.2M rd / 85.0k wr"},
	} {
		doc := cachestateWithExtras(t, c.window)
		got, _ := doc["window_saved"].(string)
		if got != c.want {
			t.Errorf("%s: window_saved = %q, want %q", c.name, got, c.want)
		}
	}
}

const cachestateWindowStart = 1_800_000_000_000 - 3_600_000

// cachestateWithExtras runs `cg.py cachestate --json` against a stub proxy whose window read for this session answers
// window, and returns the doc, with the `since` the window read asked for under "window_asked".
func cachestateWithExtras(t *testing.T, window string) map[string]any {
	t.Helper()
	const now = 1_800_000_000_000
	const start = cachestateWindowStart
	cache := fmt.Sprintf(`{"session":"session-A","model":"m","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,"threads":[{"id":"","main":true,"last_ts":%d}]}`, now-60_000, now, now-60_000)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var windowAsked string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(cache)) }) //nolint:errcheck
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("session") != "" && q.Get("since") != "": // this session in the window: the band's saving
			windowAsked = q.Get("since")
			w.Write([]byte(window)) //nolint:errcheck
		case q.Get("session") != "":
			w.Write([]byte(`{"total_saved_usd":0.31,"manual_pings":{"count":2,"ping_usd":0.08,"saved_usd":0.27}}`)) //nolint:errcheck
		default: // every session in the window: the band must not show these
			w.Write([]byte(`{"saved_unique":999000,"replay_tokens":9990000,"tokens":{"saved":{"input":0,"cache_read":0,"cache_write_5m":999000,"cache_write_1h":0}}}`)) //nolint:errcheck
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	base := "http://" + ln.Addr().String()

	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "update-check.yaml"), []byte("answer: ask\nlatest: v0.6.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"XDG_CACHE_HOME=" + t.TempDir(), "CONTEXT_GURU_STATE=" + state}
	out := runCG(t, base, env, "cachestate", "--json", "--window-start-ms", fmt.Sprint(start))
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	doc["window_asked"] = windowAsked
	return doc
}

// The band says `cg ✕ proxy offline` only when nothing answers, as the status line does. A proxy that answers 404
// (no --dashboard) or 500 is not offline.
func TestCGCachestateSaysOfflineOnlyWhenNothingAnswers(t *testing.T) {
	for _, code := range []int{404, 500} {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }), ReadHeaderTimeout: 5 * time.Second}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln) //nolint:errcheck
		out, _ := runCGCode(t, "http://"+ln.Addr().String(), []string{"XDG_CACHE_HOME=" + t.TempDir()}, "cachestate", "--json")
		srv.Close()
		if strings.Contains(out, "offline") {
			t.Errorf("a proxy answering %d was called offline: %s", code, out)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // nothing listens there now
	out, _ := runCGCode(t, dead, []string{"XDG_CACHE_HOME=" + t.TempDir()}, "cachestate", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil || doc["notice"] != "cg ✕ proxy offline" {
		t.Errorf("a dead port did not give the offline notice: %s", out)
	}
}

// Too slow, and nothing was ever read: the band says the status line's `cg: stats loading…`, not nothing (a GLM
// round-2 point on #735). A slow proxy is not offline.
func TestCGCachestateSaysLoadingWhenTheProxyIsTooSlow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release }), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { close(release); srv.Close() })
	out, _ := runCGCode(t, "http://"+ln.Addr().String(), []string{"XDG_CACHE_HOME=" + t.TempDir()}, "cachestate", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil || doc["notice"] != "cg: stats loading…" {
		t.Errorf("a proxy slower than the 5 s read gave %s, want the `cg: stats loading…` notice", out)
	}
}
