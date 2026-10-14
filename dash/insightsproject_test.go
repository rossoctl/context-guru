package dash

import (
	"strings"
	"testing"
)

// The project picker is a VIEW control, and three properties of it are easy to break silently.
func TestTheProjectPickerIsAViewControlAndNotAFilterDimension(t *testing.T) {
	js := readUI(t, "ui/insights.js")
	code := stripJSComments(js)
	app := readUI(t, "ui/app.js")

	// 1. It must not join the shared filter dimensions. DIMS drives the filter bar, the URL hash
	//    and the removable chips, and navhash.test.mjs asserts that list is EXACTLY fourteen
	//    entries — so registering a fifteenth breaks the nav contract rather than extending it.
	//    A scope selector is a view of one filtered window, the same call state.dim makes for the
	//    Usage breakdown.
	if strings.Contains(app, "['project',") {
		t.Error("app.js registers `project` as a filter dimension. DIMS is pinned at fourteen " +
			"entries by navhash.test.mjs, and a scope selector is a view rather than a filter")
	}

	// 2. Changing it must RE-QUERY, not filter locally. The verdicts, the observation floor and
	//    the owner rollups are all computed server-side over the filter's window, so narrowing in
	//    JS would leave the verdicts describing every project while the rows described one.
	if !strings.Contains(code, "q.project = insProject") {
		t.Error("insights.js does not send the project with its request, so the page would be " +
			"filtering locally while the server's verdicts still describe every project")
	}

	// 3. And it must not call insRender(): that replaces #insights-body, which collapses every
	//    open fold and steals focus. TestASelectionChangeDoesNotRebuildTheView caps the call
	//    count globally; this pins the picker's own handler, the one most likely to reach for it.
	at := strings.Index(code, "function insProjectPicker")
	if at < 0 {
		t.Fatal("insProjectPicker is gone")
	}
	end := strings.Index(code[at:], "\n}")
	if end < 0 {
		t.Fatal("cannot find the end of insProjectPicker")
	}
	if body := code[at : at+end]; strings.Contains(body, "insRender(") {
		t.Error("the project picker calls insRender(), which rebuilds #insights-body and so " +
			"collapses every open fold. loadInsights() ends in a render of its own")
	}
}

// "All projects" has to stay reachable and be the default, or this feature takes away the view the
// reader has today. It is also the strongest evidence available: per project there are one to five
// sessions against a floor of five sessions and seven days.
func TestAllProjectsIsTheDefaultAndStaysSelectable(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))
	if !strings.Contains(code, "let insProject = ''") {
		t.Error("insProject does not default to '' (all projects), so the view no longer opens " +
			"on the pooled numbers it showed before this feature existed")
	}
	if !strings.Contains(code, "'all projects'") {
		t.Error("the picker offers no `all projects` option; a reader who narrows could not get " +
			"back to the pooled view the rest of the dashboard reports")
	}
	// The unattributed rows are offered as their own option, named by a reserved word rather than
	// an empty value: "" already means "every project", so it cannot also mean "those with none".
	//
	// Asserted on the OPTION, not on the bare string 'none'. The looser form passed while the
	// served file still carried a '\u0000unattributed' sentinel — the word appears elsewhere in
	// the file, so the test matched a different occurrence and a browser check found the real
	// value. A NUL in a query string survives neither a URL nor a bookmark, and the server would
	// have matched it against nothing while the page looked scoped.
	if !strings.Contains(code, "opts.push(['none'") {
		t.Error("the picker's unattributed option is not the reserved word `none`; that is where " +
			"every row written before the project column existed now lives, and a sentinel " +
			"character would not survive the query string")
	}
	if strings.Contains(code, "\\u0000") {
		t.Error("insights.js still carries a NUL sentinel; it cannot survive a URL round trip")
	}
}

// ONE PROJECT IS STILL A CHOICE, because the unattributed rows are the other side of it.
//
// The picker hid itself whenever fewer than two project FOLDERS were recorded, on the reasoning
// that a one-entry dropdown is a control whose only outcome is no change. That reasoning was
// wrong, and it shipped: on the real store there is one folder and 2,985 unattributed rows —
// every request written before the column existed — so there were two scopes worth comparing and
// no control to compare them with. It also printed "Every request recorded so far came from X",
// which was false for 2,985 of 2,987 rows.
//
// So the test is about the GUARD, not the wording: the picker may only stand down when there is
// genuinely nothing to choose between.
func TestThePickerAppearsWhenThereIsAnythingToChooseBetween(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))
	at := strings.Index(code, "function insProjectPicker")
	if at < 0 {
		t.Fatal("insProjectPicker is gone")
	}
	end := strings.Index(code[at:], "\n}")
	body := code[at : at+end]

	// The old guard counted folders alone. Nothing may stand down on that count by itself.
	if strings.Contains(body, "insProjects.length < 2") {
		t.Error("the picker hides itself whenever fewer than two FOLDERS are recorded, which " +
			"ignores the unattributed requests — on a store with one folder and thousands of " +
			"unattributed rows there are two scopes to compare and no control to compare them")
	}
	// And the claim that every request came from one folder must be gone with it: it is only
	// true when nothing is unattributed, which the picker cannot know from the folder list.
	if strings.Contains(code, "Every request recorded so far came from") {
		t.Error("insights.js still claims every request came from one folder; that is false " +
			"whenever any request was recorded before the project column existed")
	}
}
