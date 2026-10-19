package dash

// Static checks over the History tab's assets.
//
// WHY A SOURCE-LEVEL TEST. This tab makes a weaker claim than any other in the dashboard, and
// the weakness is invisible on screen: it reports usage recovered from Claude Code's own
// transcripts, over a span the proxy never watched, with NO cost figure — because a transcript
// records what a request spent in total and never which tools it declared. A page of counts and
// dates looks exactly as authoritative as a page of measurements. Every assertion here exists to
// keep the difference visible to a reader who did not read the API's documentation.

import (
	"regexp"
	"strings"
	"testing"
)

// THE REFUSAL HAS TO BE ON SCREEN, not only in the JSON.
//
// `/api/history` carries a `basis` saying the cost of carrying an unused capability is not
// recoverable from this data. A view that renders the counts and drops the basis turns a
// deliberately incomplete document into one that looks complete — and the absent cost column
// then reads as zero rather than as unknown.
func TestTheHistoryViewRendersItsBasis(t *testing.T) {
	src := readUI(t, "ui/history.js")
	// APPENDED, not merely mentioned. Asserting that the string "basis" appears somewhere in the
	// file is satisfied by this very comment, so the check has to see the value reach the DOM:
	// a `text:` carrying `.basis` inside an el() that is appended.
	if !regexp.MustCompile(`appendChild\([^;]*\.basis`).MatchString(src) {
		t.Error("doc.basis is never appended to the view. The one field whose job is saying what " +
			"kind of claim this page makes is being dropped before it reaches the reader")
	}
	// And the page must say in its own words why no money is shown — in RENDERED text, since a
	// reader who skips the basis paragraph still has to understand it. Checked as a `text:`
	// property so a prose comment cannot satisfy it.
	// EVERY QUOTED LITERAL, with comment lines stripped first. Simpler than trying to parse the
	// el() call that holds them, and it cannot be satisfied by a comment — which is exactly how
	// an earlier version of this check passed while the explanation existed only in the header.
	var code strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	lits := regexp.MustCompile(`'(?:[^'\\]|\\.)*'`).FindAllString(code.String(), -1)
	var said bool
	for _, r := range lits {
		low := strings.ToLower(r)
		for _, phrase := range []string{"not recoverable", "cannot be priced", "no money", "never recorded"} {
			if strings.Contains(low, phrase) {
				said = true
			}
		}
	}
	if !said {
		t.Error("no text RENDERED by this tab explains why it shows no money. An absent cost " +
			"column reads as zero unless something says otherwise — which is the whole reason " +
			"this surface is separate from the Cleanup tab")
	}
}

// NOT ONE PRICE IN THIS VIEW. The same rule the KV-cache tab carries, and for a sharper reason:
// here there is no price to show at all, so any arithmetic producing one would be inventing it.
func TestTheHistoryViewComputesNoCost(t *testing.T) {
	src := readUI(t, "ui/history.js")
	// A per-token rate literal, in the scientific forms such a rate takes.
	rate := regexp.MustCompile(`\d\s*[eE]-0?[5-9]|0\.00000\d`)
	if m := rate.FindString(src); m != "" {
		t.Errorf("found what looks like a per-token rate (%q). This view has no cost to show: a "+
			"figure computed here would be invented, and would look more authoritative than the "+
			"measured ones on the Cleanup tab because it covers a longer span", m)
	}
	// And no multiplication of a token count into money.
	if regexp.MustCompile(`tokens\s*\*|\*\s*tokens`).MatchString(src) {
		t.Error("a token count is being multiplied in this view; the only honest arithmetic here " +
			"is counting")
	}
}

// IT MUST SAY THAT THE SPAN IS NOT THE MEASURED ONE.
//
// This is the tab's most confusable property. The Cleanup tab's figures cover what the proxy
// watched; this tab's cover months before it was installed. A reader comparing the two without
// being told will read the longer span as better evidence for the same kind of claim.
func TestTheHistoryViewDistinguishesImportedFromMeasured(t *testing.T) {
	src := readUI(t, "ui/history.js")
	for _, f := range []string{"imported_requests", "measured_requests"} {
		if !strings.Contains(src, f) {
			t.Errorf("the view never reads %s; a reader cannot tell how much of this page the "+
				"proxy actually watched", f)
		}
	}
}

// The tab has to be reachable: mounted, and in the router's loader map. A view absent from
// `loaders` renders an empty section forever, which is the failure mode insights.js documents.
func TestTheHistoryViewIsMountedAndRouted(t *testing.T) {
	src := readUI(t, "ui/history.js")
	if !strings.Contains(src, "mountTab(") {
		t.Error("history.js does not call mountTab; nothing would put the tab in the nav")
	}
	// The entry has to call the loader, not merely name the key: `history:` alone is satisfied
	// by any object literal anywhere in a 560 KB file.
	if !regexp.MustCompile(`history:\s*\(\)\s*=>[^,]*loadHistory`).MatchString(readUI(t, "ui/app.js")) {
		t.Error("`loaders` in app.js has no history entry calling loadHistory; the tab would " +
			"mount and then render an empty section forever")
	}
	if !strings.Contains(readUI(t, "ui/index.html"), "history.js") {
		t.Error("index.html does not load history.js, so none of it runs")
	}
}

// THE REPORTER STAYS OUT OF ITS OWN REPORT — and says that it did.
//
// The API holds this plugin's own skills out of every figure and reports them under
// `self_excluded`, because excluding it from its own numbers is right while hiding that it
// occupies the prompt is not. A view that drops the field completes the concealment.
func TestTheHistoryViewDisclosesItsOwnExclusion(t *testing.T) {
	src := readUI(t, "ui/history.js")
	if !strings.Contains(src, "self_excluded") {
		t.Error("the view never reads self_excluded; this tool's own skills are held out of " +
			"every figure on the page and the reader is not told, which is worse than the cost")
	}
}

