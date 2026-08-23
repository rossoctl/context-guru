package dash

// Static checks over the KV-cache tab's own assets.
//
// Every one of them is an HONESTY property, and every one is invisible on screen when it is
// wrong — the page looks completely fine. That is what earns a source-level assertion here,
// exactly as TestEveryKeepAliveTileHasAnExplanation and TestEveryInventoryTileHasAnExplanation
// do for the two tabs before it.

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every tile on the tab has its plain-English definition, how it was derived, and its catch.
//
// This is the tab whose figures authorise changing a caching policy — i.e. changing somebody's
// bill — so a number nobody can interpret is worse here than almost anywhere else.
func TestEveryKVCacheTileHasAnExplanation(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`tile\('(kv-[a-z0-9-]+)'`).FindAllStringSubmatch(src, -1) {
		keys[m[1]] = true
	}
	if len(keys) == 0 {
		t.Fatal("found no kv- tile() calls; this check needs rewriting against whatever replaced them")
	}
	for k := range keys {
		if !strings.Contains(src, "'"+k+"': {") {
			t.Errorf("tile %q has no TILE_INFO entry.\n"+
				"Every figure on this tab needs what it means, how it was derived and its "+
				"catch — these are the numbers somebody changes a cache policy on.", k)
		}
	}
	// And the two disclosures every reader of this page needs: that a request with no successor
	// is excluded rather than counted as zero, and that an unpriced request is not a free one.
	for _, must := range []string{"no next request", "not zero", "not a free one"} {
		if !strings.Contains(src, must) {
			t.Errorf("no explanation on this tab says %q; that is the assumption every average "+
				"on it depends on", must)
		}
	}
}

// NOT ONE PRICE IN THE BROWSER.
//
// The recorded failure this guards: app.js once carried a hardcoded 3.00/3.75/0.30 per-MTok
// rate table, sonnet-class, on a deployment that bills opus — so every net figure derived from
// it was ~27% wrong in a direction nobody could see. This page is entirely about prices, which
// makes it the most likely place for that to happen again.
//
// Two assertions. First, no per-token rate literal: the scientific-notation forms a USD/token
// rate takes, and the two Anthropic cache multiples, used as ARITHMETIC rather than mentioned
// in prose (the tile explanations legitimately say "2.0x base input"). Second, no dollar field
// is ever an operand: every *_usd on this page is displayed, never computed with.
func TestTheKVCacheViewCarriesNoPriceOfItsOwn(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/kvcache.js"))
	for _, bad := range []string{"e-6", "e-7", "* 1.25", "1.25 *", "* 3.75", "3.75 *",
		"* 0.1", "0.1 *", "/ 1.25", "* 2.0", "2.0 *"} {
		if strings.Contains(code, bad) {
			t.Errorf("the KV-cache view computes with %q. Every rate belongs to the server: the "+
				"browser posts inputs and renders the answers it gets back.", bad)
		}
	}
	// A money field as an operand of + - * / — including a unary minus to flip a sign for a
	// colour, which is how the first draft of this page negated the cache premium.
	operand := regexp.MustCompile(`(?:[-+*/]\s*[A-Za-z0-9_.\[\]]*_usd\b)|(?:\b[A-Za-z0-9_.\[\]]*_usd\s*[-+*/])`)
	for _, m := range operand.FindAllString(code, -1) {
		t.Errorf("a dollar figure is used in arithmetic here: %q. Absolute savings, percentages "+
			"and the cache premium are all computed server-side, in one place, from the rates "+
			"that were actually applied.", strings.TrimSpace(m))
	}
}

