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
	for i, line := range strings.Split(ins, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "*") || strings.Contains(line, "/*") {
			continue // the header comment explains the rule and may name a colour
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
