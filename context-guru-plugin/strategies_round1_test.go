package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// noBinaryEnv is a route environment where no context-guru-proxy can be found.
func noBinaryEnv(t *testing.T, home, state string) []string {
	t.Helper()
	return append(routeEnv(t, home, state, ""), "PATH=/usr/bin:/bin", "CONTEXT_GURU_DEST="+t.TempDir(), "CONTEXT_GURU_BIN=")
}

// An unchecked --cache-strategy name must never become a fact. With no binary, attach mode installs
// none, so the name is refused before routing is written. --plan says it is unchecked.
func TestAttachModeRefusesAnUncheckedStrategyName(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	env := noBinaryEnv(t, home, state)
	args := []string{"--mode", "attach", "--base-url", "http://127.0.0.1:9", "--no-health-check",
		"--scope", "project", "--cache-strategy", "5-minute-ping"}

	plan, code := runRoute(t, proj, env, append([]string{"--plan"}, args...)...)
	if code != 0 || plan["result"] != "needs_decision" || plan["reason"] != "strategy_unchecked" {
		t.Errorf("plan: exit %d %v; want needs_decision/strategy_unchecked", code, plan)
	}
	if !strings.Contains(plan["note"], "--cache-strategy none") || plan["cache_strategy"] != "" {
		t.Errorf("plan lacks the advice, or reports the name as a fact: %v", plan)
	}

	facts, code := runRoute(t, proj, env, append([]string{"--i-consent-to-traffic-interception"}, args...)...)
	if code != 2 || facts["reason"] != "strategy_unchecked" || facts["result"] == "routed" {
		t.Errorf("confirm: exit %d %v; want 2/strategy_unchecked", code, facts)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "settings.local.json")); err == nil {
		t.Error("routing was written for an unchecked strategy")
	}

	// `none` needs no table, so it still installs.
	ok, code := runRoute(t, proj, env, "--plan", "--mode", "attach", "--base-url", "http://127.0.0.1:9",
		"--scope", "project", "--cache-strategy", "none")
	if code != 0 || ok["result"] != "planned" {
		t.Errorf("none in attach mode: exit %d %v", code, ok)
	}
}

// A local --plan with no binary yet cannot check the name. It must say so.
func TestPlanWithNoBinarySaysTheStrategyIsUnchecked(t *testing.T) {
	home, state, proj := t.TempDir(), t.TempDir(), t.TempDir()
	plan, code := runRoute(t, proj, noBinaryEnv(t, home, state), "--plan", "--scope", "project")
	if code != 0 || plan["result"] != "planned" || plan["strategy_unchecked"] != "true" {
		t.Errorf("exit %d %v; want planned with strategy_unchecked=true", code, plan)
	}
}

// Every bad answer from the binary becomes the clean advice, never a traceback.
func TestBadProxyOutputIsTheCleanAdviceNotACrash(t *testing.T) {
	cases := map[string]string{
		"garbage":       "echo this is not json",
		"a json list":   "echo '[1,2]'",
		"a json string": "echo '\"x\"'",
		"not utf-8":     "printf '\\377\\376\\000{'",
		"bad aliases":   `echo '{"default":"auto","aliases":[1],"strategies":[{"name":"auto","providers":["any"],"spends":true,"desc":"d"}]}'`,
		"bad presets":   `echo '{"default":"auto","presets":"off","strategies":[{"name":"auto","providers":["any"],"spends":true,"desc":"d"}]}'`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "context-guru-proxy")
			if err := os.WriteFile(bin, []byte("#!/usr/bin/env bash\n"+body+"\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"strategy", "list"}, {"strategy", "set", "--name", "auto", "--port", keepalivePort, "--preset", "off"}} {
				facts, code, _ := settingsWithBin(t, bin, nil, args...)
				if code == 0 || facts["reason"] != "proxy_too_old" ||
					!strings.Contains(facts["note"], "update the proxy to v0.7.1 or later") {
					t.Errorf("%v: exit %d %v; want the proxy_too_old advice", args, code, facts)
				}
			}
		})
	}
}

