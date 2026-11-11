package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
)

// --- the plugin reads the table from the proxy -------------------------------------------------

// oldProxyBin is a v0.7.0-style binary. It ignores `strategies`, starts serving, and never prints
// JSON (#483). It records the LISTEN_ADDR it was given, and it sleeps far longer than the time limit.
func oldProxyBin(t *testing.T) (bin, startedMarker string) {
	t.Helper()
	dir := t.TempDir()
	startedMarker = filepath.Join(dir, "started")
	bin = filepath.Join(dir, "context-guru-proxy")
	script := "#!/usr/bin/env bash\necho \"LISTEN_ADDR=$LISTEN_ADDR\" > '" + startedMarker + "'\nexec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, startedMarker
}

// stubTableBin is a binary that prints the given JSON for `strategies`.
func stubTableBin(t *testing.T, table string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "context-guru-proxy")
	script := "#!/usr/bin/env bash\n[ \"$1\" = strategies ] && echo '" + table + "' && exit 0\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func settingsWithBin(t *testing.T, bin string, extraEnv []string, args ...string) (map[string]string, int, time.Duration) {
	t.Helper()
	py := requireTool(t, "python3")
	cmd := exec.Command(py, append([]string{filepath.Join(scriptsDir(t), "settings.py")}, args...)...)
	cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+t.TempDir(), "HOME="+t.TempDir(),
		"CONTEXT_GURU_BIN="+bin)
	cmd.Env = append(cmd.Env, extraEnv...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	took := time.Since(start)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running settings.py: %v (%s)", err, out)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			facts[k] = v
		}
	}
	return facts, code, took
}

// TestStrategyListIsReadFromTheProxyBinary. The plugin has no table of its own. A stub binary that
// prints a different table must change what the plugin offers, down to the description text.
func TestStrategyListIsReadFromTheProxyBinary(t *testing.T) {
	bin := stubTableBin(t, `{"default":"zz","aliases":{"old-zz":"zz"},"presets":["off"],"strategies":[`+
		`{"name":"zz","spends":true,"desc":"only the stub says this","providers":["anthropic"],"cache":{}}]}`)
	facts, code, _ := settingsWithBin(t, bin, nil, "strategy", "list")
	if code != 0 || facts["names"] != "zz" || facts["default"] != "zz" ||
		facts["strategy_zz"] != "only the stub says this" || facts["spends_zz"] != "true" ||
		facts["aliases"] != "old-zz:zz" {
		t.Fatalf("the plugin did not take the table from the binary: exit %d %v", code, facts)
	}
}

// TestStrategyListMatchesTheRealTable. The names, texts, spends flags, default and alias the user
// sees are the proxy's own, character for character, minus the strategies for other providers.
func TestStrategyListMatchesTheRealTable(t *testing.T) {
	facts, code := settings(t, "strategy", "list")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, facts)
	}
	var want []string
	for _, s := range config.Strategies {
		if !slices.Contains(s.Providers, "anthropic") && !slices.Contains(s.Providers, "any") {
			continue
		}
		want = append(want, s.Name)
		key := strings.ReplaceAll(s.Name, "-", "_")
		if facts["strategy_"+key] != s.Desc {
			t.Errorf("%s: desc differs from the proxy table", s.Name)
		}
		if facts["spends_"+key] != strconv.FormatBool(s.Spends) {
			t.Errorf("%s: spends=%q, proxy says %v", s.Name, facts["spends_"+key], s.Spends)
		}
	}
	if facts["names"] != strings.Join(want, ",") {
		t.Errorf("names=%q, want %q", facts["names"], strings.Join(want, ","))
	}
	if facts["default"] != config.DefaultStrategy || facts["aliases"] != "5-min-ping:auto" {
		t.Errorf("default/aliases: %v", facts)
	}
}

