package proxy

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
)

// The user's own Claude Code auto-compact settings (#948).
//
// WHY A SIGNAL PER SESSION and not a header per request. The plugin's session-start hook reads the
// settings once, where Claude Code runs, and posts them here with the session id, the way it posts
// ENABLE_TOOL_SEARCH to /tool-search. A header on every request would need the plugin to put the
// values into ANTHROPIC_CUSTOM_HEADERS at install time. That variable is fixed at the start of the
// session and is shared with the user's own headers, so a setting changed with /autocompact would
// never reach us. The hook runs at every session start (startup, resume, fork, clear), so it sees
// the setting that session will use.
//
// WHERE IT LIVES: the session's own tenant store, under store.CompactionPointPrefix, like the tool
// search opt-out. It is a few short strings (metadata), so the disk store keeps it at every storage
// level. With storage off (the Nop store) nothing is kept and C stays at the table value.
//
// A session the hook never reported (no plugin, another client, a request with no session id of its
// own) has no record, and C is the table value: the behavior before #948.

// maxCompactionCfgField bounds each reported value. The longest real one is "1000000" or "1m".
const maxCompactionCfgField = 32

// compactionCfg is the wire and stored shape: the raw text of each setting, so the parsing rules
// live in one place (internal/compactionpoint.ClaudeCode).
type compactionCfg struct {
	Session         string            `json:"session,omitempty"`
	Harness         string            `json:"harness"`
	EnvWindow       string            `json:"env_window,omitempty"`
	EnvPct          string            `json:"env_pct,omitempty"`
	EnvDisableAuto  string            `json:"env_disable_auto,omitempty"`
	EnvDisable      string            `json:"env_disable,omitempty"`
	SettingsWindow  string            `json:"settings_window,omitempty"`
	SettingsEnabled string            `json:"settings_enabled,omitempty"`
	ModelWindows    map[string]string `json:"model_windows,omitempty"`
}

func (c compactionCfg) valid() bool {
	if c.Harness != "claude-code" {
		return false // only Claude Code is verified; see compactionpoint.ClaudeCode
	}
	for _, v := range []string{c.EnvWindow, c.EnvPct, c.EnvDisableAuto, c.EnvDisable, c.SettingsWindow, c.SettingsEnabled} {
		if len(v) > maxCompactionCfgField {
			return false
		}
	}
	if len(c.ModelWindows) > 16 {
		return false
	}
	for k, v := range c.ModelWindows {
		if len(k) > 64 || len(v) > maxCompactionCfgField {
			return false
		}
	}
	return true
}

func (c compactionCfg) claudeCode() compactionpoint.ClaudeCode {
	return compactionpoint.ClaudeCode{
		EnvWindow: c.EnvWindow, EnvPct: c.EnvPct, EnvDisableAuto: c.EnvDisableAuto, EnvDisable: c.EnvDisable,
		SettingsWindow: c.SettingsWindow, SettingsEnabled: c.SettingsEnabled, ModelWindows: c.ModelWindows,
	}
}

// compactionConfigSignal receives one session's Claude Code auto-compact settings from the
// plugin's session-start hook. The same checks as /tool-search: the operator's
// CONTEXT_GURU_CLIENT_SIGNALS switch, JSON only, the same authentication, the same tenant-scoped
// session id. 204 on accept.
func (h *Handler) compactionConfigSignal(w http.ResponseWriter, r *http.Request) {
	if !clientSignalsSwitchOn() {
		http.NotFound(w, r)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "compaction-config signals are application/json", http.StatusUnsupportedMediaType)
		return
	}
	tn, err := h.authenticate(r)
	if err != nil {
		h.refuse(w, r, err)
		return
	}
	release, ok := h.meter(w, r, tn)
	defer release()
	if !ok {
		return
	}
	var in compactionCfg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLifecycleBody)).Decode(&in); err != nil || !in.valid() {
		http.Error(w, "bad compaction-config signal", http.StatusBadRequest)
		return
	}
	scoped, ok := scopedClientID(tn.ID, strings.TrimSpace(in.Session))
	if !ok {
		http.Error(w, "bad compaction-config signal", http.StatusBadRequest)
		return
	}
	in.Session = ""
	if b, err := json.Marshal(in); err == nil && tn.Store != nil {
		tn.Store.Put(store.CompactionPointPrefix+scoped, b)
	}
	w.WriteHeader(http.StatusNoContent)
}

// compactionPointFor is C for this request: the user's own setting when the session's hook
// reported one that changes it, else the bootstrap table. Fail open: no record, an unreadable
// record or an odd value all give the table value, which is what the proxy did before #948.
func (h *Handler) compactionPointFor(tn *Tenancy, r *http.Request, body []byte, window int) compactionpoint.Point {
	model := gjson.GetBytes(body, "model").String()
	table := compactionpoint.For(model)
	if tn == nil || tn.Store == nil {
		return table
	}
	explicit := apply.ExplicitSession(r.Header.Get("x-context-guru-session"), body)
	if explicit == "" {
		return table // a session with no id of its own cannot be matched to the hook's report
	}
	scoped := session.Scoped(tn.ID, explicit, "", "")
	if l, ok := tn.Store.(store.SessionLoader); ok {
		l.LoadSession(scoped) // the disk copy after a restart; a no-op once loaded
	}
	raw, ok := tn.Store.Get(store.CompactionPointPrefix + scoped)
	if !ok {
		return table
	}
	var cfg compactionCfg
	if json.Unmarshal(raw, &cfg) != nil || !cfg.valid() {
		return table
	}
	if p, ok := cfg.claudeCode().Resolve(model, window); ok {
		return p
	}
	return table
}
