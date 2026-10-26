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
// `▲ update` marker, the session's manual pings and, on a subscription, the tokens saved in the 5-hour window. Each
// comes from a statusline.py helper; status_extras swallows every error, so a rename there would blank a field in
// silence. This test is what catches it.
func TestCGCachestateCarriesTheStatusLinesExtras(t *testing.T) {
	const now = 1_800_000_000_000
	const start = now - 3_600_000
	cache := fmt.Sprintf(`{"session":"session-A","model":"m","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,"threads":[{"id":"","main":true,"last_ts":%d}]}`, now-60_000, now, now-60_000)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var windowAsked string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(cache)) }) //nolint:errcheck
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("session") != "" {
			w.Write([]byte(`{"total_saved_usd":0.31,"manual_pings":{"count":2,"ping_usd":0.08,"saved_usd":0.27}}`)) //nolint:errcheck
			return
		}
		windowAsked = r.URL.Query().Get("since")
		w.Write([]byte(`{"saved_unique":85000,"replay_tokens":1200000}`)) //nolint:errcheck
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
	if doc["update"] != "▲ update v0.6.9" {
		t.Errorf("update = %v, want the status line's `▲ update v0.6.9`", doc["update"])
	}
	if mp, _ := doc["manual_pings"].(map[string]any); mp["saved_usd"] != 0.27 {
		t.Errorf("manual_pings = %v, want the session stats' figure", doc["manual_pings"])
	}
	if doc["window_saved"] != "saved 1.2M rd / 85.0k wr" {
		t.Errorf("window_saved = %v (window asked since=%q, want %d)", doc["window_saved"], windowAsked, start)
	}
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
