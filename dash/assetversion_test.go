package dash

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// localRef finds every same-origin asset URL the served markup or script asks the
// browser to fetch: href="…" / src="…" in the HTML, and the href: '…' that tools.js
// builds at runtime. Absolute, data: and fragment URLs are not our assets.
var localRef = regexp.MustCompile(`(?:src|href)(?:=|:\s*)["']([^"']+)["']`)

// The production bug this guards. index.html is no-cache, the assets were
// max-age=3600, and the URLs carried no version — so a deploy that changed the HTML and
// the JS together served NEW HTML against an HOUR-OLD app.js: new buttons that did
// nothing, the old refresh interval still running, and a deploy that looked like it had
// silently failed until the cache expired. If the served markup ever again names an
// asset without a version token, this fails.
func TestServedUIVersionsEveryAssetItReferences(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	m := http.NewServeMux()
	a.Mount(m)

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s -> %d", path, w.Code)
		}
		return w
	}

	if assetVersion == "" {
		t.Fatal("no asset version was computed from the embedded UI")
	}

	// Both entry points must work and must be versioned: the plain directory URL and
	// the explicit filename. Both serve the same markup, so `checked` — the number of
	// local references found in it — is reused by the rewrite-count assertion below.
	checked := 0
	for _, entry := range []string{"/dashboard/", "/dashboard/index.html"} {
		w := get(entry)
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q; the HTML must not be cached, or it cannot hand out new asset URLs", entry, cc)
		}
		checked = 0
		for _, mm := range localRef.FindAllStringSubmatch(w.Body.String(), -1) {
			ref := mm[1]
			if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") || strings.Contains(ref, "://") {
				continue
			}
			checked++
			if !strings.Contains(ref, "?v="+assetVersion) {
				t.Errorf("%s references %q with no version token — a stale cached copy of it will be paired with this HTML", entry, ref)
				continue
			}
			// The versioned URL must actually serve the asset, not 404.
			if body := get("/dashboard/" + ref).Body; body.Len() == 0 {
				t.Errorf("%s serves an empty body", ref)
			}
		}
		// Fail loudly if the reference extraction stops finding anything: a reshaped
		// index.html that no longer matches must break this test, not quietly ship
		// unversioned URLs.
		if checked < 3 {
			t.Errorf("%s: found only %d local asset references; index.html references at least style.css, app.js and tools.js", entry, checked)
		}
	}

	// tools.css is fetched by tools.js, not by the HTML, and it goes stale the same
	// way. The rewrite covers any text asset, so the reference inside the served
	// script is versioned too.
	js := get("/dashboard/tools.js").Body.String()
	if !strings.Contains(js, "'tools.css?v="+assetVersion+"'") {
		t.Error("tools.js still asks for an unversioned tools.css")
	}

	// Versioned URLs are what make the hour of caching safe; keep it.
	js2 := get("/dashboard/app.js?v=" + assetVersion)
	if cc := js2.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("app.js Cache-Control = %q; want public, max-age=3600", cc)
	}
	// http.ServeContent types the response from the name it is given; a script served as
	// text/plain is not executed.
	if ct := js2.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q; want a javascript type", ct)
	}

	// Every rewrite has to be one this test knows about. The match in ui.go is
	// deliberately blind to context — it has to be, because references live in an HTML
	// attribute AND in a JS object literal — so a quoted asset name that is NOT a
	// reference gets versioned too, silently. Totalling the rewrites and comparing them
	// with the references enumerated above turns that into a red test: index.html's
	// references, plus the one tools.js builds.
	rewrites := 0
	for _, b := range versionedUI {
		rewrites += strings.Count(string(b), "?v="+assetVersion)
	}
	if want := checked + 1; rewrites != want {
		t.Errorf("versionedUI rewrote %d references but this test accounts for %d — either a quoted asset name that is not a reference is being versioned, or a reference is no longer found", rewrites, want)
	}
}