// ONE POPULATION, ONE QUESTION.
//
// THE DEFECT THIS REPLACES, reported as "not a single place where I can understand something":
// the page carried four counts over four different populations — skills used here of skills used
// anywhere, skills never used of all installed, a machine-wide tab count, and a per-project tab
// count that included tools. No two could be compared, and two contradicted each other outright
// ("used in this project 0 of 6" beside a Used tab reading 1, because one counted skills and the
// other counted tools too).
//
// So every count on this page is over INSTALLED SKILLS, and tools appear only in their own
// section with their own count. This test pins the rule that made the page readable: no skill
// count and tool count may be rendered as parts of one figure.
func TestTheHistoryViewKeepsToolsOutOfTheSkillCount(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	// The tools section has to filter to non-skills, and the skills sections to skills, so the
	// two populations can never be summed by accident.
	if !strings.Contains(code, "filter((u) => !u.skill)") {
		t.Error("the view never separates tools from skills when listing them. Mixing the two " +
			"populations is what produced a skills count of 0 beside a tools-inclusive count " +
			"of 1 on the same screen")
	}
	if !strings.Contains(code, "filter((u) => u.skill)") {
		t.Error("the used-skills list is not filtered to skills, so a tool would be counted in " +
			"a figure the reader compares with the installed-skill count")
	}
	// And tools must be marked as not removable, or a reader takes them for waste.
	lits := strings.ToLower(strings.Join(
		regexp.MustCompile(`'(?:[^'\\]|\\.)*'`).FindAllString(code, -1), " "))
	var says bool
	for _, p := range []string{"cannot be removed", "part of claude code", "not removable"} {
		if strings.Contains(lits, p) {
			says = true
		}
	}
	if !says {
		t.Error("nothing says the built-in tools cannot be removed. Listed beside a waste list " +
			"without that, they read as more waste")
	}
}

// THE HEADLINE STATES BOTH HALVES OF THE RATIO.
//
// "31 never used" is not a finding without "of your 35 installed" — the denominator is what
// makes it mean something, and both have to come from the document rather than being counted in
// the browser from a list that may be filtered.
func TestTheHistoryViewStatesTheRatioFromTheDocument(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	if !regexp.MustCompile(`never\s*\+[^;]*inst|inst[^;]*\+\s*never`).MatchString(code) &&
		!regexp.MustCompile(`never \+ ' of your ' \+ inst`).MatchString(code) {
		t.Error("the headline does not render the count and the denominator together. A bare " +
			"'31 never used' gives the reader nothing to judge it against")
	}
	if !regexp.MustCompile(`[a-z]\.installed_skills`).MatchString(code) {
		t.Error("the denominator is not read from the document, so it would be right on this " +
			"machine and wrong everywhere else")
	}
}

// stripComments drops comment lines so an assertion about RENDERED text cannot be satisfied by
// prose explaining the very thing that is missing. An earlier version of these checks passed on
// a file header while the explanation had been deleted from the view.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// THE OWNER ROW MUST SEPARATE THE NAME FROM THE COUNT.
//
// THE DEFECT, reported by a reader who could not add the list up: the rows rendered as
// "anthropic-agent-skills19 skills never used". The two spans had no layout between them, so the
// owner name ran into its count and `anthropic-agent-skills19` read as one token. The arithmetic
// was right -- 19 + 11 + 1 = 31 -- and unverifiable on screen, which is the same as wrong for a
// page whose whole job is letting a reader check a claim.
//
// The spans reuse .ins-nm and .ins-meta from the Cleanup tab, where the gap between them lives
// on .ins-head -- a class this view does not use, because its header is a <summary>. So the
// summary needs its own rule, and this pins that it has one.
func TestTheHistoryOwnerRowSeparatesNameFromCount(t *testing.T) {
	css := readUI(t, "ui/insights.css")
	// A rule for this view's summary, with a gap. Without it the two spans are adjacent inline
	// boxes and the browser renders them with no space at all.
	rx := regexp.MustCompile(`(?s)\.ins-own\s*>?\s*summary[^{]*\{[^}]*gap:`)
	if !rx.MatchString(css) {
		t.Error("no rule gives .ins-own summary a gap, so the owner name and its skill count " +
			"render as one token -- `anthropic-agent-skills19 skills never used`. The list then " +
			"cannot be added up by eye, which is the only thing it is there for")
	}
	// And it has to be a flex row, or `gap` does nothing.
	rx2 := regexp.MustCompile(`(?s)\.ins-own\s*>?\s*summary[^{]*\{[^}]*display:\s*flex`)
	if !rx2.MatchString(css) {
		t.Error(".ins-own summary is not laid out as flex, so any gap declaration is inert")
	}
}

// THE GROUPS STATE THEIR OWN TOTAL.
//
// Reported: a reader added 19 + 11 + 1 by hand to check it against the headline's 31 and asked
// why they disagreed. They did not -- but making the reader do the arithmetic is what invites
// the doubt, especially on a page whose figures have been wrong before. So the sum the page is
// claiming is printed next to the groups that make it up.
func TestTheHistoryViewStatesTheTotalOfItsGroups(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	if !regexp.MustCompile(`reduce\(\(a, g\) => a \+ g\.skills\.length`).MatchString(code) {
		t.Error("the view never sums the owner groups, so a reader has to add them up to check " +
			"them against the headline -- which is exactly the arithmetic that got reported as " +
			"not closing")
	}
}
