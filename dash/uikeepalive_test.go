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
	if !strings.Contains(body, "rec.lo_usd") || !strings.Contains(body, "rec.hi_usd") {
		t.Error("the recommendation does not render an interval")
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
