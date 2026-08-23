package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tenant"
)

// The keep-alive's WRITE routes: arm a session, disarm one, and the account-wide off switch.
//
// Here rather than in dash because this is where the keeper and the web principal live, and
// because they share PUT /api/me's authentication and CSRF path exactly — no new auth code, and
// the same table-driven scope enforcement (ctlRoutes / gate / TestEveryControlRouteEnforcesItsScope).
//
// # The consent asymmetry, and why it is deliberate
//
// Turning the mechanism ON keeps the manager gate, because it is stored in the configuration
// document and that document is manager-owned. Turning it OFF is open to ANY principal on their
// own account. Consent withdrawal must never be harder than consent, and the account-level box
// was drawn disabled for a non-manager (app.js: `ka.disabled = inherited || !mgr`) — so an
// account owner whose own key was being spent could not switch it off from the page at all.
// DELETE /api/me/keepalive is that hole closed, one-way.

// keepAliveCtlRoutes is this feature's half of the control-plane table.
func (h *Handler) keepAliveCtlRoutes() []ctlRoute {
	return []ctlRoute{
		// What is armed RIGHT NOW: the live map, not a stored intention. There is no stored
		// intention — see keepaliveoverride.go for why an authorization to spend does not
		// survive a restart.
		{"GET /api/me/keepalive/sessions", ctlTenant, h.ctlKeepAliveArmed},
		// ctlManager, and the class is the honest statement of it: arming spends a credential,
		// so v1 keeps the same role boundary the account-wide box has. It acts on the caller's
		// OWN account — no {id} in the path — which is why it sits among the /api/me routes
		// rather than the /api/tenants ones. The handler re-checks the role for the reason
		// every handler in this file does: the gate makes the class enforced by the table, and
		// its own check keeps it correct if it is ever called from somewhere else.
		{"POST /api/me/keepalive/sessions", ctlManager, h.ctlKeepAliveArm},
		{"DELETE /api/me/keepalive/sessions/{session}", ctlTenant, h.ctlKeepAliveDisarm},
		// The account-wide off switch. ctlTenant and NOT ctlManager, deliberately: see above.
		{"DELETE /api/me/keepalive", ctlTenant, h.ctlKeepAliveOff},
	}
}

// ctlKeepAliveArmed lists the caller's own live overrides.
func (h *Handler) ctlKeepAliveArmed(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"armed": h.keeper.armedFor(t.ID),
		// Stated on the wire and not only in the copy: these are lost on restart, and a client
		// that renders them as durable is misrepresenting an authorization to spend.
		"durable":          false,
		"max_hours":        int(maxOverrideUntil / time.Hour),
		"default_hours":    int(defaultOverrideUntil / time.Hour),
		"max_hold_minutes": int(maxOverrideHold / time.Minute),
	})
}