// TestOnlyAnthropicStrategiesAreOffered. The Claude Code plugin routes Anthropic only, so the
// OpenAI-only `30-min-ping` is not listed and cannot be set.
func TestOnlyAnthropicStrategiesAreOffered(t *testing.T) {
	facts, _ := settings(t, "strategy", "list")
	if strings.Contains(facts["names"], "30-min-ping") || facts["strategy_30_min_ping"] != "" {
		t.Fatalf("30-min-ping is offered to Claude Code: %v", facts)
	}
	set, code := settings(t, "strategy", "set", "--name", "30-min-ping", "--port", keepalivePort, "--preset", "off")
	if code != 2 || set["reason"] != "not_offered" {
		t.Errorf("setting 30-min-ping: exit %d %v; want 2 / not_offered", code, set)
	}
}

// TestAnOldProxyBinaryGetsTheUpdateMessageWithinTheTimeLimit (#483). A binary older than v0.7.1 does
// not fail on `strategies`: it starts serving. The plugin must stop it at the limit, on a loopback
// port that cannot collide, and say plainly to update the proxy. It must not fall back to a table.
func TestAnOldProxyBinaryGetsTheUpdateMessageWithinTheTimeLimit(t *testing.T) {
	bin, marker := oldProxyBin(t)
	facts, code, took := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STRATEGIES_TIMEOUT=1"}, "strategy", "list")
	if code == 0 || facts["reason"] != "proxy_too_old" {
		t.Fatalf("an old binary was accepted: exit %d %v", code, facts)
	}
	if !strings.Contains(facts["note"], "update the proxy to v0.7.1 or later") {
		t.Errorf("the error does not say to update the proxy to v0.7.1: %q", facts["note"])
	}
	if took > 8*time.Second {
		t.Errorf("took %s; the time limit was 1 s", took)
	}
	if got := readFileString(t, marker); !strings.Contains(got, "LISTEN_ADDR=127.0.0.1:0") {
		t.Errorf("the binary was not given a loopback port that cannot collide: %q", got)
	}
	// And no copy of a table answers instead: a write is refused, not served from memory.
	set, code, _ := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STRATEGIES_TIMEOUT=1"},
		"strategy", "set", "--name", "auto", "--port", keepalivePort, "--preset", "off")
	if code == 0 || set["reason"] != "proxy_too_old" {
		t.Errorf("set with an old binary: exit %d %v", code, set)
	}
}

// TestSwitchingOffNeedsNoProxyTable. `none` removes the file. It must work with any binary, even one
// that cannot list strategies, so a user can always turn the pings off.
func TestSwitchingOffNeedsNoProxyTable(t *testing.T) {
	bin, _ := oldProxyBin(t)
	facts, code, _ := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STRATEGIES_TIMEOUT=1"},
		"strategy", "set", "--name", "none", "--port", keepalivePort, "--preset", "off")
	if code != 0 || facts["result"] != "set" {
		t.Fatalf("none with an old binary: exit %d %v", code, facts)
	}
}

// TestNoProxyBinaryIsAClearError. With no binary at all there is nothing to ask.
func TestNoProxyBinaryIsAClearError(t *testing.T) {
	facts, code, _ := settingsWithBin(t, filepath.Join(t.TempDir(), "absent"),
		[]string{"PATH=/usr/bin:/bin", "CONTEXT_GURU_DEST=" + t.TempDir()}, "strategy", "list")
	if code == 0 || facts["reason"] != "no_proxy_binary" {
		t.Fatalf("exit %d %v", code, facts)
	}
}

// TestStrategyFileNamesTheStrategyAndLeavesTheKeysToTheProxy. The per-port file holds
// `cache: strategy: <name>` and none of the keys.
func TestStrategyFileNamesTheStrategyAndLeavesTheKeysToTheProxy(t *testing.T) {
	for _, name := range []string{"auto", "1-ping", "2-ping", "1-hour-head"} {
		t.Run(name, func(t *testing.T) {
			state, home := t.TempDir(), t.TempDir()
			facts, code := settingsIn(t, state, home, "strategy", "set", "--name", name, "--port", keepalivePort, "--preset", "medium")
			if code != 0 {
				t.Fatalf("exit %d %v", code, facts)
			}
			body := readFileString(t, facts["file"])
			if !strings.HasPrefix(body, "# context-guru: strategy="+name+" written by ") ||
				!strings.Contains(body, "\npreset: medium\n") ||
				!strings.HasSuffix(body, "cache:\n  strategy: "+name+"\n") {
				t.Errorf("unexpected file:\n%s", body)
			}
			if strings.Contains(body, "keepalive") || strings.Contains(body, "head_ttl") {
				t.Errorf("the file still carries keys:\n%s", body)
			}
			cfg, err := config.LoadBytes([]byte(body))
			if err != nil || cfg.Cache.Strategy != name || cfg.Preset != "medium" {
				t.Errorf("the proxy loader reads it as %+v (%v)", cfg.Cache, err)
			}
		})
	}
}

