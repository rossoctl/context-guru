package compactionpoint

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/tidwall/gjson"
)

// This file holds the choice of the compaction point for one request, and the readers of each
// client's own settings. It moved out of proxy/compactioncfg.go (#1300), so that every host gets
// the same C for the same client. A host passes only what it alone can know: the env (getenv),
// the home directory, an opener for files (Open), and its runtime.GOOS (goos). The library finds
// Bob's system-settings path itself.

// OffEnv turns the start-up read off ("off", "0" or "false"). C then comes from the table.
const OffEnv = "CONTEXT_GURU_CLIENT_COMPACT_CONFIG"

func readOff(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(OffEnv))) {
	case "off", "0", "false":
		return true
	}
	return false
}

// Client names a client for the choice of C.
type Client string

const (
	ClientNone   Client = ""
	ClientClaude Client = "claude"
	ClientBob    Client = "bob"
	ClientCodex  Client = "codex"
)

// ClientOf names the client for the compaction point only. A wrong match would give a stranger's
// request another client's C, so Bob and Codex are matched by the start of the User-Agent they
// send: Bob Shell sends `bobshell/<version>`; Codex sends its originator first, `codex_cli_rs/...`
// by default (and `codex_exec`, `codex-tui` and the like). Claude Code is matched by the substring
// "claude-code" or "claude-cli" anywhere in the User-Agent (#955). That is the rule of dash.AgentFor
// for those two names. The dash package imports this one, so the rule is repeated here; a test in
// this package pins both to the same answers.
func ClientOf(userAgent string) Client {
	l := strings.ToLower(strings.TrimSpace(userAgent))
	switch {
	case strings.HasPrefix(l, "bobshell/"):
		return ClientBob
	case strings.HasPrefix(l, "codex"):
		return ClientCodex
	}
	// dash.AgentFor lowercases the raw User-Agent (no trim) and tests claude-code, then claude-cli.
	raw := strings.ToLower(userAgent)
	if strings.Contains(raw, "claude-code") || strings.Contains(raw, "claude-cli") {
		return ClientClaude
	}
	return ClientNone
}

// Settings holds each client's own settings, read once at start-up in local mode. The zero value
// changes nothing.
type Settings struct {
	Claude ClaudeCode
	Bob    Bob
	Codex  Codex
}

// Pick is C for a request from this User-Agent, from the user's own setting for that client, when
// it changes C. ok is false when the caller must use the operator's price table instead. A hosted
// proxy must not call Pick: it never has a user setting. bobWindow gives Bob's own window for the
// model (0 = not seen). It runs only for a Bob request.
func (s Settings) Pick(userAgent, model string, window int, bobWindow func(model string) int) (Point, bool) {
	switch ClientOf(userAgent) {
	case ClientClaude:
		return s.Claude.Resolve(model, window)
	case ClientBob:
		bw := 0
		if bobWindow != nil {
			bw = bobWindow(model)
		}
		return s.Bob.Resolve(window, bw)
	case ClientCodex:
		return s.Codex.Resolve(window)
	}
	return Point{}, false
}

// CleanBobWindows returns the catalog to store, or nil when nothing should be stored. An empty
// catalog, or one over max entries, stores nothing. The host passes the agent's own cap
// (bob.MaxModelWindows), so the cap has one definition. A window <= 0 is dropped. The result
// is a copy: the caller keeps no handle on the agent's map.
func CleanBobWindows(windows map[string]int, max int) map[string]int {
	if len(windows) == 0 || len(windows) > max {
		return nil
	}
	got := make(map[string]int, len(windows))
	for m, w := range windows {
		if w > 0 {
			got[m] = w
		}
	}
	if len(got) == 0 {
		return nil
	}
	return got
}

// short trims a raw setting and drops a value over 32 bytes.
func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		return ""
	}
	return s
}

// Open opens a file by the OS path the library names. A host passes a function that calls os.Open,
// so each path means what it meant before: a relative path is relative to the working directory, a
// `.` or `..` element works, and a Windows drive path works. A test passes an in-memory one.
type Open func(path string) (fs.File, error)

// readSmall reads a settings file of at most 4 MiB, or nil. A missing file is the normal case.
func readSmall(open Open, path string) []byte {
	f, err := open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 4<<20 {
		return nil
	}
	var raw json.RawMessage
	if json.NewDecoder(f).Decode(&raw) != nil {
		return nil
	}
	return raw
}

// ReadClaude reads Claude Code's auto-compact settings from env and the user's settings files (#948).
// getenv, home and open are injected. It never fails: an unreadable or odd file is skipped (fail
// open). Project settings files depend on the directory the client runs in, so they are not read.
func ReadClaude(getenv func(string) string, home string, open Open) ClaudeCode {
	if readOff(getenv) {
		return ClaudeCode{}
	}
	c := ClaudeCode{
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
	if b := readSmall(open, globalCfg); b != nil {
		if v := gjson.GetBytes(b, "autoCompactEnabled"); v.Type == gjson.True || v.Type == gjson.False {
			c.SettingsEnabled = fmt.Sprint(v.Bool())
		}
	}
	if b := readSmall(open, settings); b != nil {
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

// ReadBob reads Bob Shell's model.compressionThreshold (#963) from the user settings file
// ~/.bob/settings.json and the system settings file (BobSystemSettingsPath), which wins in the
// client's merge. goos is the host's runtime.GOOS. The project file is not read.
// Read is true whenever the read ran, so Bob's own default (0.5) applies when nothing is set. Same
// off switch as the others. It never fails.
func ReadBob(getenv func(string) string, home, goos string, open Open) Bob {
	if readOff(getenv) {
		return Bob{}
	}
	b := Bob{Read: true}
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, ".bob", "settings.json"))
	}
	paths = append(paths, BobSystemSettingsPath(getenv, goos))
	for _, p := range paths { // later wins: system over user
		if !filepath.IsAbs(p) {
			continue
		}
		if raw := readSmall(open, p); raw != nil {
			if v := gjson.GetBytes(raw, "model.compressionThreshold"); v.Type == gjson.Number || v.Type == gjson.String {
				b.Threshold = short(v.String())
			}
		}
	}
	return b
}

// BobSystemSettingsPath is the client's own rule (getSystemSettingsPath in bob.js). goos is
// runtime.GOOS of the host.
func BobSystemSettingsPath(getenv func(string) string, goos string) string {
	if p := strings.TrimSpace(getenv("BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH")); p != "" {
		return p
	}
	if goos == "darwin" {
		return "/Library/Application Support/Bob-Shell/settings.json"
	}
	return "/etc/bob-shell/settings.json"
}

// ReadCodex reads three top-level keys of $CODEX_HOME/config.toml (default ~/.codex) (#963):
// model_auto_compact_token_limit, model_auto_compact_token_limit_scope and
// model_post_turn_compact_threshold_percent. It is a small line scanner, not a TOML parser: it reads
// only `key = value` lines before the first [table] header, and skips anything it does not
// understand (fail open). Same off switch as the others.
func ReadCodex(getenv func(string) string, home string, open Open) Codex {
	if readOff(getenv) {
		return Codex{}
	}
	dir := strings.TrimSpace(getenv("CODEX_HOME"))
	if dir == "" {
		if home == "" {
			return Codex{}
		}
		dir = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(dir) {
		return Codex{}
	}
	f, err := open(filepath.Join(dir, "config.toml"))
	if err != nil {
		return Codex{}
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 4<<20 {
		return Codex{}
	}
	var c Codex
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
