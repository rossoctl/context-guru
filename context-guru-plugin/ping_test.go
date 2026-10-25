// The ping command and the mod that presses it: `cg.py ping` POSTs /api/me/cache/ping for one session and
// says what happened; with --json it prints the HTTP status beside the server's document, which the
// Claude Code mod (hooks/register.tsx) parses. Also guards that the mod ships with the plugin.
package plugin

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pingServer(t *testing.T, status int, body string, got *map[string]string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/me/cache/ping", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		*got = b
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func TestPing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		code   int
	}{
		{"sent", 200, `{"sent":true,"ping_usd":0.04,"rewrite_usd":0.5,"saved_usd":0.46}`, "Kept warm: saved $0.46 net (ping cost $0.04)", 0},
		{"cold", 409, `{"error":"cold"}`, "already cold", 1},
		{"unknown", 404, `{"error":"unknown session"}`, "does not know that session", 1},
		{"no route", 404, `404 page not found`, "does not support manual pings yet", 1}, // Go's mux for a missing route: no JSON
		{"method not allowed", 405, ``, "does not support manual pings yet", 1},
		{"keep-alive off", 409, `{"error":"not held","detail":"x"}`, "Keep-alive is off", 1},
		{"over budget", 409, `{"error":"over budget"}`, "per-ping limit, so nothing was sent", 1},
		{"too costly", 409, `{"error":"too costly"}`, "per-ping limit, so nothing was sent", 1},
		{"unknown 409", 409, `{"error":"something new"}`, "Ping refused: something new", 1},
		{"bare 409", 409, ``, "Ping refused: no detail", 1},
		{"busy", 409, `{"error":"busy"}`, "refreshes the cache itself", 1},
		{"no capacity", 429, `{"error":"no spare capacity","retry_after_s":5}`, "No spare capacity", 1},
		{"upstream", 502, `{"error":"upstream","status":529}`, "upstream model refused", 1},
		{"too soon", 429, `{"error":"too soon","retry_after_s":17}`, "again in 17s", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]string
			base := pingServer(t, c.status, c.body, &got)
			out, code := runCGCode(t, base, nil, "ping", "--session", "S1")
			if code != c.code || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want %d containing %q:\n%s", code, c.code, c.want, out)
			}
			if got["session"] != "S1" {
				t.Errorf("posted %v, want session S1", got)
			}
		})
	}
}

func TestPingJSONCarriesTheStatus(t *testing.T) {
	var got map[string]string
	base := pingServer(t, 200, `{"sent":true,"ping_usd":0.04,"saved_usd":0.46}`, &got)
	out, _ := runCGCode(t, base, nil, "ping", "--json", "--thread", "agent1")
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if doc["status"] != float64(200) || doc["saved_usd"] != 0.46 {
		t.Errorf("doc = %v", doc)
	}
	if got["session"] != "session-A" || got["thread"] != "agent1" {
		t.Errorf("posted %v: want the env session and the thread", got)
	}
}

// The mod is declared in hooks.json beside the command hooks, and everything it loads is in the plugin.
func TestModShips(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Modules []string        `json:"modules"`
		Hooks   json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Modules) != 1 || len(h.Hooks) == 0 {
		t.Fatalf("hooks.json must carry both command hooks and one module: %s", b)
	}
	for _, f := range []string{filepath.Join("hooks", h.Modules[0]), "hooks/model.ts", "types/index.d.ts"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s does not ship: %v", f, err)
		}
	}
}

// take_slot elects one fetcher per window and removes the lock files of EVERY earlier window, not only the
// previous one: after an idle gap the older files used to stay forever, one more per gap per session.
func TestTakeSlotCleansEveryEarlierLock(t *testing.T) {
	requireTool(t, "python3")
	dir := t.TempDir()
	path := filepath.Join(dir, "cs.json")
	script := `
import os, sys, time
sys.path.insert(0, "scripts")
import cgcache
path, window = sys.argv[1], 10.0
slot = int(time.time() // window)
for old in (slot - 40, slot - 7, slot - 1, slot + 5):  # three stale windows, and one from the future
    open(f"{path}.lock.{old}", "w").close()
open(path + ".lock", "w").close()  # the legacy name
assert cgcache.take_slot(path, window) is True
assert cgcache.take_slot(path, window) is False  # same window: someone else already holds it
print(" ".join(sorted(n.split("lock", 1)[1] for n in os.listdir(os.path.dirname(path)) if "lock" in n)))
`
	out, err := exec.Command("python3", "-c", script, path).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	if len(got) != 2 { // this window's file, and the future one (never touched)
		t.Errorf("lock files left = %v, want only the current and the future window's", got)
	}
}

