package proxy

import (
	"fmt"
	"os"
	"strings"
)

// Authentication modes for the control plane.
//
// WHY THIS EXISTS. A deployment fronted by an external identity provider — SSO, OIDC, an
// enterprise gateway — has no passwords, and the seven password routes must then be ABSENT rather
// than merely unused. Present-but-unused is not equivalent: POST /api/register and
// POST /api/password-reset are ctlPublic, so an unauthenticated caller can reach them and create
// or take over an account whose identity the IdP is supposed to be the only source of.
//
// PasswordRoutePatterns (passwordroutes.go) tells a host WHICH routes those are. This tells core
// to stop mounting them, which is the part a host cannot do for itself: Go's http.ServeMux panics
// on a duplicate pattern, so a host cannot shadow core's handlers on core's own mux, and a front
// mux answering 404 is a mitigation rather than withdrawal — the route table still holds the
// routes, so nothing can walk it and prove the door is shut.
//
// READ ONCE AT STARTUP, never per request. Authentication mode decides which routes EXIST; a
// route table mutating under a running process is harder to reason about and there is no
// operational need for it.

// envAuthMode is the variable. Named here so the error message and the docs cannot drift.
const envAuthMode = "CG_AUTH"

// AuthMode selects how a human authenticates to the control plane.
type AuthMode string

const (
	// AuthPassword is the default and today's behaviour: email, password and a mailed code. The
	// zero value, so a deployment that never sets CG_AUTH is unaffected by this existing.
	AuthPassword AuthMode = "password"
	// AuthExternal means an external identity provider is the ONLY way in, and the password
	// routes are not mounted at all. The provider is the host's business: core only needs to know
	// that passwords are not in use.
	AuthExternal AuthMode = "external"
)

// authModeAliases are accepted spellings of AuthExternal.
//
// "w3id" is here because this feature was built for a W3ID-fronted deployment and its operators
// already have CG_AUTH=w3id in their systemd drop-ins. Accepting the alias costs one map entry
// and means upstreaming the feature does not break the deployment it came from. New deployments
// should say "external", which describes the shape rather than one vendor.
var authModeAliases = map[string]AuthMode{
	"w3id":   AuthExternal,
	"sso":    AuthExternal,
	"oidc":   AuthExternal,
	"proxy":  AuthExternal,
	"header": AuthExternal,
}

// ReadAuthMode reads CG_AUTH.
//
// Empty or unset → AuthPassword, so an existing deployment is untouched.
//
// AN UNRECOGNISED VALUE IS AN ERROR, deliberately, and this is the whole point of reading it
// through a function rather than comparing a string at the call site. A typo'd security-relevant
// setting must never fail towards LESS secure: CG_AUTH=oauth2 silently leaving the password door
// open is exactly the outcome this refuses. The caller is expected to treat the error as fatal.
func ReadAuthMode() (AuthMode, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envAuthMode)))
	switch v {
	case "":
		return AuthPassword, nil
	case string(AuthPassword):
		return AuthPassword, nil
	case string(AuthExternal):
		return AuthExternal, nil
	}
	if m, ok := authModeAliases[v]; ok {
		return m, nil
	}
	return "", fmt.Errorf("%s=%q is not a recognised authentication mode: use %q when an "+
		"external identity provider is the only way in, or leave it unset for email and password",
		envAuthMode, v, AuthExternal)
}

// DisablesPasswordRoutes reports whether the password door is withdrawn.
//
// A method rather than an equality test at each call site: there are several, and "== something"
// repeated is how one of them comes to compare against the wrong thing.
func (m AuthMode) DisablesPasswordRoutes() bool { return m == AuthExternal }
