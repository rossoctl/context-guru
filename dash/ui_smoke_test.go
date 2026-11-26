package dash

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestUISmokeEveryTab opens every dashboard tab in a real headless browser, against the golden
// fixture served through the real route table, and fails on any JavaScript error, a missing
// heading or intro, an error state on a tab whose data this package serves, or anything left of
// the removed Overview sections or the Benchmarks tab. It needs node, playwright-core (on
// NODE_PATH or CG_SMOKE_NODE_PATH) and a Chromium (CG_SMOKE_CHROME, or one under
// ~/.cache/ms-playwright); without them it skips and says why.
func TestUISmokeEveryTab(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the UI smoke test did not run")
	}
	chrome := os.Getenv("CG_SMOKE_CHROME")
	if chrome == "" {
		home, _ := os.UserHomeDir()
		m, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", "chromium-*", "chrome-linux64", "chrome"))
		sort.Strings(m)
		if len(m) > 0 {
			chrome = m[len(m)-1]
		}
	}
	if chrome == "" {
		t.Skip("no Chromium found (set CG_SMOKE_CHROME); the UI smoke test did not run")
	}
	nodePath := os.Getenv("CG_SMOKE_NODE_PATH")
	if nodePath == "" {
		nodePath = os.Getenv("NODE_PATH")
	}
	probe := exec.Command(node, "-e", "require('playwright-core')")
	probe.Env = append(os.Environ(), "NODE_PATH="+nodePath)
	if err := probe.Run(); err != nil {
		t.Skip("playwright-core is not resolvable (set CG_SMOKE_NODE_PATH); the UI smoke test did not run")
	}

	a := goldenAPI(t)
	// The fixture is served as a hosted manager so every tab is reachable.
	a.SetAuth(func(*http.Request) (Principal, bool) { return Principal{TenantID: "t1", Manager: true}, true })
	a.SetWhoami(func(*http.Request) any {
		return map[string]any{
			"authenticated": true, "hosted": true,
			"tenant": map[string]any{"id": "t1", "label": "demo", "email": "demo@example.test", "role": "manager"},
		}
	})
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cmd := exec.Command(node, "ui_smoke.mjs", srv.URL, chrome)
	cmd.Env = append(os.Environ(), "NODE_PATH="+nodePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UI smoke test failed:\n%s", out)
	} else {
		t.Log(string(out))
	}
}

// The Benchmarks tab's data route is gone with it.
func TestBenchmarksRouteIsGone(t *testing.T) {
	a := goldenAPI(t)
	for _, path := range []string{"/api/benchmarks", "/api/benchmarks/1/tasks"} {
		w, _ := get(t, a, path, "127.0.0.1:1234")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, w.Code)
		}
	}
}

// The History tab is removed: it showed the same data as Cleanup, only earlier. Its route and its
// script go with it, so nothing serves or loads a page that no longer exists.
func TestHistoryRouteIsGone(t *testing.T) {
	a := goldenAPI(t)
	if w, _ := get(t, a, "/api/history", "127.0.0.1:1234"); w.Code != http.StatusNotFound {
		t.Errorf("/api/history = %d, want 404", w.Code)
	}
	if strings.Contains(readUI(t, "ui/index.html"), "history.js") {
		t.Error("index.html still loads history.js")
	}
}
