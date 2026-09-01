package dash

// Static checks over the keep-alive tab's own assets.
//
// Every one of them is a HONESTY property: a figure that reads as a measurement when it is an
// absence, a saving presented without the spend that bought it, a downside panel that only
// renders when the news is bad. All three are invisible on screen when they are wrong — the page
// looks completely fine — which is what earns a source-level assertion here, exactly as
// TestEveryInventoryTileHasAnExplanation does.

import (
	"regexp"
	"strings"
	"testing"
)

// Every tile on the new tab has its plain-English definition, how it was derived, and its catch.
//
// The same check TestEveryInventoryTileHasAnExplanation makes, applied to the keys this tab adds:
// this is the tab whose numbers authorise SPENDING somebody's money, so a figure nobody can
// interpret is worse here than anywhere else on the dashboard.
func TestEveryKeepAliveTileHasAnExplanation(t *testing.T) {
	src := readUI(t, "ui/app.js")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`tile\('(ka-[a-z0-9-]+)'`).FindAllStringSubmatch(src, -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("found no ka- tile() calls; this check needs rewriting against whatever replaced them")
	}
	for k := range keys {
		if !strings.Contains(src, "'"+k+"': {") {
			t.Errorf("tile %q has no TILE_INFO entry.\n"+
				"Every figure on this tab needs what it means, how it was derived and its "+
				"catch — these are the numbers somebody decides to spend money on.", k)
		}
	}
	// And the two disclosures that must be somewhere in the registry for the saving tiles:
	// the ceiling, and the "a zero may be an absence" state.
	for _, must := range []string{"CEILING", "content-keyed", "content, so"} {
		if strings.Contains(src, must) {
			return
		}
	}
	t.Error("no tile explanation says the keep-alive saving is a CEILING because the provider's " +
		"cache is keyed on content; that confound is the one thing this figure cannot measure")
}

// The downside panel renders UNCONDITIONALLY, with the worst-session figure and the
// service-wide split, and its copy is in the MARKUP rather than behind a branch.
//
// Because the tempting implementation is `if (net < 0) showTheDownside()`, and that is a page
// that tells the truth only to the people who already found out.
func TestTheLosingMajorityIsOnThePage(t *testing.T) {
	html := readUI(t, "ui/index.html")
	i := strings.Index(html, `id="ka-downside-panel"`)
	if i < 0 {
		t.Fatal("the downside panel is gone; this check needs rewriting")
	}
	end := strings.Index(html[i:], "</div>\n\n  <div class=\"panel\"")
	if end < 0 {
		end = len(html) - i
	}
	panel := html[i : i+end]
	// The service-wide figures, in the static markup: not fetched, not conditional, not
	// computed from the reader's own window — a reader whose own account looks fine still has
	// to meet the shape of the mechanism.
	for _, want := range []string{"34 of 119", "$2.42", "tax on most"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the downside panel's markup does not carry %q:\n%s", want, panel)
		}
	}
	// And it sits ABOVE the calculator, which is the whole information-architecture decision:
	// a page that led with the calculator would be selling.
	calc := strings.Index(html, "What would X and K buy me?")
	if calc < 0 {
		t.Fatal("the calculator panel is gone; this check needs rewriting")
	}
	if calc < i {
		t.Error("the calculator is above the downside panel. The verdict comes first and the " +
			"downside second, deliberately: 85 of the 119 sessions this touched lost money, " +
			"and a page that leads with what it could buy you is selling.")
	}
	src := readUI(t, "ui/app.js")
	// The renderer must not gate the panel on the sign of the net. It may add a banner when the
	// net is negative — that is the extra — but the split and the worst session are not behind
	// that branch.
	j := strings.Index(src, "function renderKADownside(")
	if j < 0 {
		t.Fatal("renderKADownside is gone; this check needs rewriting")
	}
	body := src[j : j+min(len(src)-j, 4000)]
	splitAt := strings.Index(body, "nps-bar")
	// The BRANCH, not any mention of the sign: colouring the headline by its sign is fine and
	// necessary. What must not happen is the split being built inside `if (net < 0) { ... }`.
	guardAt := strings.Index(body, "if (o.net_usd < 0)")
	if splitAt < 0 {
		t.Fatal("the winner/loser bar is not rendered")
	}
	if guardAt >= 0 && guardAt < splitAt {
		t.Error("the winner/loser split is rendered inside a negative-net branch; it must render " +
			"whatever the sign is")
	}
	// The only thing that branch may add is the way out.
	if guardAt < 0 || !strings.Contains(body[guardAt:], "Turn keep-alive off") {
		t.Error("a negative own net does not offer a way to switch the mechanism off")
	}
	// The status segments are labelled in WORDS as well as coloured — a status must never rest
	// on hue alone.
	if !strings.Contains(body, "Came out ahead") || !strings.Contains(body, "Paid for nothing") {
		t.Error("the winner/loser segments are not labelled in words beside their colour")
	}
}

