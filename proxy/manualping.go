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
// is a distinct 409 "not held" rather than a ping that cannot be sent. A thread the keeper does not hold at
// all answers 404 "unknown session" with no dashboard, and a hosted proxy with no recorder answers 404 too.

// ManualHoldHeader is how a client opts a request in to being held for the Ping button. Claude
// Code sends it from ANTHROPIC_CUSTOM_HEADERS (the plugin writes it into settings env); it is an
// x-context-guru-* header, so copyHeaders never forwards it upstream.
const ManualHoldHeader = "x-context-guru-ping"

// wantsManualHold reads the opt-in. Only the exact value "1" counts: a header present with another
// value is a client that did not mean it.
func wantsManualHold(r *http.Request) bool {
	return r != nil && strings.TrimSpace(r.Header.Get(ManualHoldHeader)) == "1"
}

// manualPingMinGap is keepalive.ManualPingMinGap.
const manualPingMinGap = keepalive.ManualPingMinGap

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
	// A hosted proxy with no audit sink holds nothing, so there is nothing to ping (#1457). A
	// single-tenant proxy needs no dashboard: the keeper's own facts stand in for the thread table.
	if h.keeper == nil || !h.keeper.mayHold() {
		ctlErr(w, http.StatusNotFound, "unknown session")
		return
	}
	// The caller's OWN account, always. adaptPrincipal lets a manager name another account with
	// ?tenant=; a ping spends that account's key, so the query parameter is ignored here.
	tenantID := who.TenantID
	if who.actor != nil {
		tenantID = who.actor.ID
	}
	scoped := scopedSession(tenantID, in.Session)

	// What the dashboard knows about that thread's cache: when it last was touched, the tier it
	// was written at, how big the prefix is. A thread with no cache touch is unknown. With no
	// dashboard (single-tenant only) the keeper's own facts give the same two numbers.
	now := h.keeper.now()
	var lastTouch time.Time
	var ttl time.Duration
	if h.rec != nil {
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
		lastTouch, ttl = time.UnixMilli(th.EndTS), time.Duration(th.TTLSeconds)*time.Second
	} else {
		var ok bool
		lastTouch, ttl, ok = h.keeper.Keeper.WarmFacts(keepalive.Key{Tenant: tenantID, Session: scoped, Thread: thread})
		if !ok {
			ctlErr(w, http.StatusNotFound, "unknown session")
			return
		}
	}
	// The cold gate and the 1-hour touch conflict are keepalive.ManualPingBlock.
	switch keepalive.ManualPingBlock(
		h.keeper.Keeper.OneHourTouchLive(keepalive.Key{Tenant: tenantID, Session: scoped, Thread: thread}),
		lastTouch, ttl, now) {
	case keepalive.ManualBlockAtOneHour:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "at one hour",
			"detail": "the cache entry is at 1 hour after a free 1-hour ping; a manual ping would move it back to 5 minutes"})
		return
	case keepalive.ManualBlockCold:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "cold"})
		return
	}

	res, err := h.keeper.PingNow(keepalive.ManualRequest{
		Key:    keepalive.Key{Tenant: tenantID, Session: scoped, Thread: thread},
		MinGap: manualPingMinGap,
	})
	var soon *keepalive.TooSoonError
	var up *keepalive.UpstreamError
	switch {
	case errors.As(err, &soon):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too soon",
			"retry_after_s": int(soon.RetryAfter.Seconds()) + 1})
		return
	case errors.Is(err, keepalive.ErrNotHeld):
		// The reason is the keeper's own, read now: a press that raced the release of a thread's copy
		// must not make the client guess it from a cache-state copy that is up to 15 s old.
		body := map[string]any{"error": "not held",
			"detail": "the proxy keeps nothing to ping this thread with: keep-alive is off for this account, or it stopped pinging this thread"}
		if st := h.keeper.Keeper.PingStates(tenantID, false, scoped).For(thread); st.Reason != "" {
			body["ping_reason"] = st.Reason
		}
		writeJSON(w, http.StatusConflict, body)
		return
	case errors.Is(err, keepalive.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "busy",
			"detail": "a request is in flight on this thread, and it refreshes the cache itself"})
		return
	case errors.Is(err, keepalive.ErrTooCostly):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "over budget",
			"detail": "one ping on this thread would cost more than the operator's per-ping cap, or more than the miss it protects"})
		return
	case errors.Is(err, keepalive.ErrAtOneHour):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "at one hour",
			"detail": "the cache entry is at 1 hour after a free 1-hour ping; a manual ping would move it back to 5 minutes"})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"sent":        true,
		"cache_read":  res.Usage.CacheRead,
		"cache_write": res.Usage.CacheWrite,
		"ping_usd":    res.CostUSD,
		// The saving is credited by the accountant on the thread's next real request, so the answer says it is
		// pending and carries no estimate (#1460). An older plugin reads a missing saved_usd as no saving.
		"saving":        "pending",
		"warm_until_ms": res.StartedAt.Add(ttl).UnixMilli(),
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
