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

// TestRemovableWasteReadsAsTheProblemAndKeepDoesNot pins the direction of the verdict palette,
// which was inverted and shipped that way.
//
// `remove` was GREEN and `keep` was RED, so the one line in the report a reader is meant to act on
// — money being spent on a capability nothing invokes — was coloured as good news, while a
// built-in they cannot remove and should not worry about was coloured as an alarm. Reported from
// use: "tools that can be removed should be in red even if its waste money".
//
// Asserted on the stylesheet because that is where the decision lives, and because the pull to
// re-green it is strong: "removable" does feel like good news from the perspective of the saving.
// It is not the perspective the reader has. They are looking for what to fix.
func TestRemovableWasteReadsAsTheProblemAndKeepDoesNot(t *testing.T) {
	css := readUI(t, "ui/insights.css")

	// The three verdict colours are declared as tokens, because the CHART reads these same
	// values out of the stylesheet so a cell and its table pill cannot disagree. Assert the
	// token's DEFINITION rather than its use site: a rule that says var(--ins-vd-remove) tells
	// you nothing about which hue that is.
	root := cssRuleFor(t, css, ":root")
	for _, want := range []struct{ token, hue, why string }{
		{"--ins-vd-remove", "var(--bad)",
			"removable waste is what the reader came to find, so it carries the alarm colour"},
		{"--ins-vd-consider", "var(--warn)",
			"unused but under the evidence floor is a question, not a finding"},
	} {
		if !strings.Contains(root, want.token+":") {
			t.Fatalf("%s is no longer declared in :root; the chart reads it from here", want.token)
		}
		decl := want.token + ":"
		at := strings.Index(root, decl)
		line := root[at : at+min(len(root)-at, 80)]
		if cut := strings.Index(line, ";"); cut > 0 {
			line = line[:cut]
		}
		if !strings.Contains(line, want.hue) {
			t.Errorf("%s = %q; want %s — %s", want.token, line, want.hue, want.why)
		}
		if strings.Contains(line, "var(--good)") {
			t.Errorf("%s is green (%q). This is the inversion that shipped: the saving is good "+
				"news, the SPEND is not, and the reader is looking at the spend.", want.token, line)
		}
	}
	// `stale` is the same instruction as `remove` with better evidence, so it is the same colour.
	for _, vd := range []string{"remove", "stale"} {
		line := cssRuleFor(t, css, ".ins-vd."+vd)
		if !strings.Contains(line, "var(--ins-vd-remove)") {
			t.Errorf(".ins-vd.%s does not use --ins-vd-remove: %q", vd, line)
		}
	}

	// And the verdict that means "you cannot remove this" must not be the alarm, or the alarm
	// means nothing: built-ins are the largest group in the inventory.
	//
	// Checked at the TOKEN, not at the rule. Asserting that `.ins-vd.keep` does not mention
	// var(--bad) proved nothing once the rule started going through var(--ins-vd-keep): a
	// mutation setting the token itself to --bad survived the test. Found by mutation testing,
	// which is the only reason this line reads the way it does.
	keepTok := strings.Index(root, "--ins-vd-keep:")
	if keepTok < 0 {
		t.Fatal("--ins-vd-keep is no longer declared in :root")
	}
	keepLine := root[keepTok:]
	if cut := strings.Index(keepLine, ";"); cut > 0 {
		keepLine = keepLine[:cut]
	}
	if strings.Contains(keepLine, "var(--bad)") {
		t.Errorf("--ins-vd-keep is the alarm colour (%q)\nA built-in is not a problem — it is "+
			"weight the agent needs. Colouring the largest group in the inventory red trains the "+
			"reader to ignore red.", keepLine)
	}
	if keep := cssRuleFor(t, css, ".ins-vd.keep"); strings.Contains(keep, "var(--bad)") {
		t.Errorf(".ins-vd.keep uses --bad directly: %q", keep)
	}
	// Same for the owner block, which tinted an entire keep-group red.
	own := cssRuleFor(t, css, ".ins-own.keep .ins-head")
	if strings.Contains(own, "var(--bad-soft)") {
		t.Errorf(".ins-own.keep .ins-head still washes the whole block in --bad-soft: %q", own)
	}
}

// cssRuleFor returns the first rule body for a selector, so an assertion reads one declaration
// block rather than the whole stylesheet.
func cssRuleFor(t *testing.T, css, selector string) string {
	t.Helper()
	i := strings.Index(css, selector)
	if i < 0 {
		t.Fatalf("selector %q is not in the stylesheet", selector)
	}
	open := strings.Index(css[i:], "{")
	end := strings.Index(css[i:], "}")
	if open < 0 || end < 0 || end < open {
		t.Fatalf("selector %q has no declaration block", selector)
	}
	return css[i+open : i+end]
}

