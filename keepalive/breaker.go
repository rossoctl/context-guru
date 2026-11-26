package keepalive

import (
	"log/slog"
	"time"
)

// The route breaker and the dead-credential marks (main #527). Both are keyed with the tenant
// first, so one tenant can never switch off another's pings.
const (
	routeFailLimit = 3         // failures in a row that trip a route
	routeTripFor   = time.Hour // how long a trip, and a dead credential, lasts
)

// routeKey names one (tenant, endpoint+route, model) route — matching main's kaRouteKey
// (tenant + "\x00" + up.base + up.path + "\x00" + model, `git show 67c5f758:proxy/keepalive.go`
// L1557-1558): endpoint and route are concatenated directly, with no separator between them,
// because that pair is exactly where the ping is sent (base+path), so two different paths on
// the same endpoint must trip independently.
func routeKey(tenant, endpoint, route, model string) string {
	return tenant + "\x00" + endpoint + route + "\x00" + model
}

// credModelKey names one credential on one model, for a 403.
func credModelKey(tenant, cred, model string) string {
	return tenant + "\x00" + cred + "\x00" + model
}

// credAllKey names one credential on every model, for a 401.
func credAllKey(tenant, cred string) string { return tenant + "\x00" + cred }

// routeFailedLocked counts a failure on route and trips it at the limit. It also prunes trips
// that have expired, so the map cannot grow without bound. It reports whether this failure
// tripped the route, so the caller can log the trip once k.mu is released. k.mu must be held.
func (k *Keeper) routeFailedLocked(route string, now time.Time) bool {
	for r, until := range k.routeTripped {
		if !now.Before(until) {
			delete(k.routeTripped, r)
		}
	}
	k.routeFails[route]++
	if k.routeFails[route] >= routeFailLimit {
		delete(k.routeFails, route)
		k.routeTripped[route] = now.Add(routeTripFor)
		k.routeTrips.Add(1)
		return true
	}
	return false
}

// routeFailed is routeFailedLocked for a ping's outcome: it takes k.mu itself and, when the
// failure trips the route, logs main's operator-visible Warn. The log names the tenant (by
// label), the endpoint+route and the model, never the credential or its id.
func (k *Keeper) routeFailed(j pingJob) {
	k.mu.Lock()
	tripped := k.routeFailedLocked(routeKey(j.tenant, j.e.endpoint, j.e.route, j.e.model), k.now())
	k.mu.Unlock()
	if tripped {
		slog.Warn("context-guru: cache keep-alive off for a route whose pings keep refreshing nothing",
			"tenant", tenantLabel(j.tenant), "route", j.e.endpoint+j.e.route, "model", j.e.model,
			"failures", routeFailLimit, "for", routeTripFor.String())
	}
}

// routeOKLocked resets route's run of failures. k.mu must be held.
func (k *Keeper) routeOKLocked(route string) { delete(k.routeFails, route) }

// routeIsTrippedLocked reports whether route is tripped at now. k.mu must be held.
func (k *Keeper) routeIsTrippedLocked(route string, now time.Time) bool {
	until, ok := k.routeTripped[route]
	return ok && now.Before(until)
}

// markCredDeadLocked marks cred dead for an hour: on every model for a 401, on model only for a
// 403. It prunes expired marks first. k.mu must be held.
func (k *Keeper) markCredDeadLocked(tenant, cred, model string, status int, now time.Time) {
	for c, until := range k.credDead {
		if !now.Before(until) {
			delete(k.credDead, c)
		}
	}
	key := credModelKey(tenant, cred, model)
	if status == 401 {
		key = credAllKey(tenant, cred)
	}
	k.credDead[key] = now.Add(routeTripFor)
}

// credIsDeadLocked reports whether cred is dead on model. "" (a server-held key) never is.
// k.mu must be held.
func (k *Keeper) credIsDeadLocked(tenant, cred, model string, now time.Time) bool {
	if cred == "" {
		return false
	}
	for _, key := range []string{credAllKey(tenant, cred), credModelKey(tenant, cred, model)} {
		if until, ok := k.credDead[key]; ok && now.Before(until) {
			return true
		}
	}
	return false
}
