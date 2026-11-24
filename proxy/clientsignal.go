package proxy

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/session"
)

// Client lifecycle signals for the keep-alive.
//
// A client that runs Claude Code hooks can tell the proxy things the proxy cannot see in
// the request stream: that the user left (SessionEnd), that the conversation was compacted so its
// old cached prefix is dead (SessionStart source=compact), that the user is back (SessionStart
// resume / clear / startup / fork), that the session is blocked on a human (Notification:
// permission_prompt / elicitation_dialog / idle_prompt). The event carries no content: a session
// id, an event name, a closed set of reason strings and a timestamp.
//
//	event         kind                                         action
//	SessionEnd    clear, resume, logout, prompt_input_exit, ""  retire the session, mark it ended
//	              other
//	SessionStart  compact                                      retire the session (no mark: it goes on)
//	SessionStart  startup, resume, clear, fork                 recorded only
//	Notification  permission_prompt, elicitation_dialog,       recorded only
//	              idle_prompt
//
// "Recorded only" is deliberate, not a placeholder: in offline replay, holding a returning user's
// entry until their first request earned little, and pinging on through a permission prompt lost
// money. Both would also make a signal ADD spend.
//
// # Failure safety
//
// Every signal only ever REMOVES spend: it retires a held entry or lowers a ping budget. None
// extends a hold or arms a ping. A lost, late or forged event therefore costs at most the
// pings the no-signal policy would also have sent, and a missing event is the no-signal policy.
// A signal is also scoped to its own tenant and session (the key is tenant-prefixed by
// session.Scoped), so one tenant cannot silence another's keep-alive.
//
// On whenever the keep-alive runs: CONTEXT_GURU_CLIENT_SIGNALS=off turns the route and the effects off,
// and so does the keep-alive kill switch (CONTEXT_GURU_KEEPALIVE=off), which leaves nothing to retire.
//
// # Wire shapes
//
// The body is either the plugin's own {session, event, kind}, or a Claude Code / Codex hook payload
// posted as it is (session_id, hook_event_name, reason | source | notification_type), which is what a
// one-line `curl --data-binary @-` hook sends. Only those fields are read; every other field of the
// payload (cwd, transcript_path, model) is discarded by the decoder, never logged or stored.

// maxLifecycleBody fits a raw hook payload (its cwd and transcript path are the bulk of it). The
// end marks' lifetime and per-tenant cap are package keepalive's (lifecycle.go).
const maxLifecycleBody = 4096

// compactRaceMargin is keepalive.CompactRaceMargin.
const compactRaceMargin = keepalive.CompactRaceMargin

func clientSignalEnabled() bool {
	return clientSignalsSwitchOn() && !keepAliveDisabled()
}

// clientSignalsSwitchOn is the operator's switch alone (CONTEXT_GURU_CLIENT_SIGNALS off/0/false
// turns every client hook route off), without clientSignalEnabled's keep-alive half. The tool
// deferral routes (/tool-search, /tool-defer/probe) use it: they are not about keep-alive, and must
// still obey the operator.
func clientSignalsSwitchOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_GURU_CLIENT_SIGNALS"))) {
	case "off", "0", "false":
		return false
	}
	return true
}

// clientSessionAlias is the tenant-scoped id a client's hooks name this session by, when the proxy
// keys the session by something else. Codex sends its session id in a session-id header (equal to
// the hook payload's session_id and to prompt_cache_key, verified on codex-cli 0.162.1) but no id
// the session resolver reads, so its requests key by content hash. "" when there is no usable alias.
func clientSessionAlias(tenantID, sessionKey string, r *http.Request) string {
	if r == nil {
		return ""
	}
	sid := strings.TrimSpace(r.Header.Get("session-id"))
	scoped, ok := scopedClientID(tenantID, sid)
	if !ok || scoped == sessionKey {
		return ""
	}
	return scoped
}

// scopedClientID scopes a client-named session id to its tenant, refusing an id Scoped would not use
// as given (it falls back to a content hash on an unusable id).
func scopedClientID(tenantID, sid string) (string, bool) {
	scoped := session.Scoped(tenantID, sid, "", "")
	if sid == "" || (tenantID != "" && scoped != tenantID+":"+sid) || (tenantID == "" && scoped != sid) {
		return "", false
	}
	return scoped, true
}

