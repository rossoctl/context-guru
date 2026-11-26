package plugin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// statsPresetStub answers /stats with the given running preset and records each probe ask.
func statsPresetStub(t *testing.T, running string) (url string, asks *[]string) {
	t.Helper()
	var mu sync.Mutex
	got := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/stats":
			fmt.Fprintf(w, `{"preset":%q}`, running)
		case r.URL.Path == "/tool-defer/probe":
			mu.Lock()
			got = append(got, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Write([]byte("ok")) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

// routedProject writes a project-local routing record for url and returns the state, home and project dirs.
func routedProject(t *testing.T, url string) (state, home, proj string) {
	t.Helper()
	state, home, proj = t.TempDir(), t.TempDir(), t.TempDir()
	projLocal := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(projLocal), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, code := settingsInDir(t, state, home, proj, "add", "--file", projLocal, "--url", url+"/anthropic"); code != 0 {
		t.Fatal("fixture: routing add failed")
	}
	return state, home, proj
}

// presetIn runs `settings.py preset <args>` in proj with the routing URL in its environment, as a
// session's Bash call has it, and returns the key=value facts and the exit code.
func presetIn(t *testing.T, state, home, proj, url string, args ...string) (map[string]string, int) {
	t.Helper()
	cmd := exec.Command(requireTool(t, "python3"), append([]string{filepath.Join(scriptsDir(t), "settings.py"), "preset"}, args...)...)
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home, "ANTHROPIC_BASE_URL="+url+"/anthropic",
		"CONTEXT_GURU_BIN="+fakeStrategiesBin(t))
	cmd.Dir = proj
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			facts[k] = v
		}
	}
	return facts, code
}

// #887: while the running proxy runs another preset, `preset set` says a restart is needed and sends
// NO probe (the old proxy would take it and do nothing with it).
func TestPresetSetWithAStaleProxyAsksForARestartAndSendsNoProbe(t *testing.T) {
	url, asks := statsPresetStub(t, "xhigh")
	state, home, proj := routedProject(t, url)
	facts, code := presetIn(t, state, home, proj, url, "set", "--name", "ultrahigh")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	if facts["restart_needed"] != "true" || facts["running"] != "xhigh" {
		t.Errorf("want restart_needed=true running=xhigh, got %v", facts)
	}
	if facts["gap"] != "preset: xhigh (ultrahigh after a restart)" {
		t.Errorf("gap = %q", facts["gap"])
	}
	if !strings.Contains(facts["note"], "cache rewrite") || !strings.Contains(facts["note"], "until the next session start") {
		t.Errorf("the note must name the cost of a restart and what a no means: %q", facts["note"])
	}
	if _, has := facts["probe"]; has || len(*asks) != 0 {
		t.Errorf("a probe went to the old proxy: %v %v", facts, *asks)
	}
}

// When the running proxy already runs the preset, nothing restarts, and ultrahigh asks as before.
func TestPresetSetOnAProxyThatAlreadyRunsItAsksForTheProbe(t *testing.T) {
	url, asks := statsPresetStub(t, "ultrahigh")
	state, home, proj := routedProject(t, url)
	facts, code := presetIn(t, state, home, proj, url, "set", "--name", "ultrahigh")
	if code != 0 || facts["probe"] != "asked" || facts["restart_needed"] == "true" || len(*asks) != 1 {
		t.Fatalf("exit %d, facts %v, asks %v", code, facts, *asks)
	}
}

