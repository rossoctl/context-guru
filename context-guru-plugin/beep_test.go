package plugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePlayer writes an executable named after the player into dir. It records its arguments in
// the file "played" so a test can tell whether the hook ran it.
func fakePlayer(t *testing.T, dir, name string) string {
	t.Helper()
	played := filepath.Join(dir, "played")
	script := "#!/bin/sh\necho \"" + name + " $*\" >> \"" + played + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return played
}

// runBeep runs scripts/beep.sh with only the given environment, the way Claude Code runs a hook:
// payload on stdin, no terminal.
func runBeep(t *testing.T, env []string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "beep.sh"))
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop","prompt":"secret"}`)
	if err := cmd.Run(); err != nil {
		t.Fatalf("beep.sh must always exit 0: %v", err)
	}
}

// waitPlayed waits for the detached player to write its record, or reports that none came.
func waitPlayed(path string, wait time.Duration) (string, bool) {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b), true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", false
}

// afplay exists only on macOS, so the player tests run there. Elsewhere they would test the
// Linux branches, which need sound files this test cannot assume.
func skipUnlessMac(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/System/Library/Sounds/Ping.aiff"); err != nil {
		t.Skip("macOS system sounds not present")
	}
}

// On is the default: with no setting at all the hook plays the sound.
func TestBeepIsOnByDefault(t *testing.T) {
	skipUnlessMac(t)
	dir := t.TempDir()
	played := fakePlayer(t, dir, "afplay")
	runBeep(t, []string{"PATH=" + dir + ":/usr/bin:/bin"})
	got, ok := waitPlayed(played, 3*time.Second)
	if !ok || !strings.Contains(got, "Ping.aiff") {
		t.Errorf("player record = %q, want a call with Ping.aiff", got)
	}
}

// Both switches turn the sound off, and the environment variable beats the plugin option.
func TestBeepOffPlaysNothing(t *testing.T) {
	skipUnlessMac(t)
	for _, env := range [][]string{
		{"CONTEXT_GURU_BEEP=off"},
		{"CLAUDE_PLUGIN_OPTION_BEEP=off"},
		{"CONTEXT_GURU_BEEP=off", "CLAUDE_PLUGIN_OPTION_BEEP=on"},
	} {
		dir := t.TempDir()
		played := fakePlayer(t, dir, "afplay")
		runBeep(t, append([]string{"PATH=" + dir + ":/usr/bin:/bin"}, env...))
		if got, ok := waitPlayed(played, 400*time.Millisecond); ok {
			t.Errorf("env %v: player ran: %q", env, got)
		}
	}
}

// The environment variable also turns the sound back on over a saved `off`.
func TestBeepEnvironmentOverridesOption(t *testing.T) {
	skipUnlessMac(t)
	dir := t.TempDir()
	played := fakePlayer(t, dir, "afplay")
	runBeep(t, []string{"PATH=" + dir + ":/usr/bin:/bin", "CONTEXT_GURU_BEEP=on", "CLAUDE_PLUGIN_OPTION_BEEP=off"})
	if _, ok := waitPlayed(played, 3*time.Second); !ok {
		t.Error("CONTEXT_GURU_BEEP=on must beat the saved option")
	}
}

// A machine with no player and no terminal stays silent and the hook still exits 0.
func TestBeepWithNoPlayerExitsZero(t *testing.T) {
	runBeep(t, []string{"PATH=" + t.TempDir()})
}

// Stop and Notification both run beep.sh. Notification keeps signal.sh in the same group.
func TestBeepIsWiredToStopAndNotification(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	has := func(event, script string) bool {
		for _, g := range cfg.Hooks[event] {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, script) {
					return true
				}
			}
		}
		return false
	}
	for _, event := range []string{"Stop", "Notification"} {
		if !has(event, "beep.sh") {
			t.Errorf("%s does not run beep.sh", event)
		}
	}
	if !has("Notification", "signal.sh") {
		t.Error("Notification lost signal.sh")
	}
}

// The beep option is declared with the default the script assumes.
func TestBeepOptionDefaultsOn(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		UserConfig map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"userConfig"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if o, ok := m.UserConfig["beep"]; !ok || o.Type != "string" || o.Default != "on" {
		t.Errorf("userConfig.beep = %+v, want a string option defaulting to on", o)
	}
}
