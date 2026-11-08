package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
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

func reqWithAgent(ua string) *http.Request {
	r := httptest.NewRequest("POST", "/anthropic/v1/messages", nil)
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	return r
}

func TestCompactionPointForUsesTheStartupReadInLocalClaudeCodeOnly(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5"}`)
	cc := reqWithAgent("claude-cli/2.1.319 (external, cli)")
	h := &Handler{opts: Options{ClaudeCompact: compactionpoint.ClaudeCode{EnvWindow: "200k"}}}
	if p := h.compactionPointFor(cc, body, 1_000_000); p.Source != compactionpoint.Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("local claude code: %+v", p)
	}
	// Codex, Bob, an unknown client and no User-Agent get the table.
	for _, ua := range []string{"codex_cli_rs/0.160.0", "bob/1.2", "curl/8", ""} {
		if p := h.compactionPointFor(reqWithAgent(ua), body, 1_000_000); p.Source != compactionpoint.Assumed {
			t.Errorf("agent %q: %+v, want the table", ua, p)
		}
	}
	hosted := &Handler{opts: Options{ClaudeCompact: compactionpoint.ClaudeCode{EnvWindow: "200k"}, Tenants: &TenantSource{}}}
	if p := hosted.compactionPointFor(cc, body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("hosted: %+v", p)
	}
	if p := (&Handler{}).compactionPointFor(cc, body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("nothing set: %+v", p)
	}
}

// No home directory: no settings file is read, so a relative path cannot reach a project's
// .claude/settings.json. Env still counts.
func TestReadClaudeCompactReadsNoFileWithoutAHome(t *testing.T) {
	cwd := t.TempDir()
	writeCfg(t, filepath.Join(cwd, ".claude", "settings.json"), `{"autoCompactWindow": 300000}`)
	t.Chdir(cwd)
	c := ReadClaudeCompact(envOf(map[string]string{"DISABLE_AUTO_COMPACT": "1"}), "")
	if c.SettingsWindow != "" || c.EnvDisableAuto != "1" {
		t.Errorf("got %+v", c)
	}
	// A relative CLAUDE_CONFIG_DIR is not read either.
	writeCfg(t, filepath.Join(cwd, "rel", "settings.json"), `{"autoCompactWindow": 300000}`)
	if c := ReadClaudeCompact(envOf(map[string]string{"CLAUDE_CONFIG_DIR": "rel"}), t.TempDir()); c.SettingsWindow != "" {
		t.Errorf("relative config dir: %+v", c)
	}
}

// The /compact endpoint records on its row the same C it gated on (#948). Before this was fixed the
// gate used the configured C and the row carried none, so a later dashboard walk read the table.
func TestCompactEndpointRecordsTheConfiguredPoint(t *testing.T) {
	agg := metrics.NewAggregator()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: t.TempDir() + "/d.db", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		Preset: cfg.Preset, Dashboard: rec, Prices: fixedPricer{},
		ClaudeCompact: compactionpoint.ClaudeCode{EnvWindow: "200k"},
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	for _, ua := range []string{"claude-cli/2.1.319 (external, cli)", "codex_cli_rs/0.160.0"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/compact?provider=anthropic",
			strings.NewReader(string(anthropicRequest("claude-opus-5"))))
		req.Header.Set("User-Agent", ua)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	waitForRows(t, rec, 2)
	page, _ := rec.DB().Requests(dash.Filter{}, 0, 10)
	got := map[string]int{}
	for _, e := range page.Requests {
		got[e.Agent] = e.CompactionPoint
		if e.Agent == "claude-cli" && (e.CompactionPointSource != "configured") {
			t.Errorf("claude-cli row source = %q", e.CompactionPointSource)
		}
	}
	if got["claude-cli"] == 0 || got["codex"] != 0 {
		t.Errorf("recorded points = %v, want a configured C on the Claude Code row only", got)
	}
}

func TestReadBobCompact(t *testing.T) {
	home := t.TempDir()
	if b := ReadBobCompact(envOf(map[string]string{"BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": filepath.Join(home, "none.json")}), home); !b.Read || b.Threshold != "" {
		t.Errorf("no file: %+v", b)
	}
	writeCfg(t, filepath.Join(home, ".bob", "settings.json"), `{"model":{"compressionThreshold":0.8,"other":"x"},"ibm":{"teamId":"secret"}}`)
	sys := filepath.Join(t.TempDir(), "s.json")
	env := envOf(map[string]string{"BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH": sys})
	if b := ReadBobCompact(env, home); b.Threshold != "0.8" {
		t.Errorf("user file: %+v", b)
	}
	writeCfg(t, sys, `{"model":{"compressionThreshold":0.6}}`)
	if b := ReadBobCompact(env, home); b.Threshold != "0.6" {
		t.Errorf("system file must win: %+v", b)
	}
	off := envOf(map[string]string{ClaudeCompactOffEnv: "off"})
	if b := ReadBobCompact(off, home); b.Read || b.Threshold != "" {
		t.Errorf("switch off: %+v", b)
	}
}

func TestReadCodexCompact(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, filepath.Join(dir, "config.toml"), "model = \"gpt-6-sol\"\nmodel_auto_compact_token_limit = 180000 # c\nmodel_auto_compact_token_limit_scope = \"total\"\n\n[tui]\nmodel_auto_compact_token_limit = 5\n")
	c := ReadCodexCompact(envOf(map[string]string{"CODEX_HOME": dir}), "")
	if c.Limit != "180000" || c.Scope != "total" || c.PostTurn != "" {
		t.Errorf("got %+v", c)
	}
	home := t.TempDir()
	writeCfg(t, filepath.Join(home, ".codex", "config.toml"), "model_post_turn_compact_threshold_percent = 70\n")
	if c := ReadCodexCompact(envOf(nil), home); c.PostTurn != "70" {
		t.Errorf("default dir: %+v", c)
	}
	if c := ReadCodexCompact(envOf(map[string]string{ClaudeCompactOffEnv: "off", "CODEX_HOME": dir}), home); c != (compactionpoint.Codex{}) {
		t.Errorf("switch off: %+v", c)
	}
	if c := ReadCodexCompact(envOf(nil), t.TempDir()); c != (compactionpoint.Codex{}) {
		t.Errorf("no file: %+v", c)
	}
}

// The request's User-Agent chooses the client's rule.
func TestCompactionPointForBobAndCodex(t *testing.T) {
	h := &Handler{opts: Options{
		BobCompact:   compactionpoint.Bob{Read: true},
		CodexCompact: compactionpoint.Codex{Limit: "150000"},
	}}
	body := []byte(`{"model":"claude-opus-5"}`)
	req := func(ua string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("User-Agent", ua)
		return r
	}
	if p := h.compactionPointFor(req("bob-shell/1.0.3"), body, 1_000_000); p.Source != compactionpoint.ClientDefault || p.Tokens(1_000_000) != 500_000 {
		t.Errorf("bob: %+v", p)
	}
	if p := h.compactionPointFor(req("codex_cli_rs/0.160.0"), body, 272_000); p.Source != compactionpoint.Configured || p.Tokens(272_000) != 150_000 {
		t.Errorf("codex: %+v", p)
	}
	if p := h.compactionPointFor(req("curl/8"), body, 1_000_000); p.Source != compactionpoint.Assumed {
		t.Errorf("other client must use the table: %+v", p)
	}
	h.opts.Tenants = &TenantSource{}
	if p := h.compactionPointFor(req("bob-shell/1.0.3"), body, 1_000_000); p.Source == compactionpoint.ClientDefault {
		t.Error("hosted proxy must read nothing")
	}
}
