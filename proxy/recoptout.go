package proxy

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossoctl/context-guru/session"
)

// POST /api/me/record/off {"session": "<Claude Code session id>"}: the per-session opt-out.
//
// An account that opted in to recording is recorded in every session. This stops recording ONE
// session and deletes what the proxy already wrote for it. Two clients call it: the plugin's
// /cg:no-record command, at any time in a session, and its session-start hook when the user
// set CONTEXT_GURU_RECORD=off. Both send the id Claude Code gives the session, which is the id
// the recorder names the session's file by.
//
// It can only narrow consent: it never starts a recording. The same principal as the Ping
// button (adaptPrincipal), but for the caller's OWN account only: the ?tenant= override a
// manager has elsewhere is refused here. It answers 200 whether or not anything was recorded,
// so the client can say plainly what happened.
func (h *Handler) mountRecordOptOut(m *http.ServeMux) {
	m.HandleFunc("POST /api/me/record/off", h.recordOff)
}

type recordOffBody struct {
	Session string `json:"session"`
}

func (h *Handler) recordOff(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("tenant") {
		ctlErr(w, http.StatusBadRequest, "a session opts out for its own account only")
		return
	}
	who, err := h.adaptPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in recordOffBody
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	id := strings.TrimSpace(in.Session)
	// The recorder keys a file on session.ExplicitID(metadata.user_id); the same rule here means
	// the id names the same file, and an id it would reject (a path, "..") is refused.
	if id == "" || session.ExplicitID(id) != id {
		ctlErr(w, http.StatusBadRequest, "name the session to stop recording")
		return
	}
	tenantID := ""
	if h.opts.Tenants != nil { // the recorder's own rule (newRecCtx): a tenant subdirectory in hosted mode only
		tenantID = who.TenantID
	}
	if h.recorder == nil {
		writeJSON(w, http.StatusOK, map[string]any{"recording": false, "deleted": false})
		return
	}
	deleted, err := h.recorder.Decline(tenantID, id)
	if err != nil {
		slog.Warn("cg.record: could not stop recording a session", "session", id, "err", err)
		ctlErr(w, http.StatusInternalServerError, "could not stop recording this session")
		return
	}
	slog.Info("cg.record: session opted out", "session", id, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"recording": true, "deleted": deleted})
}
