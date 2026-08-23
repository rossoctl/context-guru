package dash

import (
	"regexp"
	"strings"
	"testing"
)

// uiScripts are the dashboard's scripts, which index.html loads with a plain <script src>
// — CLASSIC scripts, not modules. That is the whole reason this file exists: classic
// scripts do not get a scope each, they SHARE one global lexical scope, so a top-level
// declaration in either one is visible to both and to nothing else on the page.
var uiScripts = []string{"ui/app.js", "ui/tools.js"}

// topLevelDecl matches a binding declared at the start of a line, which in these files
// means module scope: every nested statement is indented, and JSDoc continuation lines
// begin with " *".
var topLevelDecl = regexp.MustCompile(
	`(?m)^(?:const|let|var|class|function|async function)\s+([A-Za-z_$][\w$]*)`)

// shadowableGlobals name the browser surface that a top-level UI binding must not reuse.
//
// The production bug. tools.js declared `const prompt = { state: 'idle', … }` for the
// Inventory tab's prompt-text cache. Because the two scripts share one global lexical
// scope, that const shadowed window.prompt for every BARE prompt() call in BOTH files —
// and only the bare form: window.prompt itself stayed a function, so app.js read
// correctly, the markup was correct, the endpoint was correct and every existing test
// passed. The only symptom was "TypeError: prompt is not a function", thrown inside
// whichever handler called it, which took out four call sites in app.js:
//
//   - Settings → "Mint a token" threw on its first statement: no dialog, no request, no
//     token. The button did nothing whatsoever.
//   - Tenants → "Reissue token" threw AFTER its POST had already succeeded, so the token
//     was minted and never displayed. Tokens are stored hashed, so every click left the
//     account one live credential that nobody holds.
//   - the keep-alive's "how many hours?" threw before it could ask, so arming a session
//     did nothing.
//   - the audit trail's "copy the document as it was before this save" is a clipboard
//     fallback, so only a browser that refuses clipboard access reached it.
//
// The names below are the globals these scripts call bare, plus the short noun-shaped
// window properties most likely to be reused for a cache or a state object. Go cannot
// enumerate Window, so this is a list and not a reflection of the platform: extend it if
// a new collision is ever found.
var shadowableGlobals = []string{
	// Called bare by the UI. prompt is the one that shipped broken.
	"prompt", "alert", "confirm",
	"fetch", "EventSource", "AbortController",
	"setTimeout", "setInterval", "clearTimeout", "clearInterval", "requestAnimationFrame",
	"URL", "URLSearchParams", "encodeURIComponent", "decodeURIComponent",
	"document", "window", "location", "history", "navigator", "localStorage", "sessionStorage",
	"Array", "Date", "Error", "Map", "Set", "String", "Number", "Object", "Promise",
	"Math", "JSON", "Intl", "Uint32Array", "isFinite", "parseInt", "parseFloat",
	// Not called here, but they are window properties with ordinary-noun names — exactly
	// the shape of a variable somebody reaches for next.
	"name", "status", "length", "origin", "event", "screen", "frames", "parent", "self",
	"top", "open", "close", "focus", "blur", "print", "stop", "find", "scroll",
}

// TestUIScriptsDoNotShadowBrowserGlobals is the guard for the defect described above: a
// top-level binding whose name the browser already defines, which silently disables that
// API for every bare call on the page.
func TestUIScriptsDoNotShadowBrowserGlobals(t *testing.T) {
	forbidden := make(map[string]bool, len(shadowableGlobals))
	for _, g := range shadowableGlobals {
		forbidden[g] = true
	}

	found := 0
	for _, script := range uiScripts {
		src := readUI(t, script)
		for _, m := range topLevelDecl.FindAllStringSubmatch(src, -1) {
			found++
			if forbidden[m[1]] {
				t.Errorf("%s declares a top-level %q, which shadows window.%s for every bare "+
					"%s(...) call in EVERY dashboard script — window.%s stays intact, so the only "+
					"symptom is a TypeError inside whichever handler calls it. Rename the binding.",
					script, m[1], m[1], m[1], m[1])
			}
		}
	}
	// A regex that silently stopped matching would make this test vacuously green.
	if found < 100 {
		t.Fatalf("found only %d top-level declarations across %v; the matcher is broken, "+
			"not the code", found, uiScripts)
	}
}

// TestDashboardUIScriptsShareOneGlobalScope pins the premise the test above rests on. If
// the dashboard ever moves to <script type="module">, each file gets its own scope, the
// shadowing hazard disappears, and shadowableGlobals can go — but until then a reader of
// tools.js has no local reason to think its top-level names reach app.js.
func TestDashboardUIScriptsShareOneGlobalScope(t *testing.T) {
	html := readUI(t, "ui/index.html")
	for _, script := range uiScripts {
		base := strings.TrimPrefix(script, "ui/")
		tag := `<script src="` + base + `"></script>`
		if !strings.Contains(html, tag) {
			t.Errorf("index.html no longer loads %s as a classic script (%s not found). If it is "+
				"now a module, the shadowing hazard above no longer applies and this file can go.",
				base, tag)
		}
	}
}
