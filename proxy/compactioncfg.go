package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
)

// The user's own Claude Code auto-compact settings (#948).
//
// WHEN IT IS READ: ONCE, when the proxy starts, in local mode only (cmd/context-guru-proxy calls
// ReadClaudeCompact and hands the result to Options.ClaudeCompact). It is held in memory for the
// life of the proxy. A setting changed while the proxy runs takes effect at the next proxy restart.
// No per-session or per-request report, no stored copy: nothing to purge.
//
// WHERE: the host, because only the host can read the user's files and env. The rule that turns the
// raw values into C stays in internal/compactionpoint (ClaudeCode.Resolve), which never imports
// proxy.
//
// WHICH ENV. The plugin starts the proxy from the SessionStart hook (start-proxy.sh, `setsid` or
// `nohup`, no `env -i`), so the proxy inherits the env of the hook, which is the env of the Claude
// Code session that started it. That is the env of ONE session: a later session with other values
// does not change a running proxy. A proxy started by hand reads the env of that shell.
//
// WHICH FILES (the sources the client reads, verified on claude 2.1.319): the user settings file
// ${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json, and the legacy global config
// ${CLAUDE_CONFIG_DIR:-~}/.claude.json for autoCompactEnabled. Project settings files depend on the
// directory the client runs in, which the proxy does not know, so they are NOT read.

// ClaudeCompactOffEnv turns the start-up read off ("off", "0" or "false"). C then comes from the
// table.
const ClaudeCompactOffEnv = "CONTEXT_GURU_CLIENT_COMPACT_CONFIG"