// The probe of a binary that ignores `strategies` must write nothing: no dashboard, no record dir,
// no hosted mode, and a fresh empty working directory.
func TestTheProbeRunsInAScrubbedEnvAndAFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	bin := filepath.Join(dir, "context-guru-proxy")
	script := "#!/usr/bin/env bash\n{ pwd; env; } > '" + rec + "'\n[ \"$1\" = strategies ] && echo '{\"default\":\"auto\",\"strategies\":[{\"name\":\"auto\",\"providers\":[\"any\"],\"spends\":true,\"desc\":\"d\"}]}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	here := t.TempDir()
	extra := []string{"DASHBOARD=1", "DASHBOARD_DB=/x", "DASHBOARD_CONTENT=1", "SESSION_STATE_DIR=/y", "CONTROL_DB=/z",
		"UPSTREAMS=a=b", "CG_RECORD_DIR=/r", "CG_RECORD_OTLP=1", "STORE=/s", "IDLE_EXIT=1h", "KEEP_ME=yes"}
	facts, code, _ := settingsWithBin(t, bin, extra, "strategy", "list")
	if code != 0 || facts["names"] != "auto" {
		t.Fatalf("exit %d %v", code, facts)
	}
	got := readFileString(t, rec)
	lines := strings.Split(got, "\n")
	if lines[0] == here || !strings.Contains(lines[0], "cg-strategies-") {
		t.Errorf("the probe did not run in a fresh temp dir: %q", lines[0])
	}
	for _, bad := range []string{"DASHBOARD_DB=", "DASHBOARD_CONTENT=", "SESSION_STATE_DIR=", "CONTROL_DB=", "UPSTREAMS=",
		"CG_RECORD_DIR=", "CG_RECORD_OTLP=", "STORE=", "IDLE_EXIT="} {
		if strings.Contains("\n"+got, "\n"+bad) {
			t.Errorf("the probe inherited %s", bad)
		}
	}
	for _, want := range []string{"DASHBOARD=0", "LISTEN_ADDR=127.0.0.1:0", "CONFIG=", "KEEP_ME=yes"} {
		if !strings.Contains("\n"+got, "\n"+want+"\n") {
			t.Errorf("the probe env lacks %s", want)
		}
	}
}

// Presets keep their old behavior: saved and warned when no proxy can list them.
func TestPresetSetSavesAndWarnsWhenNoProxyCanListPresets(t *testing.T) {
	for _, tc := range []struct{ name, bin string }{
		{"no binary", filepath.Join(t.TempDir(), "absent")},
		{"old binary", func() string { b, _ := oldProxyBin(t); return b }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, code, _ := settingsWithBin(t, tc.bin,
				[]string{"PATH=/usr/bin:/bin", "CONTEXT_GURU_DEST=" + t.TempDir(), "CONTEXT_GURU_STRATEGIES_TIMEOUT=1"},
				"preset", "list")
			if code != 0 || facts["names"] != "off,conservative,medium,high,xhigh,ultrahigh" || facts["warning"] != "preset_unchecked" {
				t.Errorf("preset list: exit %d %v", code, facts)
			}
		})
	}
	// preset set: saved, with the warning, when there is no proxy to ask.
	url, _ := statsPresetStub(t, "medium")
	state, home, proj := routedProject(t, url)
	cmd := exec.Command(requireTool(t, "python3"), filepath.Join(scriptsDir(t), "settings.py"), "preset", "set", "--name", "medium")
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state, "HOME="+home, "ANTHROPIC_BASE_URL="+url+"/anthropic",
		"PATH=/usr/bin:/bin", "CONTEXT_GURU_DEST="+t.TempDir())
	cmd.Dir = proj
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "result=set") || !strings.Contains(string(out), "warning=preset_unchecked") {
		t.Errorf("preset set with no binary: %v\n%s", err, out)
	}
}

// A named binary that exists but cannot run is an error, not a silent fall-through to PATH.
func TestANonExecutableNamedBinaryIsAnError(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "context-guru-proxy")
	if err := os.WriteFile(bin, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, code, _ := settingsWithBin(t, bin, nil, "strategy", "list")
	if code == 0 || facts["reason"] != "proxy_unrunnable" {
		t.Errorf("exit %d %v; want proxy_unrunnable", code, facts)
	}
}

// `strategy sync` renames an old alias marker, and nothing else.
func TestSyncRenamesAnOldAliasMarker(t *testing.T) {
	state := t.TempDir()
	old := "# context-guru: strategy=5-min-ping written by /context-guru:cache-strategy-picker\n" +
		"preset: off\ncache:\n  keepalive: true\n  keepalive_idle_seconds: 240\n"
	path := strategyCfg(state)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, _ := oldProxyBin(t)
	facts, code, _ := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STATE=" + state}, "strategy", "sync", "--port", keepalivePort, "--preset", "off")
	want := strings.Replace(old, "strategy=5-min-ping", "strategy=auto", 1)
	if code != 0 || facts["result"] != "synced" || readFileString(t, path) != want {
		t.Errorf("exit %d %v:\n%s", code, facts, readFileString(t, path))
	}
	show, _, _ := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STATE=" + state, "CONTEXT_GURU_STRATEGIES_TIMEOUT=1"}, "strategy", "show", "--port", keepalivePort)
	if show["strategy"] != "auto" {
		t.Errorf("show: %v", show)
	}
}
