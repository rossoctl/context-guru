package proxy

import "strings"

// PasswordRoutePatterns names every control-plane route that authenticates with, or mutates, an
// account PASSWORD.
//
// WHY THIS IS EXPORTED. A deployment that authenticates through an external identity provider —
// SSO, OIDC, an enterprise gateway — has no passwords, and these routes must then be absent
// rather than merely unused. Present-but-unused is not equivalent: POST /api/register and
// POST /api/password-reset are ctlPublic, so an unauthenticated caller can reach them and create
// or take over an account whose identity the IdP is supposed to be the only source of.
//
// Such a host previously had no way to know WHICH patterns to withdraw. It had to hardcode the
// list, which then silently rots the moment a route is added here — the failure being a route the
// host believes it withdrew and did not. Asking core is what keeps the two in step.
//
// The patterns are returned in http.ServeMux form ("METHOD /path", with a "{id}" wildcard where
// one exists), so a caller can compare them against its own mux registrations directly.
//
// This function only REPORTS. Core mounts these routes exactly as before, so no existing
// deployment changes behaviour; withdrawal is the host's decision and the host's code.
func PasswordRoutePatterns() []string {
	return []string{
		"POST /api/register",
		"POST /api/login",
		"POST /api/verify",
		"POST /api/me/password",
		"POST /api/password-reset",
		"POST /api/password-reset/verify",
		"POST /api/tenants/{id}/password-reset",
	}
}

// IsPasswordRoute reports whether a request's method and path address one of the routes
// PasswordRoutePatterns names.
//
// Provided because the patterns are not all literal: "POST /api/tenants/{id}/password-reset"
// carries a tenant id, so a host matching on strings alone has to reimplement that wildcard —
// and a host that reimplements it slightly differently has a hole exactly where it thinks it has
// a wall. Matching lives here, next to the list it matches.
//
// method is compared case-sensitively (HTTP methods are upper-case by definition) and path must
// be the cleaned request path, i.e. r.URL.Path.
func IsPasswordRoute(method, path string) bool {
	if method != "POST" {
		return false
	}
	switch path {
	case "/api/register", "/api/login", "/api/verify",
		"/api/me/password", "/api/password-reset", "/api/password-reset/verify":
		return true
	}
	// POST /api/tenants/{id}/password-reset — one path segment stands in for {id}, so
	// "/api/tenants/a/b/password-reset" must NOT match.
	const pre, suf = "/api/tenants/", "/password-reset"
	if strings.HasPrefix(path, pre) && strings.HasSuffix(path, suf) {
		id := path[len(pre) : len(path)-len(suf)]
		return id != "" && !strings.Contains(id, "/")
	}
	return false
}
