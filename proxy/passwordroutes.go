package proxy

import (
	"regexp"
	"strings"
)

// PasswordRoutePatterns names every control-plane route that belongs to the PASSWORD flow: the
// routes that present, set or reset a password, and the account creation and verification steps
// that flow depends on.
//
// "Password flow" rather than "authenticates with a password", because two entries are broader
// than the narrower phrasing would suggest and a host trusting this list should know which:
// ctlRegister takes a password and returns ErrBadPassword, ctlVerify mints the session that
// completes the emailed half, and POST /api/login has a token branch taken BEFORE any password
// work, so withdrawing it also removes a bare-token dashboard sign-in. For an external-IdP
// deployment that is the intent — the IdP is the only way in — but it is not only about passwords.
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
// PASS r.URL.EscapedPath(), NOT r.URL.Path. This matters and is not a style preference:
// http.ServeMux routes on the ESCAPED path, so "%2F" inside a segment stays part of that segment
// and the {id} wildcard matches it. r.URL.Path is DECODED, where the same "%2F" has become a
// literal "/" and looks like two segments. Matching the decoded form therefore answers false for
// a request the mux routes to the handler:
//
//	POST /api/tenants/a%2Fb/password-reset
//	  mux                             → matches {id}, handler RUNS
//	  IsPasswordRoute(r.URL.Path)     → false   ← the hole
//	  IsPasswordRoute(r.URL.EscapedPath()) → true
//
// Callers are defended against the mistake rather than merely warned about it: a path containing
// a percent-encoded slash is treated as ONE segment here too, so passing the decoded path still
// errs towards reporting a password route rather than away from it.
//
// method is compared case-sensitively — HTTP methods are upper-case by definition, and ServeMux
// does not fold them either.
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
		if id == "" {
			return false
		}
		// An encoded slash is NOT a segment boundary — ServeMux does not treat it as one, so
		// neither may this. Unescaping first would merge "a%2Fb" into "a/b" and reject a
		// request the mux accepts; instead the encoded forms are folded to a placeholder so
		// such an id counts as the single segment the mux sees it as.
		unencoded := encodedSlash.ReplaceAllString(id, "-")
		return !strings.Contains(unencoded, "/")
	}
	return false
}

// encodedSlash matches the percent-encoded forms of "/" in either case. Kept as a package-level
// regexp so the matcher stays allocation-cheap on a path that has none.
var encodedSlash = regexp.MustCompile(`(?i)%2f`)