// TestSyncKeepsAnOldKeysFileAndOnlyMovesItsPreset. A file an older plugin wrote has explicit keys.
// Sync must not rewrite it to the new form (no binary call at session start) and must keep every
// key. The proxy loads it as it was.
func TestSyncKeepsAnOldKeysFileAndOnlyMovesItsPreset(t *testing.T) {
	state := t.TempDir()
	old := "# context-guru: strategy=auto written by /context-guru:cache-strategy-picker\n" +
		"preset: off\ncache:\n  keepalive: true\n  keepalive_idle_seconds: 240\n  keepalive_max_pings: 2\n" +
		"  keepalive_max_usd_per_ping: 0.5\n  keepalive_min_prefix_tokens: 80000\n"
	path := strategyCfg(state)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// A binary that cannot list strategies: sync must not need it.
	bin, _ := oldProxyBin(t)
	facts, code, took := settingsWithBin(t, bin, []string{"CONTEXT_GURU_STATE=" + state, "CONTEXT_GURU_STRATEGIES_TIMEOUT=30"},
		"strategy", "sync", "--port", keepalivePort, "--preset", "high")
	if code != 0 || facts["result"] != "synced" {
		t.Fatalf("exit %d %v", code, facts)
	}
	if took > 5*time.Second {
		t.Errorf("sync waited %s: it called the binary", took)
	}
	got := readFileString(t, path)
	if want := strings.Replace(old, "preset: off", "preset: high", 1); got != want {
		t.Errorf("sync changed more than the preset line:\n%s", got)
	}
}

// TestSyncOfTheNewFormIsStable. A file in the new form is re-rendered byte for byte, and a second
// sync changes nothing.
func TestSyncOfTheNewFormIsStable(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	if f, code := settingsIn(t, state, home, "strategy", "set", "--name", "1-ping", "--port", keepalivePort, "--preset", "off"); code != 0 {
		t.Fatalf("%d %v", code, f)
	}
	first, _ := settingsIn(t, state, home, "strategy", "sync", "--port", keepalivePort, "--preset", "xhigh")
	second, _ := settingsIn(t, state, home, "strategy", "sync", "--port", keepalivePort, "--preset", "xhigh")
	body := readFileString(t, strategyCfg(state))
	if first["result"] != "synced" || second["result"] != "unchanged" ||
		!strings.Contains(body, "preset: xhigh") || !strings.HasSuffix(body, "  strategy: 1-ping\n") {
		t.Errorf("sync: %v then %v\n%s", first, second, body)
	}
}

// TestEveryPickerPresetIsKnownToTheProxy. The short list stays in the plugin. A preset the proxy does
// not register is an error that names it, not a silent unknown at the next proxy start.
func TestEveryPickerPresetIsKnownToTheProxy(t *testing.T) {
	facts, code := settings(t, "preset", "list")
	if code != 0 || facts["names"] != "off,conservative,medium,high,xhigh,ultrahigh" {
		t.Fatalf("exit %d %v", code, facts)
	}
	bin := stubTableBin(t, `{"default":"auto","aliases":{},"presets":["off","conservative","medium","high","xhigh"],"strategies":[`+
		`{"name":"auto","spends":true,"desc":"d","providers":["anthropic"],"cache":{}}]}`)
	bad, code, _ := settingsWithBin(t, bin, nil, "preset", "list")
	if code == 0 || bad["reason"] != "preset_unknown_to_proxy" || bad["missing"] != "ultrahigh" {
		t.Errorf("a missing preset was not reported: exit %d %v", code, bad)
	}
}
