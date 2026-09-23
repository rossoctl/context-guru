package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// AuthExternal must make the password routes ABSENT, not merely unused. Present-but-unused is not
// equivalent: /api/register and /api/password-reset are ctlPublic, so an unauthenticated caller
// reaches them and can create or take over an account whose identity the IdP is meant to own.

func TestReadAuthModeDefaultsToPassword(t *testing.T) {
	t.Setenv(envAuthMode, "")
	got, err := ReadAuthMode()
	if err != nil {
		t.Fatalf("ReadAuthMode: %v", err)
	}
	if got != AuthPassword {
		t.Errorf("unset %s = %q, want %q — an existing deployment must be unaffected",
			envAuthMode, got, AuthPassword)
	}
	if got.DisablesPasswordRoutes() {
		t.Error("the default must not withdraw the password routes")
	}
}

// AN UNRECOGNISED VALUE IS AN ERROR, and this is the reason the mode is read through a function.
// A typo'd security setting must never fail towards LESS secure: CG_AUTH=oauth2 quietly leaving
// the password door open is the outcome being refused.
func TestReadAuthModeRefusesAnUnrecognisedValue(t *testing.T) {
	for _, v := range []string{"oauth2", "saml", "yes", "true", "none", "off"} {
		t.Setenv(envAuthMode, v)
		got, err := ReadAuthMode()
		if err == nil {
			t.Errorf("%s=%q was accepted as %q; an unrecognised mode must be an error",
				envAuthMode, v, got)
		}
		if got != "" {
			t.Errorf("%s=%q returned mode %q alongside its error; it must return no mode",
				envAuthMode, v, got)
		}
	}
}

// The documented spelling and every accepted alias resolve to AuthExternal. "w3id" matters
// specifically: the deployment this came from already has CG_AUTH=w3id on disk.
func TestReadAuthModeAcceptsExternalAndItsAliases(t *testing.T) {
	for _, v := range []string{"external", "EXTERNAL", " external ", "w3id", "sso", "oidc",
		"proxy", "header"} {
		t.Setenv(envAuthMode, v)
		got, err := ReadAuthMode()
		if err != nil {
			t.Errorf("%s=%q: %v", envAuthMode, v, err)
			continue
		}
		if got != AuthExternal {
			t.Errorf("%s=%q = %q, want %q", envAuthMode, v, got, AuthExternal)
		}
		if !got.DisablesPasswordRoutes() {
			t.Errorf("%s=%q does not withdraw the password routes", envAuthMode, v)
		}
	}
}

// THE WITHDRAWAL ITSELF, checked against the single list a host is told to expect.
func TestAuthExternalWithdrawsEveryPasswordRouteFromTheTable(t *testing.T) {
	external := &Handler{opts: Options{AuthMode: AuthExternal}}
	mounted := map[string]bool{}
	for _, rt := range external.ctlRoutes() {
		mounted[rt.pattern] = true
	}
	for _, p := range PasswordRoutePatterns() {
		if mounted[p] {
			t.Errorf("%q is still in the route table under AuthExternal", p)
		}
	}

	// And the default still mounts them all, so this feature cannot silently disable the product.
	def := &Handler{opts: Options{}}
	deflt := map[string]bool{}
	for _, rt := range def.ctlRoutes() {
		deflt[rt.pattern] = true
	}
	for _, p := range PasswordRoutePatterns() {
		if !deflt[p] {
			t.Errorf("%q is missing from the DEFAULT route table; AuthPassword must be unchanged", p)
		}
	}
}

// EVERYTHING ELSE SURVIVES. A withdrawal that also removed the dashboard or /api/logout would be
// worse than the door it closes — logout ends a session rather than a password login, so an
// externally-authenticated user still needs it.
func TestAuthExternalKeepsEveryOtherRoute(t *testing.T) {
	def := &Handler{opts: Options{}}
	external := &Handler{opts: Options{AuthMode: AuthExternal}}

	withdrawn := map[string]bool{}
	for _, p := range PasswordRoutePatterns() {
		withdrawn[p] = true
	}
	kept := map[string]bool{}
	for _, rt := range external.ctlRoutes() {
		kept[rt.pattern] = true
	}
	for _, rt := range def.ctlRoutes() {
		if withdrawn[rt.pattern] {
			continue
		}
		if !kept[rt.pattern] {
			t.Errorf("%q was withdrawn but is not a password route", rt.pattern)
		}
	}
	if !kept["POST /api/logout"] {
		t.Error("/api/logout was withdrawn; it ends a session, not a password login")
	}
}

// THE MUX MUST AGREE WITH THE TABLE. Mount walks ctlRoutes, so a withdrawn route has to be
// genuinely unreachable — a 404, not a 401. This is the property a front-mux mitigation cannot
// give, because the table would still hold the route.
func TestWithdrawnPasswordRoutesAre404OnTheMux(t *testing.T) {
	h := &Handler{opts: Options{AuthMode: AuthExternal}}
	mux := http.NewServeMux()
	h.MountControl(mux)

	for _, path := range []string{"/api/register", "/api/login", "/api/me/password",
		"/api/password-reset", "/api/password-reset/verify"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d under AuthExternal, want 404 (absent, not merely refused)",
				path, rec.Code)
		}
	}
}

// The env var name is part of the contract an operator configures, so it is pinned.
func TestAuthModeEnvVarName(t *testing.T) {
	if envAuthMode != "CG_AUTH" {
		t.Errorf("envAuthMode = %q; operators have this in their unit files", envAuthMode)
	}
}
