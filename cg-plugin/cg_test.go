// Golden tests for scripts/cg.py, the /cg:* commands. A stub proxy answers from testdata/cg (and the
// cost bundle) with fixed numbers and a fixed clock, so every screen is byte-for-byte reproducible.
// UPDATE_GOLDEN=1 rewrites the goldens after an intended change; read the diff.
package cgplugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixedNow = 1791473400000 // 2026-10-08T15:30:00Z
const day = 86400000

func read(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// proxyStub serves the endpoints cg.py reads.
func proxyStub(t *testing.T, cache string) string {
	t.Helper()
	bundle := map[string]json.RawMessage{}
	if err := json.Unmarshal(read(t, "cost", "bundle.json"), &bundle); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	file := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.Write(read(t, "cg", name+".json")) }
	}
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since := q.Get("since")
		midnight := now() - now()%day
		switch {
		case q.Get("session") != "":
			w.Write(read(t, "cg", "stats_session.json"))
		case since == fmt.Sprint(midnight):
			w.Write(read(t, "cg", "stats_today.json"))
		case since == fmt.Sprint(midnight-day):
			w.Write(read(t, "cg", "stats_yesterday.json"))
		default:
			w.Write(read(t, "cg", "stats_week.json"))
		}
	})
	mux.HandleFunc("/api/series", file("series"))
	mux.HandleFunc("/api/requests", file("requests"))
	mux.HandleFunc("/api/config", file("config"))
	mux.HandleFunc("/api/cachestate", file(cache))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	for path, key := range map[string]string{"/api/spend/explain": "spend", "/api/tools": "tools", "/api/tools/last-used": "last_used"} {
		body := bundle[key]
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://%s", ln.Addr())
}

func now() int64 { return fixedNow }

func runCG(t *testing.T, base string, env []string, args ...string) string {
	t.Helper()
	requireTool(t, "python3")
	cmd := exec.Command("python3", append([]string{filepath.Join("scripts", "cg.py")}, append(args,
		"--now", "2026-10-08T15:30:00Z", "--tz-offset-min", "0")...)...)
	cfg := t.TempDir()
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"python3 x/statusline.py","refreshInterval":5}}`), 0o644) //nolint:errcheck
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CLAUDE_CONFIG_DIR="+cfg, "CLAUDE_CODE_SESSION_ID=session-A",
		"ANTHROPIC_BASE_URL="+base+"/anthropic", "TZ=UTC")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cg.py %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI escapes in %s; /cg:* output is plain Unicode so it reads on any theme", name)
	}
	path := filepath.Join("testdata", "cg", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := string(read(t, "cg", name+".golden")); got != want {
		t.Errorf("%s differs from its golden.\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestCGGoldens(t *testing.T) {
	warm, cold := proxyStub(t, "cachestate_warm"), proxyStub(t, "cachestate_cold")
	for _, c := range []struct {
		name, base string
		args       []string
	}{
		{"home", warm, []string{"home"}},
		{"savings", warm, []string{"savings"}},
		{"savings-details", warm, []string{"savings", "details"}},
		{"cache-warm", warm, []string{"cache"}},
		{"cache-cold", cold, []string{"cache"}},
		{"waste", warm, []string{"waste"}},
		{"session", warm, []string{"session"}},
		{"doctor", warm, []string{"doctor"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			golden(t, c.name, runCG(t, c.base, nil, c.args...))
		})
	}
}

// The first line of every screen is the answer: one line, no raw token counts.
func TestCGBottomLineFirst(t *testing.T) {
	base := proxyStub(t, "cachestate_warm")
	for _, c := range []string{"home", "savings", "cache", "waste", "session"} {
		out := runCG(t, base, nil, c)
		first := strings.SplitN(out, "\n", 2)[0]
		if len(first) < 20 || strings.Contains(first, "tokens") {
			t.Errorf("%s: first line %q is not a bottom line in dollars", c, first)
		}
	}
}

func TestCGNoProxy(t *testing.T) {
	requireTool(t, "python3")
	cmd := exec.Command("python3", filepath.Join("scripts", "cg.py"), "savings")
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=https://api.anthropic.com", "CG_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "No Context Guru proxy") {
		t.Errorf("want the no-proxy message, got %v: %s", err, out)
	}
}

// The Codex plugin ships the same two scripts, because plugin directories are installed apart and
// cannot import from each other. They are copies, and this keeps them identical.
func TestCGScriptsAreVendoredIntoCodex(t *testing.T) {
	for _, f := range []string{"cg.py", "cgcost.py"} {
		a, err := os.ReadFile(filepath.Join("scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "scripts", f))
		if err != nil || string(a) != string(b) {
			t.Errorf("%s in the Codex plugin differs from cg-plugin/scripts; copy it over (err=%v)", f, err)
		}
	}
}