// lifecycle receives one event. 204 on accept (including "accepted and ignored"), so the client
// cannot tell, and does not need to, whether it changed anything.
func (h *Handler) clientSignal(w http.ResponseWriter, r *http.Request) {
	if !clientSignalEnabled() {
		http.NotFound(w, r)
		return
	}
	// JSON only. A web page can POST text/plain to a loopback proxy without a CORS preflight, and the
	// local proxy takes no credential; application/json from another origin needs a preflight, which
	// this route never answers.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "lifecycle events are application/json", http.StatusUnsupportedMediaType)
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
	var in struct {
		Session string `json:"session"`
		Event   string `json:"event"`
		Kind    string `json:"kind"`
		// The raw hook payload's names for the same three fields.
		SessionID        string `json:"session_id"`
		HookEventName    string `json:"hook_event_name"`
		Reason           string `json:"reason"`
		Source           string `json:"source"`
		NotificationType string `json:"notification_type"`
		PermissionMode   string `json:"permission_mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLifecycleBody)).Decode(&in); err != nil {
		http.Error(w, "bad lifecycle event", http.StatusBadRequest)
		return
	}
	if in.Session == "" && in.Event == "" && in.Kind == "" {
		in.Session, in.Event = in.SessionID, in.HookEventName
		in.Kind = map[string]string{"SessionEnd": in.Reason, "SessionStart": in.Source, "Notification": in.NotificationType}[in.Event]
	}
	// The closed sets of events and kinds, and the "other" reading of a new SessionEnd reason, are keepalive.NormalizeSignal.
	kind, kindOK := keepalive.NormalizeSignal(in.Event, in.Kind)
	in.Kind = kind
	scoped, ok := scopedClientID(tn.ID, strings.TrimSpace(in.Session))
	if !ok || !kindOK {
		http.Error(w, "bad lifecycle event", http.StatusBadRequest)
		return
	}
	now := h.keeper.now()
	h.keeper.signal(tn.ID, scoped, in.Event, in.Kind, now)
	// Kept in the dashboard database too (#1198), with the dashboard's own switch: enums and ids only.
	// A Notification that raises a prompt also marks the request it interrupted.
	h.rec.RecordSignal(dash.ClientSignal{TS: now.UnixMilli(), TenantID: tn.ID, Session: scoped, Event: in.Event, Reason: in.Kind, PermissionMode: permissionModeOf(in.PermissionMode)})
	w.WriteHeader(http.StatusNoContent)
}

// signal applies one event. Returns whether it changed anything. What each event does is
// keepalive.Keeper.ApplyClientSignal; the host tells the campaign and counts it.
func (k *keeper) signal(tenantID, scoped, event, kind string, now time.Time) bool {
	if k == nil {
		return false
	}
	res := k.Keeper.ApplyClientSignal(tenantID, scoped, event, kind, now, func(r keepalive.SignalResult) {
		// The hook's id is the campaign's key: campaign decisions are never made on the OpenAI route (keeper.record), and Codex, the one client
		// whose hook id differs from the proxy's session key, always uses that route.
		k.pred.sessionEnded(tenantID, scoped, now, r.Cutoff)
	})
	slog.Debug("cg.client_signal", "event", event, "kind", kind, "session", scoped, "retired", res.Retired, "applied", res.Applied)
	if res.Applied {
		k.signalApplied.Add(1)
	}
	return res.Applied
}

// toolSearchSignal receives the raw ENABLE_TOOL_SEARCH value of one session, from the plugin's
// session-start hook (#485). A value that turns search off (tooldefer.SearchOffValue) turns tool
// deferral off for that session. Its own route, because /signal is off with keep-alive and this flag
// is not about keep-alive. The same checks as /signal: the operator's CONTEXT_GURU_CLIENT_SIGNALS
// switch, JSON only, the same authentication, the same tenant-scoped session id. 204 on accept.
func (h *Handler) toolSearchSignal(w http.ResponseWriter, r *http.Request) {
	if !clientSignalsSwitchOn() {
		http.NotFound(w, r)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "tool-search signals are application/json", http.StatusUnsupportedMediaType)
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
	var in struct {
		Session          string `json:"session"`
		EnableToolSearch string `json:"enable_tool_search"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLifecycleBody)).Decode(&in); err != nil ||
		len(in.EnableToolSearch) > 16 {
		http.Error(w, "bad tool-search signal", http.StatusBadRequest)
		return
	}
	scoped, ok := scopedClientID(tn.ID, strings.TrimSpace(in.Session))
	if !ok {
		http.Error(w, "bad tool-search signal", http.StatusBadRequest)
		return
	}
	h.noteToolSearch(tn, scoped, in.EnableToolSearch)
	w.WriteHeader(http.StatusNoContent)
}

// toolDeferProbe is the preset picker's call when the user picks ultrahigh (#485): it asks for the
// provider check of tool deferral to start at this account's next request, so the probe runs at once
// instead of waiting for deferral to need it. It returns 202 at once and holds no credential: the
// probe runs in the background through the prober, with that next request's own upstream, model and
// credential. The same checks as /signal, the operator's switch included.
func (h *Handler) toolDeferProbe(w http.ResponseWriter, r *http.Request) {
	if !clientSignalsSwitchOn() {
		http.NotFound(w, r)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "tool-defer probe asks are application/json", http.StatusUnsupportedMediaType)
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
	h.askProbe(tn.ID)
	w.WriteHeader(http.StatusAccepted)
}

// permissionModeOf keeps the client's permission mode only from a closed set. A value a newer client
// adds reads "other"; no value reads "".
func permissionModeOf(v string) string {
	switch v {
	case "":
		return ""
	case "default", "acceptEdits", "plan", "bypassPermissions", "dontAsk":
		return v
	}
	return "other"
}