// `cg.py cachestate --json --transcript <path>` names the sub-agents with the status line's own logic (one copy of
// it, in statusline.py), so the mod shows the names Claude Code shows. A thread without a meta file gets no name.
func TestCachestateNamesSubAgentsFromTheTranscript(t *testing.T) {
	requireTool(t, "python3")
	now := time.Now().UnixMilli()
	th := func(id string) map[string]any {
		return map[string]any{"id": id, "main": false, "last_ts": now - 1000, "end_ts": now - 1000, "ttl_s": 300, "prefix_tokens": 1000}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"session": "S1", "last_ts": now - 1000, "end_ts": now - 1000, "now_ms": now, "ttl_s": 300,
			"threads": []any{
				map[string]any{"id": "main", "main": true, "last_ts": now - 1000, "end_ts": now - 1000, "ttl_s": 300},
				th("a:abc123"), th("a:nometa"), th("p:deadbeef"),
			},
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	dir := t.TempDir()
	transcript := filepath.Join(dir, "S1.jsonl")
	sub := filepath.Join(dir, "S1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-abc123.meta.json"), []byte(`{"agentType":"Explore"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runCGCode(t, "http://"+ln.Addr().String(), []string{"XDG_CACHE_HOME=" + t.TempDir()},
		"cachestate", "--json", "--session", "S1", "--transcript", transcript)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var doc struct {
		Threads []struct{ ID, Name string } `json:"threads"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	names := map[string]string{}
	for _, th := range doc.Threads {
		names[th.ID] = th.Name
	}
	if names["a:abc123"] != "Explore" || names["a:nometa"] != "" || names["p:deadbeef"] != "" || names["main"] != "" {
		t.Errorf("names = %v, want only a:abc123 -> Explore", names)
	}
}

// A 409 "not held" says WHY when the cachestate does: cg.py ping reads ping_reason from the shared cache.
func TestPingExplainsAnUnheldCache(t *testing.T) {
	requireTool(t, "python3")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/me/cache/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"not held"}`)) //nolint:errcheck
	})
	now := time.Now().UnixMilli()
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"session": "S1", "last_ts": now, "end_ts": now, "now_ms": now, "ttl_s": 300, "pingable": false,
			"ping_reason": "not opted in",
			"threads":     []any{map[string]any{"id": "main", "main": true, "pingable": false, "ping_reason": "not opted in"}},
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	env := []string{"XDG_CACHE_HOME=" + t.TempDir()}
	out, code := runCGCode(t, "http://"+ln.Addr().String(), env, "ping", "--session", "S1")
	if code != 1 || !strings.Contains(out, "Can't ping: Ping isn't enabled for this install yet") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	js, _ := runCGCode(t, "http://"+ln.Addr().String(), env, "ping", "--session", "S1", "--json")
	if !strings.Contains(js, `"ping_reason": "not opted in"`) {
		t.Errorf("json = %s", js)
	}
}

// Every ping_reason the server can send has words, and a code from a newer server is never shown raw.
func TestPingReasonWords(t *testing.T) {
	requireTool(t, "python3")
	for reason, want := range map[string]string{
		"below limits": "below its minimum size", "pings used": "pings have run out", "stopped": "stopped keeping this thread warm",
		"no entry": "knows nothing about this session", "unavailable": "route or credential is failing",
		"not opted in": "turns on in your next session", "expired": "cache already cold", "too large": "too large to hold",
		"unsupported": "route or settings", "over budget": "per-ping limit", "from-the-future": "cannot ping this session right now",
	} {
		t.Run(reason, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/me/cache/ping", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error":"not held"}`)) //nolint:errcheck
			})
			now := time.Now().UnixMilli()
			mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"session": "S1", "last_ts": now, "end_ts": now, "now_ms": now, "ttl_s": 300, //nolint:errcheck
					"threads": []any{map[string]any{"id": "main", "main": true, "ping_reason": reason}}})
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go srv.Serve(ln) //nolint:errcheck
			t.Cleanup(func() { srv.Close() })
			out, _ := runCGCode(t, "http://"+ln.Addr().String(), []string{"XDG_CACHE_HOME=" + t.TempDir()}, "ping", "--session", "S1")
			if !strings.Contains(out, want) || (reason == "from-the-future" && strings.Contains(out, reason)) {
				t.Errorf("reason %q -> %q, want it to contain %q", reason, out, want)
			}
		})
	}
}
