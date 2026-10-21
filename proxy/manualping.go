package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/session"
)

// POST /api/me/cache/ping: the Ping button. It sends ONE keep-alive ping on one of the caller's
// own threads right now, through the keeper's ordinary ping path, so the cache stays warm.
//
// # Who may call
//
// The shared /api/adapt/* principal (adaptapply.go): the dashboard cookie, the account's own
// proxy token, or a direct loopback caller on a single-tenant proxy. Not ctlTenant, because
// MountControl mounts nothing without a tenant registry and this feature is mostly used on a
// local single-tenant proxy; the plugin also has a token and no browser. The caller can only
// ever reach a session of its OWN tenant: the id is scoped to the authenticated tenant before
// anything is looked up, so another tenant's id addresses nothing and answers 404.
//
// # What it does not do
//
// It does not widen what the keeper may hold. The keeper keeps a thread's body and credential
// only while keep-alive is on for the account, and pinging needs both, so with it off the answer
// is a distinct 409 "not held" rather than a ping that cannot be sent.

// manualPingMinGap is the least time between two manual pings of the same thread.
const manualPingMinGap = 30 * time.Second

func (h *Handler) mountManualPing(m *http.ServeMux) {
	m.HandleFunc("POST /api/me/cache/ping", h.manualPing)
}

type manualPingBody struct {
	Session string `json:"session"`
	Thread  string `json:"thread"`
}

func (h *Handler) manualPing(w http.ResponseWriter, r *http.Request) {
	who, err := h.adaptPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in manualPingBody
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	in.Session = strings.TrimSpace(in.Session)
	if in.Session == "" || len(in.Session) > maxSessionIDBytes {
		ctlErr(w, http.StatusBadRequest, "name the session to ping")
		return
	}
	// "main" is the contract's name for the primary thread; the store's id for it is "".
	thread := in.Thread
	if thread == "main" {
		thread = ""
	}
	if h.rec == nil || h.keeper == nil {
		ctlErr(w, http.StatusNotFound, "unknown session")
		return
	}
	tenantID := who.TenantID
	scoped := scopedSession(tenantID, in.Session)

	// What the dashboard knows about that thread's cache: when it last was touched, the tier it
	// was written at, how big the prefix is. A thread with no cache touch is unknown.
	now := h.keeper.now()
	f := dash.Filter{Tenant: tenantID, Session: scoped}
	ths, _, err := h.rec.DB().ThreadsFor(f, now.Add(-2*time.Hour).UnixMilli())
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not read the session's cache state")
		return
	}
	var th *dash.ThreadCache
	for i := range ths {
		if ths[i].ID == thread {
			th = &ths[i]
		}
	}
	if th == nil {
		ctlErr(w, http.StatusNotFound, "unknown session")
		return
	}
	if !now.Before(time.UnixMilli(th.EndTS).Add(time.Duration(th.TTLSeconds) * time.Second)) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "cold"})
		return
	}

	// The cache write this ping avoids, priced from the SAME prefix and tier /api/cachestate
	// shows as next_cold_usd.
	var rewrite float64
	if p := h.opts.Prices; p != nil && th.Model != "" {
		if price, ok := p.Price(r.Context(), th.Model); ok && !price.Zero() {
			_, rewrite = dash.NextRequestUSD(price, th.PrefixTokens, th.TTLSeconds)
		}
	}

	res, err := h.keeper.PingNow(keepalive.ManualRequest{
		Key:        keepalive.Key{Tenant: tenantID, Session: scoped, Thread: thread},
		MinGap:     manualPingMinGap,
		RewriteUSD: rewrite,
	})
	var soon *keepalive.TooSoonError
	var up *keepalive.UpstreamError
	switch {
	case errors.As(err, &soon):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too soon",
			"retry_after_s": int(soon.RetryAfter.Seconds()) + 1})
		return
	case errors.Is(err, keepalive.ErrNotHeld):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not held",
			"detail": "the proxy keeps nothing to ping this thread with: keep-alive is off for this account, or it stopped pinging this thread"})
		return
	case errors.Is(err, keepalive.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "busy",
			"detail": "a request is in flight on this thread, and it refreshes the cache itself"})
		return
	case errors.Is(err, keepalive.ErrLimited):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "no spare capacity", "retry_after_s": 5})
		return
	case errors.As(err, &up):
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream", "status": up.Status})
		return
	case err != nil:
		slog.Debug("context-guru: manual ping failed", "tenant", tenantID, "session", scoped, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream"})
		return
	}
	saved := rewrite - res.CostUSD
	if saved < 0 || res.AvoidedUSD == 0 {
		saved = 0 // a ping that read nothing, or cost more than the write, kept nothing warm
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sent":          true,
		"cache_read":    res.Usage.CacheRead,
		"cache_write":   res.Usage.CacheWrite,
		"ping_usd":      res.CostUSD,
		"rewrite_usd":   rewrite,
		"saved_usd":     saved,
		"warm_until_ms": res.StartedAt.Add(time.Duration(th.TTLSeconds) * time.Second).UnixMilli(),
	})
}

// scopedSession is the session key as the proxy stores it for this tenant. An id already in the
// tenant's own namespace (what /api/cachestate and the dashboard show in hosted mode) is kept; a
// bare client id is scoped, exactly as the request path does (session.Scoped).
func scopedSession(tenantID, id string) string {
	if tenantID != "" && strings.HasPrefix(id, tenantID+":") {
		return id
	}
	return session.Scoped(tenantID, id, "", "")
}
