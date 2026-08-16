package dash

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Finding D. The dashboard's cookie is SameSite=Lax, so a cross-site frame renders the
// UNAUTHENTICATED sign-in gate — a ready-made UI-redress phishing target for anyone on
// 9.0.0.0/8. frame-ancestors, base-uri and form-action do NOT inherit from default-src,
// and nginx sets no X-Frame-Options, so a self-only default-src leaves all three open.
func TestDashboardCSPForbidsFramingAndFormHijacking(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	m := http.NewServeMux()
	a.Mount(m)

	for _, path := range []string{"/dashboard/", "/dashboard/style.css", "/dashboard/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> %d", path, w.Code)
		}
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{
			"frame-ancestors 'none'", // clickjacking of the sign-in gate
			"base-uri 'none'",        // no rebasing of the page's relative URLs
			"form-action 'self'",     // credentials cannot be posted off-origin
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP is missing %q: %q", path, want, csp)
			}
		}
		// The directives that were already there must survive.
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s CSP lost a directive it had: %q", path, csp)
		}
	}
}