// ReadClaudeCompact reads the settings from env and the user's settings files. getenv and home are
// injected for tests. It never fails: an unreadable or odd file is skipped (fail open).
func ReadClaudeCompact(getenv func(string) string, home string) compactionpoint.ClaudeCode {
	switch strings.ToLower(strings.TrimSpace(getenv(ClaudeCompactOffEnv))) {
	case "off", "0", "false":
		return compactionpoint.ClaudeCode{}
	}
	c := compactionpoint.ClaudeCode{
		EnvWindow:      short(getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW")),
		EnvPct:         short(getenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE")),
		EnvDisableAuto: short(getenv("DISABLE_AUTO_COMPACT")),
		EnvDisable:     short(getenv("DISABLE_COMPACT")),
	}
	dir := strings.TrimSpace(getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" && home == "" {
		return c // env only: with no home, a relative path would name a project file
	}
	if dir != "" && !filepath.IsAbs(dir) {
		return c // a relative CLAUDE_CONFIG_DIR depends on the working directory: not read
	}
	globalCfg := filepath.Join(home, ".claude.json")
	settings := filepath.Join(home, ".claude", "settings.json")
	if dir != "" {
		globalCfg = filepath.Join(dir, ".claude.json")
		settings = filepath.Join(dir, "settings.json")
	}
	// ~/.claude.json first, then settings.json: the settings file wins (the client's own order puts
	// userSettings above the legacy global config).
	if b := readSmall(globalCfg); b != nil {
		if v := gjson.GetBytes(b, "autoCompactEnabled"); v.Type == gjson.True || v.Type == gjson.False {
			c.SettingsEnabled = fmt.Sprint(v.Bool())
		}
	}
	if b := readSmall(settings); b != nil {
		if v := gjson.GetBytes(b, "autoCompactWindow"); v.Type == gjson.Number || v.Type == gjson.String {
			c.SettingsWindow = short(v.String())
		}
		if v := gjson.GetBytes(b, "autoCompactEnabled"); v.Type == gjson.True || v.Type == gjson.False {
			c.SettingsEnabled = fmt.Sprint(v.Bool())
		}
		if ms := gjson.GetBytes(b, "modelSettings"); ms.IsObject() {
			n := 0
			ms.ForEach(func(name, val gjson.Result) bool {
				if n >= 16 {
					return false
				}
				w := val.Get("autoCompactWindow")
				if len(name.String()) <= 64 && (w.Type == gjson.Number || w.Type == gjson.String) {
					if c.ModelWindows == nil {
						c.ModelWindows = map[string]string{}
					}
					c.ModelWindows[name.String()] = short(w.String())
					n++
				}
				return true
			})
		}
	}
	return c
}

// readSmall reads a settings file of at most 4 MiB, or nil. A missing file is the normal case.
func readSmall(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 4<<20 {
		return nil
	}
	var b []byte
	dec := json.NewDecoder(f)
	var raw json.RawMessage
	if dec.Decode(&raw) != nil {
		return nil
	}
	b = raw
	return b
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		return ""
	}
	return s
}

// compactionPointFor is C for this request: the user's own setting, read at start-up, when it
// changes C for the request's model; else the bootstrap table. A hosted proxy (Options.Tenants)
// never has a user setting. Fail open: any odd value gives the table.
//
// WHICH REQUESTS. Each client's setting applies only to a request whose User-Agent is that client's
// own (clientOf below; the client sets it itself). A request of another client on the same local
// proxy gets the table. Bob also gets its own verified default when the user set nothing (#963).
// Codex has no default (see compactionpoint.Codex). The limit: the header is the client's own
// claim, and the Claude Agent SDK sends the same User-Agent, so an SDK program run with the same
// env is also covered, which is right, because it reads the same settings. A request with no
// User-Agent gets the table.
//
// MODEL MATCH. A modelSettings key matches by substring on the normalized model id, longest key
// first, the same style as the table in compactionpoint.For. So a key for claude-opus-5 also applies
// to a newer id that contains it (claude-opus-5-1). That is the client's own rule too: its canonical
// model name matches dated and variant spellings.
func (h *Handler) compactionPointFor(r *http.Request, body []byte, window int) compactionpoint.Point {
	model := gjson.GetBytes(body, "model").String()
	if h.opts.Tenants == nil && r != nil {
		switch clientOf(r.UserAgent()) {
		case "claude":
			if p, ok := h.opts.ClaudeCompact.Resolve(model, window); ok {
				return p
			}
		case "bob":
			if p, ok := h.opts.BobCompact.Resolve(window, h.bobWindow(model)); ok {
				return p
			}
		case "codex":
			if p, ok := h.opts.CodexCompact.Resolve(window); ok {
				return p
			}
		}
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	return h.resolveCompactionPoint(ctx, body, window)
}

// clientOf names the client for the compaction point only. It is narrower than dash.AgentFor, which
// matches the substring "bob" or "codex" anywhere and serves the dashboard's filter. A wrong match
// here would give a stranger's request another client's C, so Bob and Codex are matched by the
// start of the User-Agent they send: Bob Shell sends `bobshell/<version>` (bob.js, the request
// headers of its API client); Codex sends its originator first, `codex_cli_rs/...` by default (and
// `codex_exec`, `codex-tui` and the like). Claude Code keeps the dash.AgentFor rule from #955.
func clientOf(userAgent string) string {
	l := strings.ToLower(strings.TrimSpace(userAgent))
	switch {
	case strings.HasPrefix(l, "bobshell/"):
		return "bob"
	case strings.HasPrefix(l, "codex"):
		return "codex"
	}
	switch dash.AgentFor(userAgent) {
	case "claude-code", "claude-cli":
		return "claude"
	}
	return ""
}

// bobWindow is Bob's own max_input_tokens for the model, as seen in the model/info response that
// passed through this proxy (noteBobModelInfo). 0 = not seen. Metadata only: a number for each
// model id, in memory, at most bobWindowMax entries.
func (h *Handler) bobWindow(model string) int {
	h.bobWinMu.Lock()
	defer h.bobWinMu.Unlock()
	return h.bobWin[model]
}

const bobWindowMax = 512

// noteBobModelInfo records data[].model_name -> data[].model_info.max_input_tokens (the LiteLLM
// shape Bob reads in getLiteLLMModels). A body that is not that shape records nothing.
func (h *Handler) noteBobModelInfo(body []byte) {
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return
	}
	got := map[string]int{}
	data.ForEach(func(_, v gjson.Result) bool {
		name := v.Get("model_name").String()
		w := v.Get("model_info.max_input_tokens")
		if name != "" && len(name) <= 128 && w.Type == gjson.Number && w.Int() > 0 && w.Int() < 1<<40 && len(got) < bobWindowMax {
			got[name] = int(w.Int())
		}
		return true
	})
	if len(got) == 0 {
		return
	}
	h.bobWinMu.Lock()
	defer h.bobWinMu.Unlock()
	h.bobWin = got // the whole catalog replaces the old one: bounded, and never stale
}

func isBobModelInfo(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/inference/v1/model/info" || r.URL.Path == "/v1/model/info")
}

// ReadBobCompact reads Bob Shell's model.compressionThreshold (#963) from the user settings file
// ~/.bob/settings.json and the system settings file, which wins in the client's merge. The project
// file is not read (the proxy does not know the directory). Read is true whenever the read ran, so
// Bob's own default (0.5) applies when nothing is set. Same switch as the others: off gives the zero
// value. It never fails.
func ReadBobCompact(getenv func(string) string, home string) compactionpoint.Bob {
	switch strings.ToLower(strings.TrimSpace(getenv(ClaudeCompactOffEnv))) {
	case "off", "0", "false":
		return compactionpoint.Bob{}
	}
	b := compactionpoint.Bob{Read: true}
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, ".bob", "settings.json"))
	}
	paths = append(paths, bobSystemSettingsPath(getenv))
	for _, p := range paths { // later wins: system over user
		if !filepath.IsAbs(p) {
			continue
		}
		if raw := readSmall(p); raw != nil {
			if v := gjson.GetBytes(raw, "model.compressionThreshold"); v.Type == gjson.Number || v.Type == gjson.String {
				b.Threshold = short(v.String())
			}
		}
	}
	return b
}

