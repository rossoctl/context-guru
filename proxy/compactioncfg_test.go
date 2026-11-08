package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The start-up read takes the env and the user's files, and nothing else of the files.
func TestReadClaudeCompactReadsEnvAndUserFiles(t *testing.T) {
	home := t.TempDir()
	writeCfg(t, filepath.Join(home, ".claude", "settings.json"), `{
		"autoCompactWindow": 300000, "autoCompactEnabled": true, "apiKeyHelper": "secret-helper",
		"modelSettings": {"claude-opus-5": {"autoCompactWindow": "250k", "effort": "high"}, "x": {"effort": "low"}}}`)
	writeCfg(t, filepath.Join(home, ".claude.json"), `{"autoCompactEnabled": false}`)
	c := ReadClaudeCompact(envOf(map[string]string{
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "60", "DISABLE_AUTO_COMPACT": "1"}), home)
	if c.EnvPct != "60" || c.EnvDisableAuto != "1" || c.SettingsWindow != "300000" ||
		c.SettingsEnabled != "true" /* settings.json beats ~/.claude.json */ ||
		len(c.ModelWindows) != 1 || c.ModelWindows["claude-opus-5"] != "250k" {
		t.Errorf("got %+v", c)
	}
}

func TestReadClaudeCompactHonoursClaudeConfigDir(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeCfg(t, filepath.Join(home, ".claude", "settings.json"), `{"autoCompactWindow": 111111}`)
	writeCfg(t, filepath.Join(dir, "settings.json"), `{"autoCompactWindow": 222222}`)
	c := ReadClaudeCompact(envOf(map[string]string{"CLAUDE_CONFIG_DIR": dir}), home)
	if c.SettingsWindow != "222222" {
		t.Errorf("window = %q, want the CLAUDE_CONFIG_DIR file", c.SettingsWindow)
	}
}

// Fail open: no files, bad JSON, wrong types and the switch all give the zero value.
func TestReadClaudeCompactFailsOpen(t *testing.T) {
	home := t.TempDir()
	if c := ReadClaudeCompact(envOf(nil), home); c.SettingsWindow != "" || c.ModelWindows != nil {
		t.Errorf("no files: %+v", c)
	}
	writeCfg(t, filepath.Join(home, ".claude", "settings.json"), `not json {`)
	if c := ReadClaudeCompact(envOf(nil), home); c.SettingsWindow != "" {
		t.Errorf("bad json: %+v", c)
	}
	writeCfg(t, filepath.Join(home, ".claude", "settings.json"), `{"autoCompactWindow": {"a":1}, "autoCompactEnabled": "no", "modelSettings": 5}`)
	if c := ReadClaudeCompact(envOf(nil), home); c.SettingsWindow != "" || c.SettingsEnabled != "" || c.ModelWindows != nil {
		t.Errorf("wrong types: %+v", c)
	}
	writeCfg(t, filepath.Join(home, ".claude", "settings.json"), `{"autoCompactWindow": 300000}`)
	if c := ReadClaudeCompact(envOf(map[string]string{ClaudeCompactOffEnv: "off", "DISABLE_AUTO_COMPACT": "1"}), home); c.SettingsWindow != "" || c.EnvDisableAuto != "" {
		t.Errorf("switch off: %+v", c)
	}
}

func TestCompactionPointForUsesTheStartupReadInLocalModeOnly(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5"}`)
	h := &Handler{opts: Options{ClaudeCompact: compactionpoint.ClaudeCode{EnvWindow: "200k"}}}
	if p := h.compactionPointFor(body, 1_000_000); p.Source != compactionpoint.Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("local: %+v", p)
	}
	// Another harness's model is not special: the setting is Claude Code's, and it applies to
	// the model name the request carries.
	hosted := &Handler{opts: Options{ClaudeCompact: compactionpoint.ClaudeCode{EnvWindow: "200k"}, Tenants: &TenantSource{}}}
	if p := hosted.compactionPointFor(body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("hosted: %+v", p)
	}
	if p := (&Handler{}).compactionPointFor(body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("nothing set: %+v", p)
	}
}
