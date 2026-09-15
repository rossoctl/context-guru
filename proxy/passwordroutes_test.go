package proxy

import "testing"

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
	// And the converse: every mounted route that looks like a password route must be named.
	for pattern := range mounted {
		method, path := splitPattern(pattern)
		looksLikeOne := path == "/api/register" || path == "/api/login" || path == "/api/verify" ||
			path == "/api/me/password" || path == "/api/password-reset" ||
			path == "/api/password-reset/verify" || path == "/api/tenants/{id}/password-reset"
		if !looksLikeOne {
			continue
		}
		named := false
		for _, p := range PasswordRoutePatterns() {
			if p == pattern {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("%s %s is mounted and is a password route, but PasswordRoutePatterns does "+
				"not name it", method, path)
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
