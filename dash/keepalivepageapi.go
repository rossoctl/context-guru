package dash

import (
	"net/http"
	"strconv"
)

// The keep-alive PAGE's two read routes.
//
// Both in API.routes(), which is the table Mount walks AND the table both scoping tests walk —
// TestEveryMountedRouteDeclaresItsScope and TestNoRouteServesContentTextFromAnUntrustedAddress —
// exactly the reasoning dash/keepaliveapi.go's own top-of-file comment gives for its six
// routes. Both are scopeTenant, and both return numbers, enum labels, session ids and this
// file's own static feature-catalog prose — no prompt text, no transcript, no tool schema.
func (a *API) keepAlivePageRoutes() []route {
	return []route{
		{"GET /api/keepalive/page/session", scopeTenant, a.keepAlivePageSession},
		{"GET /api/keepalive/page/sessions", scopeTenant, a.keepAlivePageSessions},
	}
}

// keepAlivePageSession serves one session's whole page: the timeline, the side-by-side
// predictors, the feature inspector and the economics.
//
// The COHORT filter is the same one .scope(r) already parsed, with Session cleared — so the
// tenant/model/agent/stop_reason narrowings the shared filter bar sends apply to the cohort
// panels exactly as they do everywhere else on the dashboard, and the session panels stay
// pinned to the one session regardless of them.
func (a *API) keepAlivePageSession(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	if f.Session == "" {
		httpErr(w, http.StatusBadRequest, "name a session: ?session=<id>")
		return
	}
	cohort := f
	cohort.Session = ""
	out, err := a.db(r).KeepAlivePageSession(f, cohort, a.pricer)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, out)
}

// keepAlivePageSessions serves the session picker / cohort list: every session matching the
// current tenant/model/agent/stop_reason filter, costliest-addressable-miss first.
func (a *API) keepAlivePageSessions(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	f.Session = "" // the picker lists sessions IN the cohort, not rows within one
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.db(r).KeepAlivePageSessions(f, limit)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": rows})
}
