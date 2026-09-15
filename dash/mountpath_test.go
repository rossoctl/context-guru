package dash

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mountPathAPI(t *testing.T, prefix string) http.Handler {
	t.Helper()
	rec, err := NewRecorder(Options{DBPath: t.TempDir() + "/d.db"})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() })
	a := NewAPI(rec)
	if prefix != "" {
		a.SetUIPath(prefix)
	}
	mux := http.NewServeMux()
	a.Mount(mux)
	return mux
}

func isUIShell(body string) bool {
	return strings.Contains(strings.ToLower(body), "<!doctype html")
}

// THE DEFAULT IS UNCHANGED. This is the property that matters most: a deployment that never calls
// SetUIPath must see exactly what it saw before this existed, because /dashboard/ is in the README
// and in everybody's bookmarks.
func TestTheDefaultUIPathIsUnchanged(t *testing.T) {
	if DefaultUIPath != "/dashboard/" {
		t.Fatalf("DefaultUIPath = %q; changing it breaks every existing bookmark", DefaultUIPath)
	}
	mux := mountPathAPI(t, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/", nil))
	if rec.Code != http.StatusOK || !isUIShell(rec.Body.String()) {
		t.Errorf("GET /dashboard/ = %d, want 200 with the UI shell", rec.Code)
	}
	// And the bare form still redirects to the slashed one.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET /dashboard = %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard/" {
		t.Errorf("Location = %q, want /dashboard/", got)
	}
}

// A HOST CAN MOVE IT, which is the point: renaming the dashboard used to require forking api.go.
func TestSetUIPathMovesTheMountAndTheRedirect(t *testing.T) {
	const prefix = "/dashboard-stats/"
	mux := mountPathAPI(t, prefix)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, prefix, nil))
	if rec.Code != http.StatusOK || !isUIShell(rec.Body.String()) {
		t.Errorf("GET %s = %d, want 200 with the UI shell", prefix, rec.Code)
	}

	// The bare form of the NEW prefix redirects to the new slashed form.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard-stats", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET /dashboard-stats = %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != prefix {
		t.Errorf("Location = %q, want %q", got, prefix)
	}

	// AND THE OLD PREFIX IS GONE. A host that moved the dashboard did not ask for two of them;
	// if it wants the old path to redirect, that is its own route to add.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard/", nil))
	if rec.Code == http.StatusOK && isUIShell(rec.Body.String()) {
		t.Error("the old prefix still serves the UI after SetUIPath; the mount was duplicated " +
			"rather than moved")
	}
}

// ASSETS MOVE WITH IT. The UI references siblings relatively, so a moved mount must serve them at
// the new prefix without any change to the assets themselves.
func TestAssetsAreServedUnderTheNewPrefix(t *testing.T) {
	const prefix = "/dashboard-stats/"
	mux := mountPathAPI(t, prefix)
	for _, asset := range []string{"index.html", "style.css", "app.js"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, prefix+asset, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s%s = %d (%d bytes), want 200 with content",
				prefix, asset, rec.Code, rec.Body.Len())
		}
	}
}

// THE REFUSAL MESSAGE MUST NAME THE CONFIGURED PATH. Telling somebody to sign in at a prefix this
// deployment does not serve is worse than not telling them where at all.
func TestTheSignInRefusalNamesTheConfiguredPath(t *testing.T) {
	rec := httptest.NewRecorder()
	a := &API{uiPath: "/dashboard-stats/"}
	a.unauthorized(rec)
	if body := rec.Body.String(); !strings.Contains(body, "/dashboard-stats/") {
		t.Errorf("refusal = %q, want it to name /dashboard-stats/", body)
	}

	// And the default still names the default.
	rec = httptest.NewRecorder()
	(&API{}).unauthorized(rec)
	if body := rec.Body.String(); !strings.Contains(body, DefaultUIPath) {
		t.Errorf("refusal = %q, want it to name %s", body, DefaultUIPath)
	}
}

// A HALF-FORMED PREFIX PANICS AT STARTUP, deliberately. Without the trailing slash the UI's
// relative asset references resolve one directory up and the page renders blank with no error —
// materially harder to diagnose than a panic on the line that caused it.
func TestSetUIPathRejectsAMalformedPrefix(t *testing.T) {
	for _, bad := range []string{"dashboard/", "/dashboard", "", "/", "dashboard"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SetUIPath(%q) did not panic; a half-formed prefix serves a blank "+
						"page with no error", bad)
				}
			}()
			(&API{}).SetUIPath(bad)
		}()
	}
}
