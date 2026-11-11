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
// session and deletes what the proxy already wrote for it. A client (a deployment's Claude Code
// plugin, for example a /no-record command or a session-start hook) sends the id Claude Code
// gives the session, which is the id the recorder names the session's file by.
//
// It can only narrow consent: it never starts a recording. The same principal as the Ping
// button (adaptPrincipal), but for the caller's OWN account only: a ?tenant= override other
// than "me" is refused. Calls are rate-limited per account, like the other writes on this
// principal.
//
// The answer is 200 {"recording": <the account is recorded>, "deleted": <a file was deleted>}.
// "recording" is the account's consent, not only "a recorder exists", so a client never tells a
// person that a recording was stopped when there was none.
func (h *Handler) mountRecordOptOut(m *http.ServeMux) {
	m.HandleFunc("POST /api/me/record/off", h.recordOff)
}

type recordOffBody struct {
	Session string `json:"session"`
}

func (h *Handler) recordOff(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query(); q.Has("tenant") && q.Get("tenant") != "me" {
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
	// The recorder's own rules (newRecCtx, recordFor): a tenant subdirectory and the account's own
	// consent in hosted mode; on a single-tenant proxy the operator's --record is the consent.
	tenantID, consent := "", true
	if h.opts.Tenants != nil {
		tenantID = who.TenantID
		consent = who.actor != nil && who.actor.RecordTraces
	}
	if h.recorder == nil {
		writeJSON(w, http.StatusOK, map[string]any{"recording": false, "deleted": false})
		return
	}
	if !h.adaptAllow("record-off:" + tenantID) {
		code, msg := statusOf(errAdaptRate)
		ctlErr(w, code, msg)
		return
	}
	// An account that is not recorded has nothing to stop. Write nothing for it, so a caller cannot
	// fill the record directory with markers for ids that were never recorded. A file that exists
	// (the account opted out of recording after this session began) is still deleted.
	if !consent && !h.recorder.HasSession(tenantID, id) {
		writeJSON(w, http.StatusOK, map[string]any{"recording": false, "deleted": false})
		return
	}
	stopped, deleted, err := h.recorder.Decline(tenantID, id)
	if err != nil {
		slog.Warn("cg.record: could not stop recording a session", "session", id, "stopped", stopped, "err", err)
		msg := "could not stop recording this session"
		if stopped {
			msg = "recording of this session is stopped, but what was recorded could not be deleted"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": msg, "stopped": stopped})
		return
	}
	slog.Info("cg.record: session opted out", "session", id, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"recording": consent, "deleted": deleted})
}
