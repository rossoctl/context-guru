package dash

import (
	"regexp"
	"strings"
	"testing"
)

var cssVarUse = regexp.MustCompile(`var\((--[a-z0-9-]+)`)
var cssVarDef = regexp.MustCompile(`(--[a-z0-9-]+)\s*:`)

// TestInsightsStylesheetUsesOnlyTokensThatExist is the cheap guard against the one CSS failure that
// is invisible: `var(--does-not-exist)` does not error, it resolves to nothing. A renamed token in
// style.css would silently strip a colour, a space or a radius from this view, and the view would
// still render — just wrong, in a way only a human looking at it would notice.
func TestInsightsStylesheetUsesOnlyTokensThatExist(t *testing.T) {
	style := readUI(t, "ui/style.css")
	defined := map[string]bool{}
	for _, m := range cssVarDef.FindAllStringSubmatch(style, -1) {
		defined[m[1]] = true
	}
	ins := readUI(t, "ui/insights.css")
	// A feature stylesheet may define its OWN tokens — this one declares a five-step sequential
	// ramp, because a heatmap needs a ramp and style.css has no reason to carry one for a single
	// view. Those count as defined; what must not happen is a reference to a token NOBODY
	// defines, which resolves to nothing and renders as a missing colour.
	for _, m := range cssVarDef.FindAllStringSubmatch(ins, -1) {
		defined[m[1]] = true
	}
	var used int
	for _, m := range cssVarUse.FindAllStringSubmatch(ins, -1) {
		used++
		if !defined[m[1]] {
			t.Errorf("insights.css uses %s, which style.css does not define. An undefined custom "+
				"property resolves to nothing rather than erroring, so this renders as a missing "+
				"colour or space that no test would otherwise catch", m[1])
		}
	}
	if used == 0 {
		t.Error("insights.css uses no tokens at all, so it cannot be following the page's theme")
	}
}

// TestInsightsStylesheetHasNoLiteralColours pins theme-awareness. The dashboard re-declares its
// tokens for dark mode, so a stylesheet built from tokens needs no dark rules of its own — and a
// literal hex is the one thing that would not follow, giving one theme's text on the other theme's
// background.
func TestInsightsStylesheetHasNoLiteralColours(t *testing.T) {
	ins := readUI(t, "ui/insights.css")
	// A line that DEFINES a token may carry a literal — that is what a definition is, and this
	// file declares its own sequential ramp. What must not carry one is a line that STYLES
	// something, because a literal there cannot follow the theme. The ramp's own dark values are
	// declared under both dark selectors, which is the mechanism that makes it theme-aware.
	defining := regexp.MustCompile(`^\s*--[a-z0-9-]+\s*:`)
	for i, line := range strings.Split(ins, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "*") || strings.Contains(line, "/*") {
			continue // the header comment explains the rule and may name a colour
		}
		if defining.MatchString(line) {
			continue
		}
		if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).MatchString(line) {
			t.Errorf("insights.css:%d has a literal colour: %q. Every colour must come from a "+
				"token, or it will not follow the dark theme", i+1, strings.TrimSpace(line))
		}
	}
}

// TestInsightsAssetsAreServedAndVersioned pins the wiring that makes the view loadable at all.
// uiHandler serves from the embedded FS and versionAssets rewrites sibling references to carry a
// content hash; an asset outside that set is one a deploy can serve stale, which is the exact bug
// versionAssets exists to prevent — new markup running old code.
func TestInsightsAssetsAreServedAndVersioned(t *testing.T) {
	for _, name := range []string{"insights.css", "insights.js"} {
		if _, ok := versionedUI[name]; !ok {
			t.Errorf("%s is not in versionedUI, so it is served at an unversioned URL and a "+
				"deploy can hand a browser new HTML against an hour-old copy of it", name)
		}
		if _, err := uiFS.ReadFile("ui/" + name); err != nil {
			t.Errorf("ui/%s is not embedded: %v", name, err)
		}
	}
}

// TestASelectionChangeDoesNotRebuildTheView pins a bug a user found by hand: every tick rebuilt
// #insights-body, so each one collapsed every open fold, dropped focus, and scrolled the reader
// down to the command block. Selecting a second item then meant re-opening the fold you were
// working in, which makes multi-select unusable — the reported symptom exactly.
//
// Asserted on the source rather than by driving a DOM, because the defect is structural: the
// selection path must not call the function that clears the host. A renderer test would need a
// browser; this needs only the shape of the call graph.
func TestASelectionChangeDoesNotRebuildTheView(t *testing.T) {
	js := readUI(t, "ui/insights.js")

	// insRender is the only thing allowed to replace the DOM, and only the loader may call it.
	// Two of the three matches are its own definition and the comment explaining this rule.
	if n := strings.Count(js, "insRender()"); n > 3 {
		t.Errorf("insRender() appears %d times. It clears #insights-body, so anything on the "+
			"selection path that calls it collapses every open fold and moves focus — route the "+
			"change through insSyncSelection instead", n)
	}
	// The selection path goes through the in-place sync.
	if !strings.Contains(js, "function insSetMany(names, on) {") ||
		!strings.Contains(js, "insSyncSelection();") {
		t.Error("insSetMany no longer routes through insSyncSelection, so a tick rebuilds the view")
	}
	// The review has its own host, or it cannot be refreshed without rebuilding its parent.
	for _, id := range []string{"insights-review", "insights-actions"} {
		if !strings.Contains(js, id) {
			t.Errorf("no %q host; the review and the buttons depend on the selection and need to "+
				"re-render without taking the inventory with them", id)
		}
	}
	// Focus is only restored when it was already inside the review. Restoring it unconditionally
	// would steal it from the checkbox the reader just ticked — the same bug, reversed.
	if !strings.Contains(js, "host.contains(document.activeElement)") {
		t.Error("the review re-render does not check where focus was, so it will either lose it " +
			"or steal it")
	}
}
