package proxy

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

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

// compactRaceMargin bounds how late a SessionStart compact event may arrive after the request that
// follows the compaction started. Measured hook-to-proxy delivery is 0.2-0.8 s; the old prefix's last
// request (the compaction call itself) started tens of seconds earlier. Known limitation: a ping also
// moves startedAt, so an old-prefix entry pinged within the margin is not retired by the event and is
// pinged at most once more before its next idle check stops it.
const compactRaceMargin = 2 * time.Second

func clientSignalEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_GURU_CLIENT_SIGNALS"))) {
	case "off", "0", "false":
		return false
	}
	return !keepAliveDisabled()
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

// lifecycleEvents is the closed set of accepted events; anything else is refused, so a client
// cannot smuggle a free-text field through this route.
var lifecycleEvents = map[string]bool{"SessionEnd": true, "SessionStart": true, "Notification": true}

// lifecycleKinds is the closed set of accepted reason / notification types.
var lifecycleKinds = map[string]bool{"": true, "clear": true, "resume": true, "logout": true, "prompt_input_exit": true, "other": true,
	"startup": true, "compact": true, "fork": true, "permission_prompt": true, "elicitation_dialog": true, "idle_prompt": true}

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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLifecycleBody)).Decode(&in); err != nil {
		http.Error(w, "bad lifecycle event", http.StatusBadRequest)
		return
	}
	if in.Session == "" && in.Event == "" && in.Kind == "" {
		in.Session, in.Event = in.SessionID, in.HookEventName
		in.Kind = map[string]string{"SessionEnd": in.Reason, "SessionStart": in.Source, "Notification": in.NotificationType}[in.Event]
	}
	// A SessionEnd reason a newer client adds still means the session ended; read it as "other"
	// rather than refuse the event and silently lose the feature.
	if in.Event == "SessionEnd" && !lifecycleKinds[in.Kind] {
		in.Kind = "other"
	}
	scoped, ok := scopedClientID(tn.ID, strings.TrimSpace(in.Session))
	if !ok || !lifecycleEvents[in.Event] || !lifecycleKinds[in.Kind] {
		http.Error(w, "bad lifecycle event", http.StatusBadRequest)
		return
	}
	h.keeper.signal(tn.ID, scoped, in.Event, in.Kind, h.keeper.now())
	w.WriteHeader(http.StatusNoContent)
}

// signal applies one event. Returns whether it changed anything.
func (k *keeper) signal(tenantID, scoped, event, kind string, now time.Time) bool {
	if k == nil {
		return false
	}
	switch event {
	case "SessionEnd":
		// Every end reason (even clear / resume) retires the id the event names: that id will not
		// send its next request from this process. If it does (a resumed session) the request
		// starts after the mark and is tracked normally; see keepalive.Keeper.EndedAfter.
		before := k.Stats().Live
		k.retireSession(tenantID, scoped)
		// The hook's id is the campaign's key: campaign decisions are never made on the OpenAI route (keeper.record), and Codex, the one client
		// whose hook id differs from the proxy's session key, always uses that route.
		k.pred.sessionEnded(tenantID, scoped, now, time.Time{})
		slog.Debug("cg.client_signal", "event", event, "kind", kind, "session", scoped, "retired", before-k.Stats().Live)
		k.markEnded(tenantID, scoped, now)
		k.signalApplied.Add(1)
		return true
	case "SessionStart":
		if kind != "compact" {
			break
		}
		// Compaction replaced the conversation: every entry held for this session caches the old
		// prefix, which no later request will send. The session itself goes on, so there is no end
		// mark, and the post-compaction request is tracked as usual. The hook sends in the background,
		// so the event can arrive after that request started (even after it was recorded): only
		// entries whose last request started compactRaceMargin before the arrival are the old prefix.
		before := k.Stats().Live
		k.retireSessionStartedBefore(tenantID, scoped, now.Add(-compactRaceMargin))
		k.pred.sessionEnded(tenantID, scoped, now, now.Add(-compactRaceMargin))
		slog.Debug("cg.client_signal", "event", event, "kind", kind, "session", scoped, "retired", before-k.Stats().Live)
		k.signalApplied.Add(1)
		return true
	}
	// Everything else is accepted and recorded, never acted on. Notification: offline, the pings
	// sent on tool-wait rows that outlast 60 s (the nearest proxy for "blocked on a human") net
	// positive, so a rule that stops them would lose money. SessionStart presence: see the table above.
	slog.Debug("cg.client_signal", "event", event, "kind", kind, "session", scoped, "applied", false)
	return false
}