// ctlKeepAliveArm arms one session, for a stated time, with stated parameters.
//
// The manager gate stays in v1 for the same reason it is on the account-wide box: this
// authorizes spend on a credential, and widening who may do that needs its own consent story.
// Disarming does not, and neither does the account-wide off switch.
func (h *Handler) ctlKeepAliveArm(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !t.IsManager() {
		ctlErr(w, http.StatusForbidden, "a manager arms a session's keep-alive")
		return
	}
	var in struct {
		Session         string `json:"session"`
		IdleSeconds     int    `json:"idle_seconds"`
		MaxPings        int    `json:"max_pings"`
		MinPrefixTokens int    `json:"min_prefix_tokens"`
		// Until is epoch ms. Mandatory: an override with no expiry is an authorization to
		// spend that nobody ever revisits.
		Until int64 `json:"until"`
		// MaxUSDPerPing is ACCEPTED AND IGNORED, and that is worth being explicit about. The
		// per-ping cost guard exists because ping cost is bimodal (p50 $0.0004, p99 $0.2275,
		// max $0.3780), so it is the one control an override may not widen. Silently dropping a
		// field a caller sent is worse than refusing it, so the response reports the value
		// actually in force.
		MaxUSDPerPing float64 `json:"max_usd_per_ping"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// Arming is a spend authorization, so it is rate-limited like one.
	if err := h.limiter.AllowAction(t.ID, "keep-alive arms", overrideArmsPerHour); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	now := time.Now()
	until := now.Add(defaultOverrideUntil)
	if in.Until > 0 {
		until = time.UnixMilli(in.Until)
	}
	idle := time.Duration(in.IdleSeconds) * time.Second
	if in.IdleSeconds == 0 {
		idle = time.Duration(recIdleSecondsDefault) * time.Second
	}
	pings := in.MaxPings
	if pings == 0 {
		pings = recMaxPingsDefault
	}
	// The session id is taken as SENT and keyed under the AUTHENTICATED principal. A session id
	// whose `tenant:uuid` prefix names somebody else therefore addresses nothing at all: it
	// becomes a key under the caller's own tenant that no request will ever match.
	o, err := validOverride(in.Session, idle, pings, in.MinPrefixTokens, until, now, t.ID)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.keeper.arm(t.ID, in.Session, o); err != nil {
		ctlErr(w, http.StatusConflict, err.Error())
		return
	}
	// The durable half. The live policy does not survive a restart; who authorized what spend,
	// with which parameters, until when, does.
	after := fmt.Sprintf("armed idle=%ds pings=%d min_prefix=%d until=%s hold<=%.0fmin",
		in.IdleSeconds, pings, in.MinPrefixTokens, until.UTC().Format(time.RFC3339),
		o.hold().Minutes())
	if err := h.registry().AuditWrite(t.ID, t.ID, "keepalive_session", in.Session, after); err != nil {
		// An arm we cannot account for is an arm we do not keep. The whole credential
		// arrangement rests on being reviewable.
		h.keeper.disarm(t.ID, in.Session)
		ctlErr(w, http.StatusInternalServerError, "could not record this in the audit log, so it "+
			"was not armed")
		return
	}
	// What the caller is authorizing, in the two units that matter: how long their credential
	// may be held, and what this can cost at worst. Arming without showing that is the thing
	// this whole feature is trying not to be.
	writeJSON(w, http.StatusOK, h.armedView(t, in.Session, o))
}

// The shipped policy's defaults, for a body that omits them. Named here rather than reaching
// into config so this file states what it applies.
const (
	recIdleSecondsDefault = 280
	recMaxPingsDefault    = 2
)

// armedView is the POST answer: the resolved policy plus the two numbers being authorized.
func (h *Handler) armedView(t *tenant.Tenant, session string, o sessionOverride) map[string]any {
	out := map[string]any{
		"session":           session,
		"idle_seconds":      int(o.pol.Idle.Seconds()),
		"max_pings":         o.pol.MaxPings,
		"until":             o.until.UnixMilli(),
		"hold_minutes":      o.hold().Minutes(),
		"durable":           false,
		"min_prefix_tokens": o.pol.MinPrefixTokens,
	}
	// The per-ping guard that will actually be ENFORCED, reported because the body's value is
	// ignored. Through CachePolicy.Ceiling(), so the number shown is the number enforced: the
	// account's document stores 0 for "unconfigured", and reporting that raw told the operator
	// the ceiling was $0.00 on exactly the accounts — keep-alive off, one session armed by hand
	// — where the guard resolves to the default instead.
	ceiling := CachePolicy{}.Ceiling()
	if h.opts.Tenants != nil {
		if tn, err := h.opts.Tenants.ForTenant(t); err == nil {
			ceiling = CachePolicy{MaxUSDPerPing: tn.Cache.MaxUSDPerPing}.Ceiling()
		}
	}
	out["max_usd_per_ping"] = ceiling
	// The worst case, priced from THIS SESSION's own last billed prefix at its own model's
	// cache-read rate — never a service-wide average, because per-ping cost is bimodal. Absent
	// rather than zero when the model has no rate on the operator's list.
	prefix, model, spans := h.worstCase(t.ID, session, o)
	out["last_prefix_tokens"], out["model"], out["worst_case_pings"] = prefix, model, spans
	if p := h.opts.Prices; p != nil && model != "" && prefix > 0 {
		if price, ok := p.Price(context.Background(), model); ok && !price.Zero() {
			each := float64(prefix)*price.CacheRead + price.Output
			out["ping_usd_each"] = each
			out["worst_case_usd"] = each * float64(spans)
			out["priced"] = true
		}
	}
	if _, ok := out["priced"]; !ok {
		out["priced"] = false
	}
	return out
}

// worstCase is the ceiling on what one armed override can cost: the session's last billed
// prefix, its model, and the most pings the override can send before it expires.
//
// The count is `time until expiry / X`, one ping per idle interval, and NOT `K pings per
// (K+1)X span`. A span ends the instant a real request arrives, so the worst case is not a
// session that goes quiet — it is one whose requests land just after each ping, restarting the
// clock every time and sending one ping per X for the whole hold. The span form under-states by
// (K+1)/K: 8 against a true 12 at the shipped defaults, and 2x at K=1. Under-stating a ceiling
// on a spend authorization is the one direction that cannot be defended.
//
// Deliberately a CEILING and not an estimate: a session that goes quiet sends none of them, and
// the honest thing to show somebody authorizing a spend is the most it can be.
func (h *Handler) worstCase(tenantID, session string, o sessionOverride) (prefix int64, model string, pings int64) {
	if o.pol.Idle <= 0 {
		return 0, "", 0
	}
	pings = int64(time.Until(o.until).Seconds() / o.pol.Idle.Seconds())
	if pings < 1 {
		pings = 1
	}
	if h.rec == nil {
		return 0, "", pings
	}
	p, m, err := h.rec.DB().LastBilledPrefix(dash.Filter{Tenant: tenantID}, session)
	if err != nil {
		return 0, "", pings
	}
	return p, m, pings
}

// ctlKeepAliveDisarm removes one override and releases what is held for that session NOW.
//
// Any principal on their own account: this is a withdrawal, and a withdrawal that needed a role
// would be consent that is harder to take back than to give.
func (h *Handler) ctlKeepAliveDisarm(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	session := r.PathValue("session")
	if session == "" {
		ctlErr(w, http.StatusBadRequest, "name the session to disarm")
		return
	}
	had := h.keeper.disarm(t.ID, session)
	if had {
		if err := h.registry().AuditWrite(t.ID, t.ID, "keepalive_session", session, "disarmed"); err != nil {
			// The material is already released, which is the part that matters; say the audit
			// row failed rather than pretend it did not.
			ctlErr(w, http.StatusInternalServerError, "disarmed, but the audit log could not be written")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "armed": false, "was_armed": had})
}

// ctlKeepAliveOff is the account-wide off switch, open to ANY principal on their own account.
//
// Three things, in this order, because the order is the guarantee: the stored consent goes, the
// per-session authorizations go, and the material already held is released now rather than at the
// hard deadline. One-way by design — turning it back ON keeps the manager gate, because it is a
// field of a manager-owned document.
func (h *Handler) ctlKeepAliveOff(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	reg := h.registry()
	current := reg.Config(t)
	cur, _ := config.ParseForm(current)
	if cur.ParseError != "" {
		// The document cannot be edited as fields — but the withdrawal must still take effect,
		// so the live hold is dropped and the caller is told what did not happen. Refusing
		// outright would leave a user unable to stop spend because of an unrelated typo.
		h.keeper.forget(t.ID)
		ctlErr(w, http.StatusConflict, "the keep-alive is stopped for now and every armed "+
			"session is disarmed, but your stored configuration does not load so the setting "+
			"itself could not be changed; a manager must repair it on the account page")
		return
	}
	was := "true"
	if cur.Cache == nil || !cur.Cache.KeepAlive {
		was = "false"
	}
	if cur.Cache == nil {
		cur.Cache = &config.CacheForm{}
	}
	cur.Cache.KeepAlive = false
	// The form was READ from this document, so it models every component the document runs and
	// says so. Without the claim ApplyForm would preserve names this form did not send, which is
	// the right default for a browser and pointless here.
	cur.PipelineKnown = append([]string(nil), cur.Pipeline...)
	doc, err := config.ApplyForm(current, cur)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Written with the REGISTRY's own self-service path, so it is audited field by field like
	// any other change to the document. tenant.Patch is checked against the actor inside Update.
	if err := reg.Update(t, t.ID, tenant.Patch{ConfigYAML: &doc}); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Stops RETENTION now, not at the deadline: drops every held body and credential for this
	// account and every per-session override with them.
	h.keeper.forget(t.ID)
	if err := reg.AuditWrite(t.ID, t.ID, "keepalive", was, "false"); err != nil {
		ctlErr(w, http.StatusInternalServerError, "switched off, but the audit log could not be written")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keepalive": false})
}