// The token has to follow the BYTES, or a dirty rebuild at the same commit serves new
// assets on old URLs.
func TestAssetVersionFollowsAssetBytes(t *testing.T) {
	// Attribute order, quoting style and element are all different from the real
	// index.html: the rewrite must not depend on the shape of the markup.
	markup := `<link href='style.css' rel=stylesheet><script defer src="app.js"></script>`
	build := func(js string) (string, string) {
		v, out := versionFS(fstest.MapFS{
			"index.html": {Data: []byte(markup)},
			"style.css":  {Data: []byte("body{}")},
			"app.js":     {Data: []byte(js)},
		})
		if v == "" {
			t.Fatal("versionFS computed no version")
		}
		return v, string(out["index.html"])
	}

	v1, html1 := build("let a = 1")
	for _, want := range []string{"'style.css?v=" + v1 + "'", `"app.js?v=` + v1 + `"`} {
		if !strings.Contains(html1, want) {
			t.Errorf("rewritten HTML is missing %s: %s", want, html1)
		}
	}

	v2, html2 := build("let a = 2")
	if v2 == v1 {
		t.Fatalf("app.js changed but the version did not: %s", v1)
	}
	if strings.Contains(html2, v1) {
		t.Errorf("rewritten HTML still carries the old version %s: %s", v1, html2)
	}

	// Rewriting is idempotent: an already-versioned reference is not versioned twice.
	if _, again := versionFS(fstest.MapFS{
		"index.html": {Data: []byte(html2)},
		"style.css":  {Data: []byte("body{}")},
		"app.js":     {Data: []byte("let a = 2")},
	}); strings.Count(string(again["index.html"]), "?v=") != 2 {
		t.Errorf("re-versioning doubled up the tokens: %s", again["index.html"])
	}

	// The name of every file goes into the hash, not only the bytes. Two builds whose
	// concatenated bytes are IDENTICAL must still get different tokens: a rename, and the
	// same bytes split differently across two adjacent names. Hashing the bytes alone
	// cannot tell either pair apart.
	//
	// Both pairs below are decided by the NUL-delimited NAME alone; neither exercises the
	// %d length that ui.go also writes. The length is there for injectivity, not for these
	// cases — without it {"a": "b.js\x00xy"} and {"a": "", "b.js": "xy"} hash the same —
	// and closing that would need a third pair differing only in a length. Keep the
	// length; it is not this test that justifies it.
	ver := func(files fstest.MapFS) string {
		v, _ := versionFS(files)
		if v == "" {
			t.Fatal("versionFS computed no version")
		}
		return v
	}
	if a, b := ver(fstest.MapFS{"a.js": {Data: []byte("xy")}}),
		ver(fstest.MapFS{"b.js": {Data: []byte("xy")}}); a == b {
		t.Errorf("renaming the only asset left the version at %s", a)
	}
	if a, b := ver(fstest.MapFS{"a.js": {Data: []byte("xy")}, "b.js": {Data: []byte("")}}),
		ver(fstest.MapFS{"a.js": {Data: []byte("x")}, "b.js": {Data: []byte("y")}}); a == b {
		t.Errorf("moving a byte between two assets left the version at %s", a)
	}

	// An asset in a SUBDIRECTORY used to switch versioning off for everything: the old
	// fs.Glob(fsys, "*") returned the directory as a name, reading it failed, and
	// versionFS returned no assets — so the whole dashboard fell back to unversioned
	// URLs at max-age=3600, which is the bug this file exists to prevent.
	v, out := versionFS(fstest.MapFS{
		"index.html":   {Data: []byte(`<script src="sub/extra.js"></script>`)},
		"sub/extra.js": {Data: []byte("let a = 1")},
	})
	if v == "" {
		t.Fatal("a subdirectory turned versioning off entirely")
	}
	if want := `"sub/extra.js?v=` + v + `"`; !strings.Contains(string(out["index.html"]), want) {
		t.Errorf("a subdirectory asset was left unversioned, want %s in: %s", want, out["index.html"])
	}
}
