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

// THE DENOMINATOR AND THE OVERLAP, both on screen.
//
// "40 never used" means nothing without "of 46 installed", and the two do not partition the
// inventory on their own — a used capability absent from disk is in neither set. The API states
// the overlap in `used_installed` for exactly that reason; a view that ignores it and subtracts
// instead puts arithmetic on screen that does not add up.
func TestTheHistoryViewShowsTheDenominatorNotJustTheGap(t *testing.T) {
	src := readUI(t, "ui/history.js")
	// READ FROM THE DOCUMENT. A literal would make the denominator right on this machine and
	// wrong on every other one, which is invisible on screen.
	if !regexp.MustCompile(`[a-z]\.installed_skills`).MatchString(src) {
		t.Error("the view never reads installed_skills off the document; '40 never used' is not " +
			"a finding without the number installed, and a hardcoded denominator is wrong " +
			"everywhere but here")
	}
	if !strings.Contains(src, "used_installed") {
		t.Error("the view never reads used_installed, so it is either hiding the overlap or " +
			"recomputing it by subtraction — and Used and NeverUsed do not partition the " +
			"inventory, so the subtraction is wrong")
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

// THE TWO SCOPES HAVE TO BE LABELLED ON SCREEN.
//
// Under a project filter the used list is the folder's and the never-used count is the machine's.
// That is the right pair of scopes — a skill used in another project is not waste — but two
// figures from different scopes sitting side by side are read as sharing one. The tile that says
// "42 never used" has to say "in any project", or a reader takes it as a claim about the folder
// they just selected.
func TestTheHistoryViewLabelsTheScopeOfEachFigure(t *testing.T) {
	src := readUI(t, "ui/history.js")
	var code strings.Builder
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	flat := strings.ToLower(code.String())
	// THE LABEL HAS TO BE ON THE NEVER-USED FIGURE, not merely somewhere in the file. An earlier
	// version of this check passed on a dropdown tooltip reading "every project pooled together",
	// which tells the reader nothing about the tile's scope.
	i := strings.Index(flat, "never used in")
	if i < 0 {
		i = strings.Index(flat, "never once used")
	}
	if i < 0 {
		t.Fatal("no never-used label found at all; this check needs rewriting against whatever " +
			"replaced it")
	}
	// Its own label plus the explanatory line under it — the tile is three short elements, so a
	// window of a few hundred characters is the tile and not the page.
	end := i + 600
	if end > len(flat) {
		end = len(flat)
	}
	tile := flat[i:end]
	var scoped bool
	for _, phrase := range []string{"any project", "every project", "no project at all",
		"across all projects"} {
		if strings.Contains(tile, phrase) {
			scoped = true
		}
	}
	if !scoped {
		t.Errorf("the never-used figure carries no scope label. Beside a project-scoped used "+
			"list it reads as a claim about the selected folder, which would report a skill used "+
			"elsewhere as waste. Found instead: %.200q", tile)
	}
}

// THE PICKER MUST NOT SHRINK TO THE FOLDER ALREADY CHOSEN.
//
// A scoped response carries only the selected project, so a view that captures its folder list
// from whatever response is in hand loses every other option on the first selection — and the
// reader has no way back to the pooled view. insights.js documents the same trap for its chart
// axis, where one drag cut the axis from six columns to two.
func TestTheHistoryViewKeepsItsProjectListFromTheUnscopedLoad(t *testing.T) {
	src := readUI(t, "ui/history.js")
	if !regexp.MustCompile(`!histProjects\s*&&\s*!histProject`).MatchString(src) {
		t.Error("the folder list is captured without checking that the load was unscoped. The " +
			"first project selection would then shrink the picker to that one folder and strand " +
			"the reader there")
	}
}

// THE POOLED VIEW IS THE WASTE LIST, AND NOTHING ELSE.
//
// The two scopes do different jobs. Selecting a project answers "what did I use here"; the pooled
// view answers only "what have I never used anywhere" — the one claim strong enough to act on,
// since those capabilities are carried on every request and have earned nothing in the whole
// recorded history. A pooled "used" list would just be the union of the per-project ones and
// would bury the finding.
func TestTheHistoryViewLeadsWithTheWasteListWhenPooled(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	// The never-used half has to be what a pooled view opens on.
	if !regexp.MustCompile(`histProject\s*(?:===?\s*''|\?)`).MatchString(code) {
		t.Error("nothing in the view branches on whether a project is selected, so the pooled " +
			"view cannot lead with the waste list while a project view leads with usage")
	}
	lits := strings.ToLower(strings.Join(
		regexp.MustCompile(`'(?:[^'\\]|\\.)*'`).FindAllString(code, -1), " "))
	// It must name what the pooled claim IS, in rendered text.
	var says bool
	for _, p := range []string{"never been used", "never used in any", "no project at all",
		"never invoked in any"} {
		if strings.Contains(lits, p) {
			says = true
		}
	}
	if !says {
		t.Error("the pooled view never states its claim in rendered text. 'Never used anywhere' " +
			"is the finding this page exists for and has to be said, not implied by a count")
	}
}

// A PROJECT'S "NOT USED HERE" IS NOT A WASTE FIGURE, AND THE VIEW HAS TO SAY SO.
//
// The honesty requirement for the per-project view. A skill used in another project is being used
// correctly — reporting it under a bare "43 not used" heading turns deliberate, sensible usage
// into an accusation. So the per-project view has to split that number: how many were never used
// anywhere (the waste), and how many are used elsewhere (not waste).
func TestTheProjectViewSeparatesUnusedHereFromNeverUsedAnywhere(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	lits := strings.ToLower(strings.Join(
		regexp.MustCompile(`'(?:[^'\\]|\\.)*'`).FindAllString(code, -1), " "))
	var splits bool
	for _, p := range []string{"used in other project", "used elsewhere", "in another project",
		"not waste"} {
		if strings.Contains(lits, p) {
			splits = true
		}
	}
	if !splits {
		t.Error("the per-project view never distinguishes 'unused here' from 'never used " +
			"anywhere'. Without that split a reader takes the whole unused count as waste, when " +
			"a skill they use in another project is being used correctly")
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

// THE PROJECT VIEW HAS TO SHOW ITS OWN ARITHMETIC CLOSING.
//
// THE CONFUSION THIS FIXES, reported by a reader of the live page: "how is it possible that every
// project writes a different number of used tools but the same number of unused tools, if the
// number of all tools is the same?"
//
// The figures were right and the layout was not. The never-used-anywhere count is a fact about
// the MACHINE and cannot move when a project is selected; it sat in a tile beside two
// project-scoped figures, so a reader saw one number refuse to change while its neighbours did
// and correctly concluded something was broken.
//
// The fix is to show the identity the reader was trying to check. In any project:
//
//	used here + unused here = installed
//	used here + used elsewhere = used anywhere   (6 on the measured machine)
//
// so the view states both halves and labels the machine-wide figure as machine-wide.
func TestTheProjectViewShowsHowItsFiguresAddUp(t *testing.T) {
	src := readUI(t, "ui/history.js")
	code := stripComments(src)
	lits := strings.ToLower(strings.Join(
		regexp.MustCompile(`'(?:[^'\\]|\\.)*'`).FindAllString(code, -1), " "))

	// THE IDENTITY MUST BE RENDERED AS ARITHMETIC THE READER CAN FOLLOW, not merely mentioned.
	//
	// Matched as one expression -- `usedHere + elsewhere` with nothing between -- because a
	// looser pattern spanning `[^;]*` crossed line breaks and paired a usedHere from one tile
	// with an elsewhere from the next, passing on coincidental proximity while both real
	// arithmetic sites had been removed.
	// "6 are used somewhere on this machine" was already on the page and did not help: the
	// reader still could not see that used-here plus used-elsewhere makes that 6. So the view
	// has to put the two addends next to their sum.
	if !regexp.MustCompile(`usedHere\s*\+\s*elsewhere|elsewhere\s*\+\s*usedHere`).
		MatchString(code) {
		t.Error("the view never renders used-here and used-elsewhere as parts of one total. A " +
			"reader seeing 'used here 1' beside 'never used anywhere 40' cannot work out where " +
			"the other 5 went, and reads the page as broken -- which is exactly what happened")
	}
	// And the machine-wide figure must say that it is the SAME in every project, which is the
	// specific fact whose absence made it look like a bug.
	var labelled bool
	for _, p := range []string{"same in every project", "not a figure about this project",
		"whole machine, not this project", "does not change when you pick"} {
		if strings.Contains(lits, p) {
			labelled = true
		}
	}
	if !labelled {
		t.Error("nothing says the never-used-anywhere figure is the same in every project. It " +
			"cannot move when a project is selected, so beside two figures that do move it " +
			"reads as a number that is mysteriously stuck")
	}
}