// bobSystemSettingsPath is the client's own rule (getSystemSettingsPath in bob.js).
func bobSystemSettingsPath(getenv func(string) string) string {
	if p := strings.TrimSpace(getenv("BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH")); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/Bob-Shell/settings.json"
	}
	return "/etc/bob-shell/settings.json"
}

// ReadCodexCompact reads three top-level keys of $CODEX_HOME/config.toml (default ~/.codex) (#963):
// model_auto_compact_token_limit, model_auto_compact_token_limit_scope and
// model_post_turn_compact_threshold_percent. It is a small line scanner, not a TOML parser: it reads
// only `key = value` lines before the first [table] header, and skips anything it does not
// understand (fail open). Same switch as the others.
func ReadCodexCompact(getenv func(string) string, home string) compactionpoint.Codex {
	switch strings.ToLower(strings.TrimSpace(getenv(ClaudeCompactOffEnv))) {
	case "off", "0", "false":
		return compactionpoint.Codex{}
	}
	dir := strings.TrimSpace(getenv("CODEX_HOME"))
	if dir == "" {
		if home == "" {
			return compactionpoint.Codex{}
		}
		dir = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(dir) {
		return compactionpoint.Codex{}
	}
	f, err := os.Open(filepath.Join(dir, "config.toml"))
	if err != nil {
		return compactionpoint.Codex{}
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 4<<20 {
		return compactionpoint.Codex{}
	}
	var c compactionpoint.Codex
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break // the first table header ends the top level
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if i := strings.Index(v, "#"); i >= 0 { // a trailing comment (no setting here holds a #)
			v = v[:i]
		}
		v = short(strings.Trim(strings.TrimSpace(v), `"'`))
		switch strings.TrimSpace(k) {
		case "model_auto_compact_token_limit":
			c.Limit = v
		case "model_auto_compact_token_limit_scope":
			c.Scope = v
		case "model_post_turn_compact_threshold_percent":
			c.PostTurn = v
		}
	}
	return c
}