// The cost formulas are PRINTED, not retyped.
//
// A formula written into a JavaScript template is a second definition of the arithmetic and
// nothing tests it against the first. They arrive in the payload, so the page shows the
// expressions the server actually evaluated.
func TestTheCostFormulasAreRenderedFromThePayload(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	i := strings.Index(src, "function renderKVAssumptions(")
	if i < 0 {
		t.Fatal("renderKVAssumptions is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 3000)]
	for _, must := range []string{"a.formulas", "f.formula", "f.note", "a.notes", "a.time_zone"} {
		if !strings.Contains(body, must) {
			t.Errorf("the assumptions panel does not render %s from the payload", must)
		}
	}
	// And the page states its timezone, because there is no per-user one in the store and a
	// time-of-day analysis without a zone is not a measurement.
	if !strings.Contains(body, "time_zone") {
		t.Error("the page does not say which timezone its hours are in")
	}
}

// A request with no successor is never rendered as a duration, and an unpriced model is never
// rendered as $0. Both are one function each, and every cell goes through them.
func TestAnAbsenceIsNeverRenderedAsAZero(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	for _, fn := range []string{"function kvIdle(", "function kvMoney(", "function kvTTLPill("} {
		if !strings.Contains(src, fn) {
			t.Errorf("%s is gone; this check needs rewriting", fn)
		}
	}
	idle := src[strings.Index(src, "function kvIdle("):]
	idle = idle[:min(len(idle), 900)]
	if !strings.Contains(idle, "has_next") || !strings.Contains(idle, "no next request") {
		t.Error("kvIdle does not distinguish a request with no successor from a zero-length gap")
	}
	if !strings.Contains(idle, "0 s") {
		t.Error("kvIdle renders a genuine zero-length gap as nothing; tied timestamps are real " +
			"(9 of 12,635 consecutive pairs on this service) and dur(0) renders an em dash, " +
			"which is what 'no successor' looks like")
	}
	money := src[strings.Index(src, "function kvMoney("):]
	money = money[:min(len(money), 600)]
	if !strings.Contains(money, "unpriced") {
		t.Error("kvMoney has no unpriced branch; usd(0) renders '$0', which is a claim that " +
			"something was free")
	}
}

// Negative savings are rendered as negative, in the status colour, and nothing clamps them.
//
// A strategy that costs money is the only result on this page that can stop somebody making a
// change, so it is the one that must not be rounded away.
func TestNegativeSavingsAreShownAsNegative(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	i := strings.Index(src, "function kvSigned(")
	if i < 0 {
		t.Fatal("kvSigned is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 600)]
	if !strings.Contains(body, "bad-text") || !strings.Contains(body, "good-text") {
		t.Error("a saving is not coloured by its sign")
	}
	// The DIRECTION must be in words, not only in the sign and the colour.
	//
	// This is the defect that shipped: a column of signed dollars was read as a saving when it
	// was the opposite, and -$2,117.80 in a "saving" column made the worst arm look like the
	// best. Sign plus colour was not enough, and colour must never carry a judgement alone
	// anyway — the rule the rest of this dashboard already keeps.
	for _, want := range []string{"cheaper", "MORE"} {
		if !strings.Contains(body, want) {
			t.Errorf("kvSigned does not say %q; the direction of a difference against the "+
				"baseline has to be a WORD beside the figure, not just its sign", want)
		}
	}

	// No clamp on a MONEY line. Scoped to lines that mention a dollar figure or a saving,
	// because clamping a page OFFSET at zero is correct and clamping a saving at zero is the
	// defect: an unscoped substring check would forbid the first to catch the second.
	//
	// Math.abs is forbidden here for the same reason — it was how a signed figure got made
	// unsigned — with ONE exemption, which is narrow and is the reason the check above exists:
	// a line that emits the direction word (class "kv-dir") has already stated which way the
	// figure points, so taking the magnitude is a rendering choice rather than a lost sign.
	// "$2,117.80 MORE" is unambiguous where "-$2,117.80 MORE" is a double negative. Any OTHER
	// line that reaches for Math.abs on a money figure is still a failure.
	code := stripJSComments(src)
	for _, line := range strings.Split(code, "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, "usd") && !strings.Contains(l, "saving") &&
			!strings.Contains(l, "percent") {
			continue
		}
		statesDirection := strings.Contains(line, "kv-dir")
		for _, bad := range []string{"math.max(0", "< 0 ? 0", "math.abs("} {
			if bad == "math.abs(" && statesDirection {
				continue
			}
			if strings.Contains(l, bad) {
				t.Errorf("a saving is clamped or made unsigned here: %s", strings.TrimSpace(line))
			}
		}
	}
	// An undefined percentage is a pill, not a 0%.
	if !strings.Contains(code, "percent_known") || !strings.Contains(code, "undefined") {
		t.Error("a percentage against a zero baseline is not marked undefined; a percentage of " +
			"nothing is not 0%")
	}
}

// The tab respects the time-range filter, so it must NOT be in UNFILTERED_VIEWS — every figure
// on it is "over this window", and a tab that ignored the picker above it would be answering a
// different question from the one on screen.
func TestTheKVCacheTabRespectsTheTimeRange(t *testing.T) {
	app := readUI(t, "ui/app.js")
	i := strings.Index(app, "const UNFILTERED_VIEWS")
	if i < 0 {
		t.Fatal("UNFILTERED_VIEWS is gone; this check needs rewriting")
	}
	line := app[i : i+strings.Index(app[i:], "\n")]
	if strings.Contains(line, "kvcache") {
		t.Errorf("the KV-cache tab is in UNFILTERED_VIEWS: %s", line)
	}
	src := readUI(t, "ui/kvcache.js")
	// Wired into the loader table, or the tab renders nothing at all.
	if !strings.Contains(src, "loaders, { kvcache: loadKVCache }") {
		t.Error("the KV-cache view has no loader")
	}
	// The tab and the section mount themselves, so the shared page has one line about it.
	if !strings.Contains(src, `'data-view': 'kvcache'`) {
		t.Error("the KV-cache view does not mount its own tab")
	}
	if !strings.Contains(src, `id: 'view-kvcache'`) {
		t.Error("the KV-cache view does not mount its own section")
	}
	if !strings.Contains(readUI(t, "ui/index.html"), `<script src="kvcache.js">`) {
		t.Error("kvcache.js is not loaded by the shared page")
	}
}

// No new hue, no pie, no gauge, no second chart library. The dashboard has a fixed four-hue
// categorical order, a status palette, a de-emphasis gray and two chart primitives; a fifth
// series colour means a chart needs splitting, not a new colour.
func TestTheKVCacheTabAddsNoNewChartVocabulary(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/kvcache.js"))
	for _, bad := range []string{"--s5", "var(--s5)", "pieChart", "treemap", "gauge", "Chart.js",
		"d3.", "conic-gradient", "y2", "rightAxis"} {
		if strings.Contains(code, bad) {
			t.Errorf("the KV-cache tab introduces %q. Every panel here maps onto stat tiles, "+
				"barRows, lineChart and a table; anything else is a new vocabulary for the "+
				"reader to learn.", bad)
		}
	}
	// It draws with the shared primitives rather than its own SVG.
	for _, want := range []string{"barRows(", "lineChart(", "tileGroup(", "emptyState("} {
		if !strings.Contains(code, want) {
			t.Errorf("the KV-cache tab does not use the shared %s", want)
		}
	}
	if strings.Contains(code, "svgEl(") {
		t.Error("the KV-cache tab draws its own SVG instead of using the shared chart helpers")
	}
	// The stylesheet adds no hue of its own either.
	css := readUI(t, "ui/kvcache.css")
	for _, m := range regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).FindAllString(css, -1) {
		t.Errorf("kvcache.css hardcodes the colour %s; every colour on this dashboard is a "+
			"token from style.css", m)
	}
}

// Every row of the detail table leads back to the request and to the conversation it came from.
// An analysis nobody can follow into the transcript is a dead end.
func TestTheDetailTableLinksBackIntoTheDashboard(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	i := strings.Index(src, "function renderKVTable(")
	if i < 0 {
		t.Fatal("renderKVTable is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 4000)]
	for _, must := range []string{"r.request_url", "r.conversation_url"} {
		if !strings.Contains(body, must) {
			t.Errorf("the detail table does not link with %s", must)
		}
	}
	// The links come from the payload rather than being assembled here, so a route the UI
	// spells differently cannot silently produce a dead link.
	if strings.Contains(body, "'#requests?req='") || strings.Contains(body, `"#requests?req="`) {
		t.Error("the detail table builds its own request URL; the server sends it")
	}
}

// The table is sorted and paged on the SERVER, which is what makes a sorted column mean
// anything: sorting one page client-side would label a column "Idle" and show the top of an
// arbitrary slice. (The same distinction app.js draws between Components and Sessions.)
func TestTheDetailTableSortsOnTheServer(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	i := strings.Index(src, "function kvSortable(")
	if i < 0 {
		t.Fatal("kvSortable is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 1200)]
	if !strings.Contains(body, "kvLoadRows()") {
		t.Error("a header click does not re-read from the server")
	}
	if strings.Contains(body, "sortRows(") {
		t.Error("the KV-cache table sorts one page client-side; it is server-paged, so that " +
			"would show the top of an arbitrary page under a column heading that claims " +
			"otherwise")
	}
}

// The coverage banner renders UNCONDITIONALLY, not only when the news is bad — the same rule
// the keep-alive tab's downside panel keeps, and for the same reason: a page that tells the
// truth only to the people who already found out is not telling the truth.
func TestTheCoverageStatementIsNotBehindABranchOnGoodNews(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	i := strings.Index(src, "function renderKVCoverage(")
	if i < 0 {
		t.Fatal("renderKVCoverage is gone; this check needs rewriting")
	}
	body := src[i : i+min(len(src)-i, 3000)]
	// The three states it must be able to report.
	for _, must := range []string{"kv-truncated", "kv-ttl-coverage", "kv-cost-coverage",
		"kv-single-note"} {
		if !strings.Contains(body, must) {
			t.Errorf("the coverage panel cannot report %s", must)
		}
	}
	if !strings.Contains(body, "NOT RECORDED") {
		t.Error("the coverage panel does not say that a blank tier means NOT RECORDED rather " +
			"than 'no cache'; that is the additive-column trap this dashboard has hit before")
	}
	// And loadKVCache calls it before anything else renders.
	load := src[strings.Index(src, "async function kvLoadAnalysis("):]
	load = load[:min(len(load), 1200)]
	cov := strings.Index(load, "renderKVCoverage()")
	tiles := strings.Index(load, "renderKVTiles()")
	if cov < 0 || tiles < 0 || cov > tiles {
		t.Error("the coverage statement is not rendered before the figures it qualifies")
	}
}

// No top-level name in a self-mounting view may collide with one in the shared script.
//
// The bug this caught: kvcache.js declared `const kv` and app.js already had a global
// `function kv(k, v)` — the key-value row renderer. Two top-level declarations of the same
// name in two classic scripts is a SyntaxError, so the whole view failed to parse and the tab
// simply never appeared. Nothing else in this suite could see it: each file parses on its own,
// and every static check here is a substring match within one file.
//
// Asserted over every appended view, not just the newest one, because the next one will have
// the same problem.
func TestAppendedViewsDeclareNoNameTheSharedScriptAlreadyOwns(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^(?:async\s+)?(?:function|const|let|var)\s+([A-Za-z_$][\w$]*)`)
	top := func(name string) map[string]bool {
		out := map[string]bool{}
		for _, m := range decl.FindAllStringSubmatch(readUI(t, name), -1) {
			out[m[1]] = true
		}
		if len(out) == 0 {
			t.Fatalf("%s: found no top-level declarations; this check needs rewriting", name)
		}
		return out
	}
	shared := top("ui/app.js")
	seen := map[string]string{}
	for k := range shared {
		seen[k] = "ui/app.js"
	}
	// In document order, which is the order a browser evaluates them in.
	for _, view := range []string{"ui/tools.js", "ui/kvcache.js"} {
		for name := range top(view) {
			if owner, clash := seen[name]; clash {
				t.Errorf("%s declares %q at top level, which %s already declares. Two classic "+
					"scripts declaring the same name is a SyntaxError, so %s would not parse at "+
					"all and its tab would silently never appear.", view, name, owner, view)
				continue
			}
			seen[name] = view
		}
	}
}

// The arm picker is built from the SERVER's registry, and this file names no arm.
//
// The failure this guards actually happened here: kvcache.js carried `const KV_ARMS` with six
// names and the API carried a matching closed list, while package kvcache had grown to ten arms.
// Two keep-alive policies, the extend-to-1h policy and the exact cost ceiling were shipped,
// tested, and unreachable from the dashboard — and no test on either side could see it, because
// each list was internally consistent. A page that enumerates its own options cannot show an
// option it was not told about.
func TestTheArmPickerIsBuiltFromTheServersRegistry(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	code := stripJSComments(src)
	if !strings.Contains(code, "kvc.sim && kvc.sim.arms") {
		t.Error("the picker does not read the arms out of the simulate payload")
	}
	// No list of arm names in this file. Two or more known arm names quoted on one line is what a
	// hardcoded roster looks like, whatever it is called.
	names := []string{"'no-cache'", "'fixed-5m'", "'fixed-1h'", "'observed-policy'",
		"'historical-probability'", "'keepalive-5m'", "'keepalive-1h'", "'optimal'"}
	for _, line := range strings.Split(code, "\n") {
		n := 0
		for _, name := range names {
			if strings.Contains(line, name) {
				n++
			}
		}
		if n >= 2 {
			t.Errorf("this line enumerates the arms; the server does that:\n%s",
				strings.TrimSpace(line))
		}
	}
}

// An UNREACHABLE arm is labelled a ceiling, in a word, and is never offered as a baseline.
//
// `optimal` is told the real next-request time, so it is the cheapest plan that exists for a
// history rather than a policy anybody can run. Two ways to mislead with it, and both are shut
// here: rendering it beside real arms in the same style, and dividing a percentage by it — every
// figure measured against it would be a share of a number no policy can reach.
func TestTheCeilingArmIsLabelledAndNeverABaselineOnThePage(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	code := stripJSComments(src)
	i := strings.Index(code, "function kvBaselineArms(")
	if i < 0 {
		t.Fatal("kvBaselineArms is gone; the baseline picker no longer filters anything")
	}
	if !strings.Contains(code[i:i+min(len(code)-i, 300)], "!a.unreachable") {
		t.Error("the baseline picker offers unreachable arms; a percentage divided by a ceiling " +
			"is a share of a number no policy can reach")
	}
	// The label is a WORD, not a colour. The colour is allowed as a second statement of the same
	// fact (see kvcache.css) but it must never be the only one.
	if !strings.Contains(code, "'ceiling'") {
		t.Error("nothing on the page says the word 'ceiling'; an unreachable arm rendered like a " +
			"real one is a promise the product cannot keep")
	}
	if !strings.Contains(code, "kvCeilingPill()") {
		t.Error("the ceiling marker is not applied anywhere")
	}
	// It is applied in BOTH places an arm appears: the picker and the comparison row.
	if n := strings.Count(code, "kvCeilingPill()"); n < 3 {
		t.Errorf("the ceiling marker appears %d times; it has to be on the picker entry and on "+
			"the comparison row, or one of the two reads as an ordinary option", n)
	}
}

// Hit rate is on the page because an operator asks for it, and the page says out loud that it is
// not the objective.
//
// Measured on this service's own traffic the two point opposite ways: holding every prefix for an
// hour gives the best hit rate on the table and is one of the most expensive arms, because it pays
// 2.0x input to protect prompts a 1.25x write already covered. A reader scanning a table for the
// best-looking column will pick it, so the warning is in the markup rather than left to judgement
// — and nothing sorts or colours by it.
func TestHitRateIsNotPresentedAsTheObjective(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	code := stripJSComments(src)
	if !strings.Contains(code, "kv-hitrate-warning") {
		t.Error("the page does not say that hit rate is not the objective")
	}
	if !strings.Contains(src, "Hit rate is not the objective") {
		t.Error("the warning does not say it in words")
	}
	// No status colour and no sort on a hit-rate cell: those are the two ways a table recommends
	// a column. Every hit_rate_pct must be a plain pct() cell.
	for _, line := range strings.Split(code, "\n") {
		if !strings.Contains(line, "hit_rate_pct") {
			continue
		}
		for _, bad := range []string{"good-text", "bad-text", "sort", "SORT"} {
			if strings.Contains(line, bad) {
				t.Errorf("a hit-rate cell is coloured or sorted, which recommends it:\n%s",
					strings.TrimSpace(line))
			}
		}
	}
}

// The two ways a keep-alive costs more than a refresh are two numbers, not one.
//
// An UPGRADE is a policy buying a hold it chose: the entry moved to the one-hour tier because the
// arm decided the conversation was worth holding. A REWRITE is a schedule repairing damage it
// caused: the ping arrived after the entry had lapsed, so the refresh re-created the prefix at
// 12.5x a read. One is the mechanism working and the other is it misconfigured. Summed into a
// single "pings that cost extra", a working arm and a broken one look identical.
func TestTheTwoKindsOfCostlyPingAreNotOneNumber(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	code := stripJSComments(src)
	for _, want := range []string{"pings_that_upgraded", "pings_that_rewrote"} {
		if !strings.Contains(code, want) {
			t.Errorf("the page never reads %s", want)
		}
	}
	// And they are not added together anywhere.
	for _, line := range strings.Split(code, "\n") {
		if strings.Contains(line, "pings_that_upgraded") && strings.Contains(line, "pings_that_rewrote") &&
			strings.Contains(line, "+") {
			t.Errorf("the two are combined into one figure:\n%s", strings.TrimSpace(line))
		}
	}
}

// Every module-level constant an appended view REFERENCES is one that exists.
//
// The bug this exists for shipped and was caught by a screenshot, not by this suite: a `KV_ARMS`
// constant was removed in favour of reading the arm roster from the server, and one reference to
// it survived inside an async loader. The view parsed, mounted, drew its tiles, its histogram, its
// prices and its table — and the strategy comparison, the whole point of the page, rendered
// "Could not replay the strategies: KV_ARMS is not defined". Every other check in this file is a
// substring match, and a substring match cannot see an identifier that is no longer declared.
//
// Scoped to SCREAMING_SNAKE_CASE names because that is this codebase's module-constant
// convention, and those are exactly the ones an appended view borrows from app.js (TILE_INFO,
// SERIES, DIMS) or declares for itself. Comments and string literals are stripped first, or the
// direction word 'MORE' and every data-testid would read as an undefined reference.
func TestAppendedViewsReferenceNoConstantThatDoesNotExist(t *testing.T) {
	shared := stripJSLiterals(readUI(t, "ui/app.js"))
	// Browser and language globals that happen to be all-caps. A NEW one appearing here should
	// fail loudly and be added deliberately rather than silently allowed by a loose pattern.
	globals := map[string]bool{"JSON": true, "NaN": true, "URL": true, "DOM": true}
	declared := regexp.MustCompile(`(?:const|let|var|function|class)\s+([A-Z][A-Z0-9_]{2,})\b`)
	word := regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}\b`)

	for _, view := range []string{"ui/kvcache.js", "ui/tools.js"} {
		src := stripJSLiterals(readUI(t, view))
		have := map[string]bool{}
		for _, m := range declared.FindAllStringSubmatch(shared+"\n"+src, -1) {
			have[m[1]] = true
		}
		for _, name := range word.FindAllString(src, -1) {
			if have[name] || globals[name] {
				continue
			}
			t.Errorf("%s references %s, which nothing declares in it or in app.js. A reference to "+
				"a removed constant parses fine and throws at run time, so the panel that touches "+
				"it renders an error while the rest of the page looks perfect.", view, name)
			have[name] = true // report each name once
		}
	}
}

// stripJSLiterals removes comments AND quoted strings, so a static scan for identifiers cannot
// be fooled by prose or by a string that happens to look like one.
//
// Crude on purpose — it is not a parser, and it does not need to be: it is only ever used to
// decide whether a run of capitals is code or text. Template literals are treated as strings
// whole, which loses any ${} interpolation inside them; that is the conservative direction for a
// check that reports unknown names.
func stripJSLiterals(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch c := src[i]; {
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
		case c == '\'' || c == '"' || c == '`':
			i++
			for i < len(src) && src[i] != c {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++
			b.WriteByte(' ') // keep tokens either side from fusing
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// When nothing in the window has a rate, the page SAYS SO above the comparison — and it reads
// the SERVER'S OWN field for it rather than re-deriving the condition.
//
// The state is real: the price map is fetched in the background, so the first load after a
// restart has no rates for a few seconds, and a deployment with no price list never gets any. It
// earns a banner rather than a row of "unpriced" pills because of what an unpriced replay does to
// the arms that decide by comparing costs — the exact ceiling included. With no costs to compare
// they let every entry expire and report a 0% hit rate and no writes, and on the ceiling row that
// reads as "the cheapest possible plan is to never cache anything", the opposite of the finding
// this page exists for. Found by rendering the page during a cold start, not by any check here.
//
// `valued` rather than `unpriced === requests`: the same fact, but one of them is the server's
// answer and the other is this page's inference, and an inference is a second definition to keep
// in step. kvcache.Result.Valued exists precisely because the derivation is the step a consumer
// forgets.
func TestAnEntirelyUnpricedWindowIsNamedFromTheServersOwnField(t *testing.T) {
	src := readUI(t, "ui/kvcache.js")
	code := stripJSComments(src)
	if !strings.Contains(code, "kv-unvalued") {
		t.Error("the comparison panel does not name the state where no model has a rate")
	}
	if !strings.Contains(code, "r.valued === false") {
		t.Error("the panel does not read Result.valued; deriving the condition from unpriced and " +
			"requests is a second definition of it, which is the inference the field exists to " +
			"remove")
	}
	// The derivation is banned on ANY receiver, not just on a result. Groups carry `valued` too
	// now, so `g.unpriced < g.requests` is the same second definition one level down — and it was
	// there until a review found it, precisely because this check named a single receiver.
	if m := regexp.MustCompile(`\w+\.unpriced\s*<\s*\w+\.requests`).FindString(code); m != "" {
		t.Errorf("the page still derives 'nothing was priced' for itself (%s) alongside reading "+
			"valued; two answers to one question is how they come to disagree, and both Result "+
			"and Group state it now", m)
	}
	if !strings.Contains(src, "ceiling") {
		t.Error("the banner does not say the ceiling is affected too, which is the one row an " +
			"unpriced replay makes actively misleading")
	}
}

// The wire contract between this package's payloads and the page that reads them.
//
// RECONSTRUCTED after I deleted the third session's version of this test with an unscoped
// tail-replacement. It is written from the failure it was for, which is worth stating because the
// failure is invisible in the most expensive way: I rebuilt KVCacheFormula with the json tags
// {name, expression, prose} while the page read {name, formula, note}. `name` matched by luck, so
// the assumptions panel rendered eleven formula HEADINGS above eleven EMPTY code boxes — which
// reads as a stylesheet problem, not a missing field. Two tests passed straight through it: one
// asserting the payload carries at least eight formulas, one asserting the page does not hardcode
// them. Each was right about its own half of a contract neither checked ACROSS.
//
// Both directions matter and only one of them is cheap, so this does the cheap one generally and
// the expensive one where the bug actually was:
//
//   - every snake_case property the page reads must be a key some payload emits. JS locals in this
//     codebase are camelCase, so an underscore is a reliable signal that a read is a wire field.
//   - the formula object, specifically, must carry exactly the keys the page reads off it — the
//     one place a rename slipped through, and the one shape whose keys have no underscores.
func TestThePageOnlyReadsFieldsThePayloadsEmit(t *testing.T) {
	// Keys are collected from the TYPES by reflection, not from marshalled instances.
	//
	// An instance cannot answer the question: `next_ts` is tagged omitempty, so a zero-valued
	// fixture omits it and the check would report a field the page legitimately reads as absent.
	// Populating every field of every payload by hand is the alternative, and it is a fixture
	// nobody would keep current. The tags are the contract; read the contract.
	emitted := map[string]bool{}
	var walk func(reflect.Type, int)
	walk = func(t reflect.Type, depth int) {
		if depth > 8 {
			return
		}
		for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() == reflect.Map {
			walk(t.Elem(), depth+1)
			return
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported: never on the wire
			}
			tag := f.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			switch {
			case name == "-":
				continue
			case f.Anonymous && name == "":
				// Embedded and untagged: its fields are inlined at this level.
				walk(f.Type, depth+1)
				continue
			case name == "":
				// An exported field with no json tag ships under its GO name, which no page reads.
				emitted[f.Name] = true
			default:
				emitted[name] = true
			}
			walk(f.Type, depth+1)
		}
	}
	for _, typ := range []any{
		KVCacheAnalysis{}, KVCacheRowPage{}, KVCacheSimulation{}, KVCachePriceView{},
	} {
		walk(reflect.TypeOf(typ), 0)
	}

	code := stripJSComments(readUI(t, "ui/kvcache.js"))
	// Reads of the form `.some_field`. An underscore is what distinguishes a wire field from a
	// local or a DOM property in this codebase.
	read := regexp.MustCompile(`\.([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\b`)
	// Properties that look like wire fields and are not: DOM and app.js surface.
	notWire := map[string]bool{"last_event_id": true}
	seen := map[string]bool{}
	for _, m := range read.FindAllStringSubmatch(code, -1) {
		name := m[1]
		if emitted[name] || notWire[name] || seen[name] {
			continue
		}
		seen[name] = true
		t.Errorf("the page reads .%s, which no payload emits. Either a json tag moved and this "+
			"cell now renders blank, or the field was never added — and a blank cell reads as a "+
			"styling problem rather than a missing field.", name)
	}

	// SINGLE-WORD wire fields, which the underscore heuristic above is blind to.
	//
	// That blindness was not small: `valued`, `label`, `n`, `usd`, `key`, `hits`, `strategy`,
	// `ttl`, `seconds`, `unpriced` and thirty more carry no underscore, so more than half the
	// wire surface this page reads was outside the check — including the one field a banner
	// gates on, which is how a page comes to test a condition the server has stopped emitting.
	//
	// Covered by scoping on the RECEIVER instead of on the shape of the name. Every payload object
	// on this page is dereferenced through a one-letter binding (a=analysis, c=cards, r=result or
	// row, g=group, s=saving, b=band, p=point, f=formula, m=model), so `x.field` where x is one of
	// those is a wire read; the handful of genuine non-wire reads on those bindings are named in
	// `local` rather than pattern-matched away.
	//
	// WHAT THIS DOES AND DOES NOT CATCH, measured rather than assumed. `emitted` is the UNION of
	// every payload's keys, and the receiver letter is not bound to a type — binding it would mean
	// type-checking JavaScript. So:
	//
	//   - a field name that disappears from the payloads ENTIRELY is caught. Verified by renaming
	//     KVCacheArm.Selectable's tag: the check reports `a.selectable`, which is the failure this
	//     is for.
	//   - a field that MOVES between shapes is NOT caught. Verified by renaming KVCacheGroup.Key's
	//     tag while kvcache.Group still emits `key`: the check stays green. A read of `g.key` that
	//     now resolves against a different shape's field is outside what a substring check can
	//     see, and pretending otherwise in this comment would be worse than the gap.
	//
	// The bug this was written for — a rename that removed `formula` from every payload — is in the
	// first category, and the formula object is additionally checked by name below.
	local := map[string]bool{
		// kvc.rates[model] keys: the page's own edit state, in USD per MILLION tokens, which is
		// deliberately NOT the payload's per-token shape.
		"in": true, "out": true, "read": true, "w5m": true, "w1h": true,
	}
	oneWord := regexp.MustCompile(`\b([abcfgmprs])\.([a-z][a-z0-9]*)\b`)
	for _, m := range oneWord.FindAllStringSubmatch(stripJSStrings(code), -1) {
		name := m[2]
		if strings.Contains(name, "_") || emitted[name] || local[name] || seen[name] {
			continue
		}
		seen[name] = true
		t.Errorf("the page reads %s.%s, which no payload emits. If it is this page's own state "+
			"rather than a wire field, name it in `local` with the reason.", m[1], name)
	}

	// The formula object, by name, because its keys carry no underscore and so the check above
	// cannot see them. This is where the rename actually shipped.
	body := code[strings.Index(code, "function renderKVAssumptions("):]
	body = body[:strings.Index(body, "\n}")]
	fb, err := json.Marshal(KVCacheFormula{})
	if err != nil {
		t.Fatal(err)
	}
	var formulaKeys map[string]any
	if err := json.Unmarshal(fb, &formulaKeys); err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\bf\.([a-z][a-z0-9_]*)\b`).FindAllStringSubmatch(body, -1) {
		if _, ok := formulaKeys[m[1]]; !ok {
			keys := make([]string, 0, len(formulaKeys))
			for k := range formulaKeys {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Errorf("the assumptions panel reads f.%s off a formula, which carries %v. Eleven "+
				"headings over eleven empty code boxes is what this looks like on screen.",
				m[1], keys)
		}
	}
}

// stripJSStrings removes single-quoted string literals, so a word inside a message or a CSS class
// is not mistaken for a property read. Comments are already gone by the time this is used.
//
// Crude on purpose: it does not need to parse JavaScript, only to stop `'not recorded'` and
// `'pill missing'` from looking like field accesses. A literal it fails to strip can only cause a
// false POSITIVE, which is a test that asks a question rather than one that hides an answer.
func stripJSStrings(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		if src[i] != '\'' {
			b.WriteByte(src[i])
			i++
			continue
		}
		i++
		for i < len(src) && src[i] != '\'' {
			if src[i] == '\\' {
				i++
			}
			i++
		}
		i++
	}
	return b.String()
}
