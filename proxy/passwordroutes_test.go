package proxy

import (
	"strings"
	"testing"
)

// THE LIST MUST MATCH THE MOUNTED TABLE. That is the whole value of exporting it: a host asks
// core which routes are password routes instead of hardcoding a list that rots the moment one is
// added. If this test fails, a password route was added or renamed and PasswordRoutePatterns did
// not follow — which would leave a host believing it withdrew a route it did not.
func TestPasswordRoutePatternsMatchTheMountedRoutes(t *testing.T) {
	h := &Handler{}
	mounted := map[string]bool{}
	for _, rt := range h.ctlRoutes() {
		mounted[rt.pattern] = true
	}
	for _, p := range PasswordRoutePatterns() {
		if !mounted[p] {
			t.Errorf("PasswordRoutePatterns names %q, which ctlRoutes does not mount — the list "+
				"has drifted from the table", p)
		}
	}
	// THE CONVERSE, AND IT MUST NOT RESTATE THE LIST. An earlier version of this test compared
	// each mounted route against a hardcoded re-listing of the same seven paths, which reduced the
	// whole check to "the list matches the list": adding a genuinely new password route to
	// ctlRoutes left the suite green. That is the one direction the export exists to protect
	// against ("silently rots the moment a route is added"), so the signal here has to be
	// INDEPENDENT of PasswordRoutePatterns.
	//
	// The independent signal is the PATH ITSELF: a route whose path mentions "password" is a
	// password route, whatever it is called. That catches an addition without knowing about it in
	// advance, which a fixed predicate cannot.
	named := map[string]bool{}
	for _, p := range PasswordRoutePatterns() {
		named[p] = true
	}
	for pattern := range mounted {
		_, path := splitPattern(pattern)
		if !strings.Contains(strings.ToLower(path), "password") {
			continue
		}
		if !named[pattern] {
			t.Errorf("%q is mounted and its path names a password, but PasswordRoutePatterns does "+
				"not include it. Either add it to the list, or — if it genuinely is not a "+
				"password route — say so here with a comment explaining why", pattern)
		}
	}

	// The three routes whose paths do NOT contain "password" cannot be caught by the rule above,
	// so they are pinned individually: account creation and verification are half of the password
	// flow (ctlRegister takes a Password and returns ErrBadPassword; ctlVerify mints the session
	// it leads to) and login is where a password is presented.
	for _, p := range []string{"POST /api/register", "POST /api/login", "POST /api/verify"} {
		if !named[p] {
			t.Errorf("%q must be named: it is part of the password flow even though its path does "+
				"not say so", p)
		}
	}
}

// IsPasswordRoute exists so a host does not reimplement the {id} wildcard and get it subtly
// wrong. These are the cases where a naive prefix/suffix match differs from the real pattern.
func TestIsPasswordRoute(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/register", true},
		{"POST", "/api/login", true},
		{"POST", "/api/verify", true},
		{"POST", "/api/me/password", true},
		{"POST", "/api/password-reset", true},
		{"POST", "/api/password-reset/verify", true},
		{"POST", "/api/tenants/abc123/password-reset", true},

		// Method matters: the same paths under GET are not these routes.
		{"GET", "/api/register", false},
		{"GET", "/api/tenants/abc123/password-reset", false},

		// Not password routes, and a sloppy matcher would catch some of these.
		{"POST", "/api/tenants", false},
		{"POST", "/api/tenants/abc123/tokens", false},
		{"POST", "/api/me/tokens", false},
		{"POST", "/api/feedback", false},
		{"POST", "/", false},
		{"POST", "", false},

		// The wildcard is ONE segment: a nested path must not match.
		{"POST", "/api/tenants/a/b/password-reset", false},
		// AN ENCODED SLASH IS NOT A SEGMENT BOUNDARY, because ServeMux does not treat it as
		// one: it routes on the escaped path, so "a%2Fb" is a single segment and the handler
		// RUNS. Answering false here is the bypass this case pins — a host filtering on this
		// function would pass the request straight through to ctlManagerReset.
		{"POST", "/api/tenants/a%2Fb/password-reset", true},
		{"POST", "/api/tenants/a%2fb/password-reset", true},
		// ...and an empty id is not an id.
		{"POST", "/api/tenants//password-reset", false},
		// A path that merely ends the same way is not the route.
		{"POST", "/api/other/abc/password-reset", false},
	} {
		if got := IsPasswordRoute(tc.method, tc.path); got != tc.want {
			t.Errorf("IsPasswordRoute(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// PURELY ADDITIVE: core still mounts every password route. This pins that the new API only
// REPORTS, so no existing deployment changes behaviour by upgrading.
func TestCoreStillMountsThePasswordRoutes(t *testing.T) {
	h := &Handler{}
	mounted := map[string]bool{}
	for _, rt := range h.ctlRoutes() {
		mounted[rt.pattern] = true
	}
	for _, p := range PasswordRoutePatterns() {
		if !mounted[p] {
			t.Errorf("%q is no longer mounted; this change must not withdraw anything", p)
		}
	}
}
