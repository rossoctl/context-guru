package compactionpoint

import (
	"fmt"
	"testing"
	"testing/fstest"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func TestClientOf(t *testing.T) {
	for ua, want := range map[string]Client{
		"claude-cli/2.1.319 (external, cli)": ClientClaude,
		"Claude-Code/1.0":                    ClientClaude,
		"bobshell/1.0.3":                     ClientBob,
		"codex_cli_rs/0.160.0":               ClientCodex,
		"codex_exec":                         ClientCodex,
		"curl/8":                             ClientNone,
		"bobcat/1.0":                         ClientNone,
		"my-bob-tool/2":                      ClientNone,
		"xcodex/1":                           ClientNone,
		"":                                   ClientNone,
	} {
		if got := ClientOf(ua); got != want {
			t.Errorf("ClientOf(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestPickChoosesTheRuleOfTheClient(t *testing.T) {
	s := Settings{Claude: ClaudeCode{EnvWindow: "200k"}, Bob: Bob{Read: true}, Codex: Codex{Limit: "150000"}}
	if p, ok := s.Pick("claude-cli/2", "claude-opus-5", 1_000_000, nil); !ok || p.Source != Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("claude: %+v %v", p, ok)
	}
	if p, ok := s.Pick("bobshell/1", "m", 1_000_000, func(string) int { return 400_000 }); !ok || p.Tokens(1_000_000) != 200_000 {
		t.Errorf("bob: %+v %v", p, ok)
	}
	if p, ok := s.Pick("codex_cli_rs/1", "m", 272_000, nil); !ok || p.Tokens(272_000) != 150_000 {
		t.Errorf("codex: %+v %v", p, ok)
	}
	if _, ok := s.Pick("curl/8", "m", 1_000_000, nil); ok {
		t.Error("another client must get the table")
	}
	if _, ok := (Settings{}).Pick("claude-cli/2", "claude-opus-5", 1_000_000, nil); ok {
		t.Error("zero settings must change nothing")
	}
}

func TestCleanBobWindows(t *testing.T) {
	got := CleanBobWindows(map[string]int{"good": 1000, "zero": 0, "neg": -5})
	if len(got) != 1 || got["good"] != 1000 {
		t.Errorf("got %v", got)
	}
	if CleanBobWindows(nil) != nil || CleanBobWindows(map[string]int{"z": 0}) != nil {
		t.Error("nothing to store must give nil")
	}
	big := make(map[string]int, MaxBobWindows+1)
	for i := 0; i <= MaxBobWindows; i++ {
		big[fmt.Sprint("m", i)] = 1
	}
	if CleanBobWindows(big) != nil {
		t.Error("an over-cap catalog must give nil")
	}
}

func TestReadClaude(t *testing.T) {
	fsys := fstest.MapFS{
		"home/u/.claude.json":          file(`{"autoCompactEnabled":false}`),
		"home/u/.claude/settings.json": file(`{"autoCompactWindow":"300k","modelSettings":{"claude-opus-5":{"autoCompactWindow":250000}}}`),
		"cfg/settings.json":            file(`{"autoCompactWindow":123}`),
	}
	c := ReadClaude(envOf(map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "200k"}), "/home/u", fsys)
	if c.EnvWindow != "200k" || c.SettingsWindow != "300k" || c.SettingsEnabled != "false" || c.ModelWindows["claude-opus-5"] != "250000" {
		t.Errorf("got %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}), "/home/u", fsys); c.SettingsWindow != "123" || c.SettingsEnabled != "" {
		t.Errorf("CLAUDE_CONFIG_DIR: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"CLAUDE_CONFIG_DIR": "cfg"}), "/home/u", fsys); c.SettingsWindow != "" {
		t.Errorf("relative dir must not be read: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{"DISABLE_AUTO_COMPACT": "1"}), "", fsys); c.EnvDisableAuto != "1" || c.SettingsWindow != "" {
		t.Errorf("no home reads env only: %+v", c)
	}
	if c := ReadClaude(envOf(map[string]string{OffEnv: "off", "DISABLE_AUTO_COMPACT": "1"}), "/home/u", fsys); c.SettingsWindow != "" || c.EnvDisableAuto != "" {
		t.Errorf("switch off: %+v", c)
	}
	bad := fstest.MapFS{"home/u/.claude/settings.json": file(`not json`)}
	if c := ReadClaude(envOf(nil), "/home/u", bad); c.SettingsWindow != "" {
		t.Errorf("bad file: %+v", c)
	}
}

func TestReadBob(t *testing.T) {
	fsys := fstest.MapFS{"home/u/.bob/settings.json": file(`{"model":{"compressionThreshold":0.8}}`)}
	if b := ReadBob(envOf(nil), "/home/u", "/sys/s.json", fsys); !b.Read || b.Threshold != "0.8" {
		t.Errorf("user file: %+v", b)
	}
	fsys["sys/s.json"] = file(`{"model":{"compressionThreshold":0.6}}`)
	if b := ReadBob(envOf(nil), "/home/u", "/sys/s.json", fsys); b.Threshold != "0.6" {
		t.Errorf("system file must win: %+v", b)
	}
	if b := ReadBob(envOf(nil), "/nohome", "/sys/none.json", fsys); !b.Read || b.Threshold != "" {
		t.Errorf("no file: %+v", b)
	}
	if b := ReadBob(envOf(map[string]string{OffEnv: "off"}), "/home/u", "/sys/s.json", fsys); b.Read || b.Threshold != "" {
		t.Errorf("switch off: %+v", b)
	}
}

func TestReadCodex(t *testing.T) {
	fsys := fstest.MapFS{
		"cx/config.toml":            file("model = \"gpt-6\"\nmodel_auto_compact_token_limit = 180000 # c\nmodel_auto_compact_token_limit_scope = \"total\"\n\n[tui]\nmodel_auto_compact_token_limit = 5\n"),
		"home/u/.codex/config.toml": file("model_post_turn_compact_threshold_percent = 70\n"),
	}
	if c := ReadCodex(envOf(map[string]string{"CODEX_HOME": "/cx"}), "", fsys); c.Limit != "180000" || c.Scope != "total" || c.PostTurn != "" {
		t.Errorf("got %+v", c)
	}
	if c := ReadCodex(envOf(nil), "/home/u", fsys); c.PostTurn != "70" {
		t.Errorf("default dir: %+v", c)
	}
	if c := ReadCodex(envOf(map[string]string{OffEnv: "off", "CODEX_HOME": "/cx"}), "/home/u", fsys); c != (Codex{}) {
		t.Errorf("switch off: %+v", c)
	}
	if c := ReadCodex(envOf(nil), "/empty", fsys); c != (Codex{}) {
		t.Errorf("no file: %+v", c)
	}
}
