package compactionpoint

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// mapOpen opens a path in an in-memory tree, with the leading "/" removed. It records each path it is given.
func mapOpen(fsys fstest.MapFS, seen *[]string) Open {
	return func(p string) (fs.File, error) {
		if seen != nil {
			*seen = append(*seen, p)
		}
		return fsys.Open(strings.TrimPrefix(p, "/"))
	}
}

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func TestPickChoosesTheRuleOfTheClient(t *testing.T) {
	s := Settings{Claude: ClaudeCode{EnvWindow: "200k"}, Bob: Bob{Read: true}, Codex: Codex{Limit: "150000"}}
	if p, ok := s.Pick(ClientClaude, "claude-opus-5", 1_000_000, nil); !ok || p.Source != Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("claude: %+v %v", p, ok)
	}
	if p, ok := s.Pick(ClientBob, "m", 1_000_000, func(string) int { return 400_000 }); !ok || p.Tokens(1_000_000) != 200_000 {
		t.Errorf("bob: %+v %v", p, ok)
	}
	if p, ok := s.Pick(ClientCodex, "m", 272_000, nil); !ok || p.Tokens(272_000) != 150_000 {
		t.Errorf("codex: %+v %v", p, ok)
	}
	if _, ok := s.Pick(ClientNone, "m", 1_000_000, nil); ok {
		t.Error("another client must get the table")
	}
	if _, ok := (Settings{}).Pick(ClientClaude, "claude-opus-5", 1_000_000, nil); ok {
		t.Error("zero settings must change nothing")
	}
}

func TestCleanBobWindows(t *testing.T) {
	got := CleanBobWindows(map[string]int{"good": 1000, "zero": 0, "neg": -5}, 512)
	if len(got) != 1 || got["good"] != 1000 {
		t.Errorf("got %v", got)
	}
	if CleanBobWindows(nil, 512) != nil || CleanBobWindows(map[string]int{"z": 0}, 512) != nil {
		t.Error("nothing to store must give nil")
	}
	const cap = 512
	big := make(map[string]int, cap+1)
	for i := 0; i <= cap; i++ {
		big[fmt.Sprint("m", i)] = 1
	}
	if CleanBobWindows(big, cap) != nil {
		t.Error("an over-cap catalog must give nil")
	}
}

