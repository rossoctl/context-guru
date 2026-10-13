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

// TestTheSparklineIsEmphasisNotTwoCategories pins the form choice, which is the decision that
// makes this chart readable rather than merely present.
//
// The calls are the SUBJECT and the carriage is CONTEXT. Drawn as two saturated series of equal
// weight, a reader compares them as peers — but the only question the row asks is "did the bars
// stop while the wash carried on". So the bars take the accent and the wash takes the same
// recessive ink as the grid, at low opacity.
//
// Asserted on the stylesheet because that is where the decision lives, and because the pull to
// "make the second series a real colour too" is strong and invisible in a screenshot of one row.
func TestTheSparklineIsEmphasisNotTwoCategories(t *testing.T) {
	css := readUI(t, "ui/insights.css")

	// The subject gets a validated chart hue.
	if !strings.Contains(css, ".ins-spark-call { fill: var(--pal-blue); }") {
		t.Error("the call bars no longer use --pal-blue. That pair was validated against the " +
			"chart surface for the lightness band, chroma floor, CVD separation and 3:1 " +
			"contrast in both modes — a hand-picked replacement has not been")
	}
	// The context stays recessive. A fill-opacity near 1 would make it a peer series.
	if !strings.Contains(css, ".ins-spark-carried { fill: var(--fg-faint); fill-opacity: .16; }") {
		t.Error("the carriage wash is no longer a low-opacity neutral. Two series of equal " +
			"weight invite a comparison the data does not support: carriage is the baseline the " +
			"calls are read against, not a second measure")
	}
	// A row with no series says so in words. A flat line would read as "never used", which is a
	// stronger and different claim than "before the window this axis can show".
	if !strings.Contains(css, ".ins-spark-none") {
		t.Error("no style for a row the axis cannot reach; drawing it as an empty chart asserts " +
			"something the store never measured")
	}
}