// TestTheChartAndTheTablesSpeakOneColourLanguage is the test that stops the two surfaces drifting.
//
// They had drifted: a built-in read red in the tables and neutral-faint in the chart; a removable
// item read green in the tables and sat on a blue magnitude ramp in the chart. Same item, three
// different colours, so the picture could not be checked against the table beneath it --
// "if i see build in tools in red in tables in chart it should be same color".
//
// The fix is structural rather than a matched pair of literals: the chart reads the SAME custom
// properties the table pills are painted with, so there is one definition and nothing to keep in
// sync. This asserts that structure, because a test comparing two hard-coded hues would pass
// while both were wrong.
func TestTheChartAndTheTablesSpeakOneColourLanguage(t *testing.T) {
	css := readUI(t, "ui/insights.css")
	js := readUI(t, "ui/insights.js")

	// Every verdict colour is declared once, in the stylesheet, and READ by the chart rather than
	// copied into it.
	for _, tok := range []string{"--ins-vd-remove", "--ins-vd-consider", "--ins-vd-keep"} {
		if !strings.Contains(css, tok+":") {
			t.Errorf("%s is not declared in insights.css; the chart has nowhere to read it from",
				tok)
		}
		// READ, not merely mentioned. `strings.Contains(js, tok)` is not enough: a chart that
		// hardcodes '#b02a1e' still mentions the token in its fallback argument, and that mutant
		// survived this test — the exact drift it exists to catch, since the stylesheet could
		// then change while the chart kept the old hue. So require the lookup form.
		if !strings.Contains(stripJSComments(js), "v('"+tok+"'") {
			t.Errorf("insights.js does not LOOK UP %s (expected v('%s', …)); the chart is "+
				"painting that verdict with a colour of its own, which is how the chart and the "+
				"tables came to disagree in the first place", tok, tok)
		}
	}

	// Read CODE, not prose. These assertions are about what the chart is configured to do, and
	// the file explains its own history at length — an earlier version of this test failed on the
	// comment that says why the visualMap was REMOVED, which is the opposite of the finding.
	code := stripJSComments(js)

	// The blue magnitude ramp must be gone from the cells: it encoded "how many invocations" in a
	// hue that meant nothing in the tables, and that is the channel the verdict now needs.
	if strings.Contains(code, "visualMap") {
		t.Error("insights.js still configures a visualMap. The cell hue now carries the VERDICT, " +
			"so a magnitude ramp would put two unrelated meanings in one channel")
	}

	// Every verdict the shaping layer can emit must have somewhere to be painted, or its rows
	// fall back to a default and silently read as something else. Checked as a BUCKET rather than
	// a quoted literal, because the verdict names reach the chart as object keys.
	for _, vd := range []string{"remove", "stale", "consider", "keep"} {
		if !strings.Contains(code, "byVd."+vd) && !strings.Contains(code, vd+": [") {
			t.Errorf("the chart has no bucket for the %q verdict; rows carrying it will be "+
				"painted as something else", vd)
		}
	}
	// And each bucket is actually drawn, with a colour taken from the theme.
	for _, vd := range []string{"remove", "consider", "keep"} {
		if !strings.Contains(code, "t.vd."+vd) {
			t.Errorf("no series paints the %q bucket with t.vd.%s, so its cells are not carrying "+
				"the verdict colour the tables use", vd, vd)
		}
	}
}

// TestTheChartDoesNotHideTheWasteItIsAboout guards against the tautology that shipped: the chart
// drew a row only for a capability invoked at least once, and an invoked capability is `keep` by
// definition -- so every drawn row was grey, every time, whatever the palette said. Reported as
// "why all come grey".
//
// Worse than monochrome: the rows it dropped were exactly the ones this page exists to show. On
// the measured store, 15 ever-invoked rows were drawn and 43 never-invoked ones -- the actual
// waste -- were filtered out, so the chart showed what the reader USES and hid what they are
// paying for and never touch.
//
// Asserted on the source because there is no DOM here. The rule: insRollup must not drop a row
// for having no invocations.
func TestTheChartDoesNotHideTheWasteItIsAbout(t *testing.T) {
	js := stripJSComments(readUI(t, "ui/insights.js"))

	i := strings.Index(js, "function insRollup")
	if i < 0 {
		t.Fatal("insRollup is gone; it is the function that builds the chart's rows")
	}
	end := strings.Index(js[i:], "\n}")
	if end < 0 {
		t.Fatal("cannot find the end of insRollup")
	}
	body := js[i : i+end]

	// The filter that caused it. Any form of "skip this row when it was never called".
	for _, bad := range []string{
		"if (!calls.some((v) => v > 0)) continue",
		"if (!calls.some(v => v > 0)) continue",
	} {
		if strings.Contains(body, bad) {
			t.Errorf("insRollup still drops never-invoked rows (%s).\nThose rows ARE the waste: "+
				"a capability carried on every request and never called is the finding this page "+
				"reports. Dropping them leaves only `keep` rows, so the chart is grey by "+
				"construction and the money is invisible.", bad)
		}
	}
	// And it must still draw a row that was only ever CARRIED, which is the positive form of the
	// same rule -- a mutation that drops the rows some other way has to fail too.
	if !strings.Contains(body, "carried") {
		t.Error("insRollup no longer reads the carried series, so a carried-but-never-invoked " +
			"capability cannot appear in the chart at all")
	}
}
