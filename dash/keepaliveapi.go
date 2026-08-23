package dash

import (
	"net/http"
	"strconv"
	"time"
)

// The keep-alive tab's six read routes.
//
// All in API.routes(), which is the table Mount walks AND the table both scoping tests walk —
// TestEveryMountedRouteDeclaresItsScope and TestNoRouteServesContentTextFromAnUntrustedAddress.
// A route mounted beside that table would be a route whose scope nothing checks, which is
// exactly how three unauthenticated routes and then /api/prompt shipped.
//
// Every one of them is scopeTenant, and every one returns NUMBERS AND ENUM LABELS ONLY: no
// prompt text, no transcript, no tool schema. That is why no CIDR gate is needed and why the
// content-gate test passes on them by construction. Session IDS are returned — they are the
// caller's own, the same as /api/sessions — but no content behind them. If any field here ever
// grows text, it must apply a.trusted(r) exactly as a.prompt does.
func (a *API) keepAliveRoutes() []route {
	return []route{
		{"GET /api/keepalive", scopeTenant, a.keepAlive},
		{"GET /api/keepalive/behaviour", scopeTenant, a.keepAliveBehaviour},
		{"GET /api/keepalive/sessions", scopeTenant, a.keepAliveSessions},
		{"GET /api/keepalive/calc", scopeTenant, a.keepAliveCalc},
		{"GET /api/keepalive/live", scopeTenant, a.keepAliveLive},
		{"GET /api/keepalive/recommend", scopeTenant, a.keepAliveRecommend},
	}
}

// keepAlive serves the ledger: panels 1 and 2, the verdict and the losing majority.
func (a *API) keepAlive(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	led, err := a.rec.DB().KeepAliveLedger(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, led)
}

// keepAliveBehaviour serves panels 3a–3e.
func (a *API) keepAliveBehaviour(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	// The coverage the gap bands are marked against, from the caller's own current policy where
	// the page knows it. Defaulted in the DB layer, never guessed here.
	x, _ := strconv.ParseFloat(r.URL.Query().Get("x"), 64)
	k, _ := strconv.Atoi(r.URL.Query().Get("k"))
	b, err := a.rec.DB().KeepAliveBehaviour(f, CoverageSeconds(x, k))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, b)
}

// keepAliveSessions serves the per-session concentration, costliest first.
func (a *API) keepAliveSessions(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.rec.DB().KeepAliveSessions(f, limit)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": rows})
}

// keepAliveCalc serves the K ladder at one idle interval and one prefix.
//
// The prefix is REQUIRED to be a real one, and the fallback chain is explicit: the session's own
// last billed prefix, then the account's own median, then nothing — in which case the panel asks
// for one rather than inventing a service-wide average. Per-ping cost is bimodal (p50 $0.0004,
// p99 $0.2275), so an average of it would be a number that describes no request.
func (a *API) keepAliveCalc(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	q := r.URL.Query()
	x, _ := strconv.ParseFloat(q.Get("x"), 64)
	k, _ := strconv.Atoi(q.Get("k"))
	prefix, _ := strconv.ParseInt(q.Get("prefix"), 10, 64)
	model, source := q.Get("model"), "given"
	db := a.rec.DB()
	if session := q.Get("session"); session != "" {
		p, m, err := db.LastBilledPrefix(f, session)
		if err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if p > 0 {
			prefix, source = p, "session"
			if model == "" {
				model = m
			}
		}
	}
	if prefix <= 0 {
		p, m, err := db.AccountMedianPrefix(f)
		if err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// "account_median" only when there IS one. On an account with no addressable expiry the
		// median comes back 0, and labelling that a measurement is the same defect the whole tab
		// polices: `prefix_source` is on the wire so nobody reads a typed number as a measured
		// one, and it was reporting a measurement where there was no number at all.
		prefix = p
		if p > 0 {
			source = "account_median"
		} else {
			source = "none"
		}
		// The account's own most-used model on those expiries, not a blend and not a default:
		// without it the panel reported "not priced" on a deployment whose price list was fine,
		// simply because nothing had named a model. Still refuses to invent a rate.
		if model == "" {
			model = m
		}
	}
	out, err := db.KeepAliveCalc(f, x, prefix, model, a.priceFn(r), k)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.PrefixSource = source
	writeJSON(w, out)
}

// keepAliveLive serves the sessions whose provider cache entry has not lapsed yet: the lifetime
// in force on each, what is left of it, and what one ping and one lapse cost on that session's
// own prefix.
//
// x and k come from the caller's current policy, as on /behaviour, because the reach figures are
// arithmetic on them. The clock is the SERVER'S: a countdown computed against a browser clock
// that is minutes out reads "2 minutes left" on an entry that expired ten ago.
func (a *API) keepAliveLive(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	q := r.URL.Query()
	x, _ := strconv.ParseFloat(q.Get("x"), 64)
	k, _ := strconv.Atoi(q.Get("k"))
	out, err := a.rec.DB().KeepAliveLive(f, time.Now().UnixMilli(), x, k, a.priceFn(r))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, out)
}

// keepAliveRecommend serves either a recommendation with its range and its n, or a refusal.
func (a *API) keepAliveRecommend(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	rec, err := a.rec.DB().KeepAliveRecommend(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rec)
}
