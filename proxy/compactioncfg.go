package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
// WHICH REQUESTS. The setting is Claude Code's, so it applies only to a request whose User-Agent is
// Claude Code's (dash.AgentFor: "claude-code" or "claude-cli"; the client sets it itself). A Codex or
// Bob request on the same local proxy gets the table. The limit: the header is the client's own
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
	if h.opts.Tenants == nil && r != nil && isClaudeCodeAgent(r.UserAgent()) {
		if p, ok := h.opts.ClaudeCompact.Resolve(model, window); ok {
			return p
		}
	}
	return compactionpoint.For(model)
}

func isClaudeCodeAgent(userAgent string) bool {
	switch dash.AgentFor(userAgent) {
	case "claude-code", "claude-cli":
		return true
	}
	return false
}