// The recommendation UI never renders a hero number, and there is no point estimate for it to
// render: the payload does not carry one (TestRecommendationPayloadCarriesNoPointEstimate) and
// the renderer reads a RANGE.
func TestTheRecommendationRendersARangeAndRefusesThinHistory(t *testing.T) {
	src := readUI(t, "ui/app.js")
	i := strings.Index(src, "async function loadKARecommend(")
	if i < 0 {
		t.Fatal("loadKARecommend is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 4000)]
	// The SEPARATOR is pinned, not merely the two field names. Asserting only that both names
	// appear leaves `usd((rec.lo_usd + rec.hi_usd) / 2)` — a single hero midpoint — passing, which
	// is precisely what this check exists to forbid, and it did pass: the whole dash package
	// stayed green with the tile rendering a point.
	if !regexp.MustCompile(`rec\.lo_usd[\s\S]{0,40}\x{2013}[\s\S]{0,40}rec\.hi_usd`).MatchString(body) {
		t.Error("the recommendation tile does not render lo and hi as a RANGE with a dash between " +
			"them. Two field names in the same function is not enough: a midpoint of the two " +
			"mentions both and is a hero number, which is the one thing this payload has no " +
			"point estimate on the wire in order to prevent")
	}
	if !strings.Contains(body, "rec.refused") {
		t.Error("the recommendation has no refusal branch; below 20 addressable expiries a real " +
			"saving cannot be told from noise, and rounding that off is the sixth time this " +
			"project would have concluded a difference from too small an n")
	}
	if !strings.Contains(body, "factor of two") {
		t.Error("the honest sentence — the direction is resolvable, the size is not — is not on " +
			"the page")
	}
	// Nothing is auto-applied: the button fills the Settings fields and leaves them unsaved.
	if strings.Contains(body, "method: 'PUT'") || strings.Contains(body, `method: "PUT"`) {
		t.Error("the recommendation panel SAVES. A recommendation whose own interval is 62% wide " +
			"is not something a service applies on somebody's behalf; it fills the fields in.")
	}
	if !strings.Contains(body, "Nothing is saved until you press Save") {
		t.Error("the apply button does not say that nothing is saved")
	}
}

// The keep-alive tab respects the time-range filter, so it must NOT be in UNFILTERED_VIEWS —
// every figure on it is "over this window" and a tab that ignored the picker above it would be
// answering a different question from the one on screen.
func TestTheKeepAliveTabRespectsTheTimeRange(t *testing.T) {
	src := readUI(t, "ui/app.js")
	i := strings.Index(src, "const UNFILTERED_VIEWS")
	if i < 0 {
		t.Fatal("UNFILTERED_VIEWS is gone; this check needs rewriting")
	}
	line := src[i : i+strings.Index(src[i:], "\n")]
	if strings.Contains(line, "keepalive") {
		t.Errorf("the keep-alive tab is in UNFILTERED_VIEWS: %s", line)
	}
	// And it is wired into the loader table, or the tab renders nothing at all.
	if !strings.Contains(src, "keepalive: loadKeepAlive") {
		t.Error("the keep-alive view has no loader")
	}
	if !strings.Contains(readUI(t, "ui/index.html"), `data-view="keepalive"`) {
		t.Error("there is no keep-alive tab button")
	}
}

// No new hue, no pie, no gauge, no second chart library. The dashboard has a fixed four-hue
// categorical order, a status palette and two chart primitives; a fifth series colour means a
// chart needs splitting, not a new colour.
func TestTheKeepAliveTabAddsNoNewChartVocabulary(t *testing.T) {
	src := readUI(t, "ui/app.js")
	i := strings.Index(src, "async function loadKeepAlive(")
	if i < 0 {
		t.Fatal("loadKeepAlive is gone; this check needs rewriting")
	}
	tab := src[i:]
	// Matched against CODE rather than the whole text: the prose on this tab names the forms it
	// deliberately does not use ("never a hero number and never a gauge"), and a check that
	// cannot tell a comment from a call would forbid saying so.
	code := stripJSComments(tab)
	for _, bad := range []string{"--s5", "var(--s5)", "pieChart", "treemap", "gauge", "Chart.js",
		"d3.", "conic-gradient"} {
		if strings.Contains(code, bad) {
			t.Errorf("the keep-alive tab introduces %q. The form heuristic maps every panel here "+
				"onto stat tiles, barRows, lineChart and the status bar; anything else is a new "+
				"vocabulary for the reader to learn.", bad)
		}
	}
	// Two charts and never two y-axes: a count and a dollar figure share no scale.
	if strings.Count(code, "lineChart(") < 2 {
		t.Error("the per-day panel does not draw two separate charts; a count and a dollar " +
			"figure on one pair of axes is a dual-axis chart, which is the single most common " +
			"way a chart implies a relationship it has not measured")
	}
	if strings.Contains(code, "y2") || strings.Contains(code, "rightAxis") {
		t.Error("a second y-axis appeared on the keep-alive tab")
	}
}

// stripJSComments removes // and /* */ comments, crudely but adequately: this file's own prose
// discusses the chart types it refuses to use, and a substring check over the comments would
// forbid documenting the decision.
func stripJSComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 2
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// No UI script may declare a top-level binding named after a window method.
//
// dash/ui/*.js are CLASSIC scripts, so they share one global scope, and a top-level
// `const prompt = {...}` in tools.js creates a binding in the global declarative
// environment that shadows `window.prompt` for every bare `prompt(...)` in every other
// script on the page — app.js included, regardless of load order, because the reference is
// resolved at call time. That shipped: the "Keep warm" button and the "copy this token now"
// dialog both threw `prompt is not a function` and failed SILENTLY, which is the only
// reason it survived review.
//
// The declaration and the call site are in different files, so nothing local to either one
// can catch this. It is a whole-bundle property, and this is the cheapest place to assert it.
func TestNoUIScriptShadowsAWindowMethod(t *testing.T) {
	// The window methods a classic script can plausibly call bare. Not exhaustive by
	// design — it is the set whose shadowing would be silent, which is the dangerous kind.
	shadowable := []string{
		"prompt", "alert", "confirm", "open", "close", "print", "focus", "blur", "stop",
		"find", "scroll", "scrollTo", "scrollBy", "postMessage", "fetch", "atob", "btoa",
		"setTimeout", "setInterval", "clearTimeout", "clearInterval", "queueMicrotask",
		"requestAnimationFrame", "cancelAnimationFrame", "structuredClone", "reportError",
		"getComputedStyle", "matchMedia", "getSelection", "name", "status", "length",
	}
	decl := regexp.MustCompile(`(?m)^(?:const|let|var|function|class)\s+([A-Za-z_$][\w$]*)`)
	// Every classic script on the page: the shadow this guards against is a whole-bundle
	// property "in any load order", so a file left out of the loop is a file that can
	// introduce one freely. campaigns.js is the pointed case — it calls bare confirm() and
	// alert(), both on the list below.
	for _, f := range []string{"ui/app.js", "ui/tools.js", "ui/kvcache.js", "ui/campaigns.js"} {
		src := stripJSComments(readUI(t, f))
		for _, m := range decl.FindAllStringSubmatch(src, -1) {
			for _, bad := range shadowable {
				if m[1] != bad {
					continue
				}
				t.Errorf("%s declares a top-level %q, which shadows window.%s for every bare "+
					"%s(...) call in every classic script on the page, in any load order.\n"+
					"Rename it (promptView, openPanel, …). A shadow like this fails silently: "+
					"the control just does nothing.", f, bad, bad, bad)
			}
		}
	}
}

// The K rule's copy states its REACH, and the two refuted numbers may not come back.
//
// The page used to justify "one ping is never suggested" with "it reaches about 4.7 minutes" and
// "it is $71 worse than nothing". Both are the K*X coverage error: coverage is K*X + TTL, because
// the last ping is itself a cache read and a read refreshes the entry — which is the arithmetic
// CoverageSeconds carries a warning about, and which the page's own K-ladder prints correctly
// three panels away. Under correct coverage one ping is +$101.56 in the adjudicated sweep, not
// -$71, so the page was contradicting itself AND arguing from a refuted figure.
//
// Pinned against CoverageSeconds itself, so reverting the reach to K*X fails here rather than
// only in a review: at X=280 the wrong arithmetic gives 4.7 and 9.3 minutes where the right one
// gives 9.7 and 14.3.
func TestTheKRuleCopyStatesItsReachAndNotTheRefutedNumbers(t *testing.T) {
	src := readUI(t, "ui/app.js")
	for _, dead := range []string{"4.7 minute", "$71 worse", "71 worse than"} {
		if strings.Contains(src, dead) {
			t.Errorf("the refuted figure %q is back in the tab's copy. Coverage is K*X + TTL; "+
				"one ping reaches 9.7 minutes at X=280 and is a smaller win, not a loss", dead)
		}
	}
	for k, want := range map[int]string{1: "9.7", 2: "14.3"} {
		if got := trimZero(CoverageSeconds(280, k) / 60); got != want {
			t.Fatalf("CoverageSeconds(280,%d) is %s minutes, not %s — this check's expected "+
				"strings need rewriting along with the copy", k, got, want)
		}
		if !strings.Contains(src, want) {
			t.Errorf("the copy does not state the K=%d reach of %s minutes; the rule has to be "+
				"argued from the coverage the code actually enforces", k, want)
		}
	}
}

// The live panel's three honesty properties, in the markup and in the renderer.
//
// Each one is invisible on screen when it is wrong — the page looks completely fine — which is
// what earns a source-level assertion, exactly as the tiles' explanations do:
//
//   - the TTL shown is the tier the provider BILLED, not one read off configuration. A
//     `ttl: "1h"` request on a model that does not support it returns a normal 200 with a
//     five-minute entry, so configuration is not evidence about what is in force.
//   - the potential saving is a CEILING and says so. It is only spent if the session resumes
//     after its entry has gone, and the provider's cache is content-keyed.
//   - the countdown is computed against the SERVER's clock, which is on the wire for that reason.
func TestTheLivePanelSaysWhatItsNumbersAre(t *testing.T) {
	html := readUI(t, "ui/index.html")
	if !strings.Contains(html, `data-testid="ka-live-table"`) {
		t.Fatal("the live-sessions table is gone; this check needs rewriting")
	}
	note := html[strings.Index(html, `data-testid="ka-live-note"`):]
	note = note[:strings.Index(note, "</p>")]
	if !strings.Contains(note, "BILLED") {
		t.Error("the live panel does not say the lifetime it shows is the tier the provider " +
			"BILLED. A `ttl: \"1h\"` request on a model that does not grant it comes back a " +
			"normal 200 with a 5-minute entry, so configuration is not evidence")
	}
	if !strings.Contains(note, "START") {
		t.Error("the live panel does not say the lifetime runs from each request's START. " +
			"Anchoring it at the response instead is the single change that flips the sign of " +
			"this whole feature, and a countdown that gets it wrong is over-optimistic by the " +
			"length of the response")
	}
	src := readUI(t, "ui/app.js")
	i := strings.Index(src, "function renderKALive(")
	if i < 0 {
		t.Fatal("renderKALive is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 6000)]
	if !strings.Contains(body, "CEILING") {
		t.Error("the expiring-soon warning states a dollar figure without calling it a CEILING. " +
			"It is only spent if the session resumes AFTER its entry lapsed, and the provider's " +
			"cache is keyed on content, so another session sending the same prefix refreshes it " +
			"for nothing")
	}
	if !strings.Contains(body, "ABSENCE") {
		t.Error("the empty state does not distinguish an absence from a zero saving; every other " +
			"coverage state on this tab does")
	}
	// The countdown must come from the server's clock, and the field has to be read for that to
	// mean anything.
	if !strings.Contains(body, "live.soon_seconds") || !strings.Contains(src, "'keepalive/live'") {
		t.Error("the live panel does not read the server's own threshold; a countdown driven by " +
			"the browser's clock reads \"2 minutes left\" on an entry that expired ten ago")
	}
	if !strings.Contains(body, "mySession(r)") {
		t.Error("the arm button is drawn without checking the row is the caller's own. An override " +
			"is keyed to the principal that arms it, so on a manager's service-wide view every " +
			"button would return 200 and keep nothing warm")
	}
}

// Every table with a manager-only scope column marks it with the attribute showScopeCol actually
// reads, and every table that calls showScopeCol has one.
//
// NOT keep-alive-specific — it lives here because it is the same kind of whole-bundle static check
// as TestNoUIScriptShadowsAWindowMethod, and because no test covered the scope column for ANY
// table. The live panel shipped its header as `data-ka-live-scope` while showScopeCol queries
// `thead th[data-scope-col]`, so the attribute was read by nothing: the TENANT header stayed
// permanently hidden while the row renderer still emitted a visible TENANT cell whenever
// wideScope() was true — the DEFAULT view for a manager. Eleven cells under ten visible headers,
// and every column after the hidden one shifted left: ONE LAPSE showed the ping price, PINGS PER
// LAPSE showed a dollar, REALISED showed the breakeven. Two dollar figures reading as each other,
// on the one panel whose whole thesis is not mislabelling dollars.
//
// The attribute name is EXTRACTED from showScopeCol rather than written down here, so renaming it
// in app.js cannot leave this check asserting a stale spelling.
func TestEveryScopeColumnUsesTheAttributeShowScopeColReads(t *testing.T) {
	app, html := readUI(t, "ui/app.js"), readUI(t, "ui/index.html")
	m := regexp.MustCompile(`thead th\[([a-z-]+)\]`).FindStringSubmatch(app)
	if m == nil {
		t.Fatal("showScopeCol's selector no longer looks like `thead th[attr]`; this check needs " +
			"rewriting against whatever replaced it")
	}
	attr := m[1]

	// Every table told to toggle a scope column must actually have one, under that attribute.
	calls := regexp.MustCompile(`showScopeCol\('\[data-testid=["']?([a-z-]+)["']?\]'`).FindAllStringSubmatch(app, -1)
	if len(calls) == 0 {
		t.Fatal("no showScopeCol call sites found; this check needs rewriting")
	}
	for _, c := range calls {
		// The QUOTED attribute, not the bare id: `sessions-table` is a substring of
		// `ka-sessions-table`, so a bare search resolves by file order and would silently inspect
		// the wrong table's thead if the panels were ever reordered — and PASS, because both carry
		// the attribute. A check that passes for the wrong reason is worse than no check.
		i := strings.Index(html, `data-testid="`+c[1]+`"`)
		if i < 0 {
			t.Errorf("showScopeCol is called for table %q, which is not in index.html", c[1])
			continue
		}
		end := strings.Index(html[i:], "</thead>")
		if end < 0 || !strings.Contains(html[i:i+end], attr) {
			t.Errorf("table %q calls showScopeCol but no <th> in its thead carries %q, so the "+
				"column can never be revealed — while the row renderer still emits the cell. "+
				"Every body row would then have one more cell than there are visible headers, and "+
				"every column after it reads as its neighbour.", c[1], attr)
		}
	}
	// And no header may mark itself with a DIFFERENT scope-ish attribute: that is the defect
	// itself, and it is invisible because a `hidden` <th> that is never un-hidden looks deliberate.
	for _, bad := range regexp.MustCompile(`data-[a-z-]*scope[a-z-]*`).FindAllString(html, -1) {
		if bad != attr {
			t.Errorf("index.html marks a scope column %q, but showScopeCol only reads %q, so that "+
				"header stays hidden forever. Use %q.", bad, attr, attr)
		}
	}
}