func TestReadClaude(t *testing.T) {
	fsys := fstest.MapFS{
		"home/u/.claude.json":          file(`{"autoCompactEnabled":false}`),
		"home/u/.claude/settings.json": file(`{"autoCompactWindow":"300k","modelSettings":{"claude-opus-5":{"autoCompactWindow":250000}}}`),
		"cfg/settings.json":            file(`{"autoCompactWindow":123}`),
	}
	c := ReadClaude(envOf(map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "200k"}), "/home/u", mapOpen(fsys, nil))
	if c.EnvWindow != "200k" || c.SettingsWindow != "300k" || c.SettingsEnabled != "false" || c.ModelWindows["claude-opus-5"] != "250000" {
		t.Errorf("got %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}), "/home/u", mapOpen(fsys, nil)); c.SettingsWindow != "123" || c.SettingsEnabled != "" {
		t.Errorf("CLAUDE_CONFIG_DIR: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"CLAUDE_CONFIG_DIR": "cfg"}), "/home/u", mapOpen(fsys, nil)); c.SettingsWindow != "" {
		t.Errorf("relative dir must not be read: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"DISABLE_AUTO_COMPACT": "1"}), "", mapOpen(fsys, nil)); c.EnvDisableAuto != "1" || c.SettingsWindow != "" {
		t.Errorf("no home reads env only: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{OffEnv: "off", "DISABLE_AUTO_COMPACT": "1"}), "/home/u", mapOpen(fsys, nil)); c.SettingsWindow != "" || c.EnvDisableAuto != "" {
		t.Errorf("switch off: %+v", c)
	}
	bad := fstest.MapFS{"home/u/.claude/settings.json": file(`not json`)}
	if c := ReadClaude(envOf(nil), "/home/u", mapOpen(bad, nil)); c.SettingsWindow != "" {
		t.Errorf("bad file: %+v", c)
	}
}

func TestReadBob(t *testing.T) {
	fsys := fstest.MapFS{"home/u/.bob/settings.json": file(`{"model":{"compressionThreshold":0.8}}`)}
	sys := envOf(map[string]string{"BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": "/sys/s.json"})
	if b := ReadBob(sys, "/home/u", "linux", mapOpen(fsys, nil)); !b.Read || b.Threshold != "0.8" {
		t.Errorf("user file: %+v", b)
	}
	fsys["sys/s.json"] = file(`{"model":{"compressionThreshold":0.6}}`)
	if b := ReadBob(sys, "/home/u", "linux", mapOpen(fsys, nil)); b.Threshold != "0.6" {
		t.Errorf("system file must win: %+v", b)
	}
	if b := ReadBob(envOf(nil), "/nohome", "linux", mapOpen(fsys, nil)); !b.Read || b.Threshold != "" {
		t.Errorf("no file: %+v", b)
	}
	if b := ReadBob(envOf(map[string]string{OffEnv: "off", "BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": "/sys/s.json"}), "/home/u", "linux", mapOpen(fsys, nil)); b.Read || b.Threshold != "" {
		t.Errorf("switch off: %+v", b)
	}
}

func TestBobSystemSettingsPath(t *testing.T) {
	for _, tc := range []struct{ env, goos, want string }{
		{"", "darwin", "/Library/Application Support/Bob-Shell/settings.json"},
		{"", "linux", "/etc/bob-shell/settings.json"},
		{"", "windows", "/etc/bob-shell/settings.json"},
		{" /x/s.json ", "darwin", "/x/s.json"},
	} {
		if got := BobSystemSettingsPath(envOf(map[string]string{"BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": tc.env}), tc.goos); got != tc.want {
			t.Errorf("env %q goos %s = %q, want %q", tc.env, tc.goos, got, tc.want)
		}
	}
}

// The library hands the path to the opener exactly as built: no rewriting, so a `.` or `..`
// element reaches os.Open as it always did.
func TestReadersPassPathsUnchangedToTheOpener(t *testing.T) {
	var seen []string
	ReadBob(envOf(map[string]string{"BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": "/a/./b/../s.json"}), "", "linux", mapOpen(fstest.MapFS{}, &seen))
	if len(seen) != 1 || seen[0] != "/a/./b/../s.json" {
		t.Errorf("opened %v", seen)
	}
}

func TestReadCodex(t *testing.T) {
	fsys := fstest.MapFS{
		"cx/config.toml":            file("model = \"gpt-6\"\nmodel_auto_compact_token_limit = 180000 # c\nmodel_auto_compact_token_limit_scope = \"total\"\n\n[tui]\nmodel_auto_compact_token_limit = 5\n"),
		"home/u/.codex/config.toml": file("model_post_turn_compact_threshold_percent = 70\n"),
	}
	if c := ReadCodex(envOf(map[string]string{"CODEX_HOME": "/cx"}), "", mapOpen(fsys, nil)); c.Limit != "180000" || c.Scope != "total" || c.PostTurn != "" {
		t.Errorf("got %+v", c)
	}
	if c := ReadCodex(envOf(nil), "/home/u", mapOpen(fsys, nil)); c.PostTurn != "70" {
		t.Errorf("default dir: %+v", c)
	}
	if c := ReadCodex(envOf(map[string]string{OffEnv: "off", "CODEX_HOME": "/cx"}), "/home/u", mapOpen(fsys, nil)); c != (Codex{}) {
		t.Errorf("switch off: %+v", c)
	}
	if c := ReadCodex(envOf(nil), "/empty", mapOpen(fsys, nil)); c != (Codex{}) {
		t.Errorf("no file: %+v", c)
	}
}