// `preset restart` goes through start-proxy.sh: the old proxy (started with xhigh) is replaced, /stats
// then reports the new preset, and the probe goes to the RESTARTED proxy.
func TestPresetRestartReplacesTheProxyThroughTheStarterAndProbesTheNewOne(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	asksFile := filepath.Join(dir, "asks")
	bin := filepath.Join(dir, "fake-proxy")
	body := "#!/usr/bin/env bash\n" + strategiesStubSnippet(t) + "exec python3 -c '\n" +
		"import sys, os, http.server\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200); self.end_headers()\n" +
		"        self.wfile.write((\"{\\\"preset\\\":\\\"\" + os.environ.get(\"PRESET\", \"\") + \"\\\"}\").encode() if self.path == \"/stats\" else b\"ok\")\n" +
		"    def do_POST(self):\n" +
		"        open(sys.argv[2], \"a\").write(os.environ.get(\"PRESET\", \"\") + \"\\n\")\n" +
		"        self.send_response(202); self.end_headers()\n" +
		"    def log_message(self, *a): pass\n" +
		"http.server.HTTPServer((\"127.0.0.1\", int(sys.argv[1])), H).serve_forever()\n" +
		"' " + port + " " + asksFile + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	url := "http://127.0.0.1:" + port
	state, home, proj := routedProject(t, url)
	pidfile := filepath.Join(state, "proxy-"+port+".pid")
	t.Cleanup(func() { stopByPidfile(pidfile) })

	env := func() []string {
		return append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home, "TMPDIR="+dir,
			"CONTEXT_GURU_BIN="+bin, "ANTHROPIC_BASE_URL="+url+"/anthropic", "CONTEXT_GURU_HEALTH_BUDGET=10")
	}
	start := exec.Command("bash", filepath.Join(scriptsDir(t), "start-proxy.sh"), "--preset", "xhigh")
	start.Env, start.Dir = env(), proj
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("first start: %v\n%s", err, out)
	}
	first := strings.TrimSpace(readFileString(t, pidfile))

	run := exec.Command(requireTool(t, "python3"), filepath.Join(scriptsDir(t), "settings.py"), "preset", "restart", "--name", "ultrahigh")
	run.Env, run.Dir = env(), proj
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("preset restart: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	for _, want := range []string{"result=restarted", "running=ultrahigh", "probe=asked", "configuration changed, restarting"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if second := strings.TrimSpace(readFileString(t, pidfile)); second == first {
		t.Errorf("the proxy was not replaced (pid %s)", first)
	}
	// The probe reached the restarted proxy (which runs ultrahigh), not the old one.
	if got := strings.TrimSpace(readFileString(t, asksFile)); got != "ultrahigh" {
		t.Errorf("the probe went to a proxy running %q", got)
	}
}

// `preset restart` on a port that is not routed refuses and starts nothing.
func TestPresetRestartRefusesAnUnroutedDirectory(t *testing.T) {
	facts, code := presetIn(t, t.TempDir(), t.TempDir(), t.TempDir(), "http://127.0.0.1:9", "restart", "--name", "high")
	if code == 0 || facts["reason"] != "not_routed" {
		t.Fatalf("exit %d, %v", code, facts)
	}
}

// The status line shows a saved preset the proxy does not run yet, and nothing when they agree.
func TestStatuslineShowsASavedPresetThatWaitsForARestart(t *testing.T) {
	const cache = `{"session":"s","model":"m","last_ts":1799999940000,"now_ms":1800000000000,"ttl_s":300,"prefix_tokens":52000,"priced":true,"rewrite_usd":0.3,"window":1000000,"window_exact":true}`
	for _, c := range []struct{ saved, running, want string }{
		{"ultrahigh", "xhigh", "preset: xhigh (ultrahigh after a restart)"},
		{"xhigh", "xhigh", ""},
		{"ultrahigh", "", ""}, // an older proxy has no preset key: no guess
	} {
		t.Run(c.saved+"/"+c.running, func(t *testing.T) {
			port := cgStub(t, cache, func(m *http.ServeMux) {
				m.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
					if c.running == "" {
						w.Write([]byte(`{}`)) //nolint:errcheck
						return
					}
					fmt.Fprintf(w, `{"preset":%q}`, c.running)
				})
			})
			cfg := t.TempDir()
			settings := fmt.Sprintf(`{"pluginConfigs":{"context-guru@context-guru":{"options":{"preset":%q}}}}`, c.saved)
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(settings), 0o644); err != nil {
				t.Fatal(err)
			}
			env := routedEnv(port)
			env["CLAUDE_CONFIG_DIR"] = cfg
			out, code, _ := runStatusline(t, env, `{"session_id":"sess-gold"}`, "--layout=balanced")
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if has := strings.Contains(out, "preset:"); has != (c.want != "") || (c.want != "" && !strings.Contains(out, c.want)) {
				t.Errorf("want %q in the line, got %q", c.want, out)
			}
		})
	}
}
