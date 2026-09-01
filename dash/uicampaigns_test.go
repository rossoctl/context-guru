package dash

import (
	"strings"
	"testing"
)

// The Campaigns tab's filters are not a display convenience — they decide what a bulk
// create actually enforces. This pins the wiring that makes that true, because the failure
// mode is silent and expensive: a filter bar that narrowed the TABLE but not the create
// body would show a manager two accounts, then create live keep-alive strategies for
// eighteen, and nothing on the page would say so.
//
// A static source check rather than a browser test because this repo ships no JS test
// harness at all (no package.json, no node in `make lint`) — and because the property
// worth protecting is a one-line one: the create call's cells come from the same function
// the table renders from.
func TestCampaignCreateSubmitsTheFilteredCellsNotTheWholePayload(t *testing.T) {
	src := readUI(t, "ui/campaigns.js")

	// One definition of "the cells this campaign is about", used by all three readers.
	if !strings.Contains(src, "function campFilteredCells(") {
		t.Fatal("campFilteredCells is gone; the filters, the count and the create body no " +
			"longer share one definition of which cells a campaign covers")
	}
	create := sliceFunc(t, src, "async function createCampaignFromPending(")
	if !strings.Contains(create, "campFilteredCells(") {
		t.Error("createCampaignFromPending no longer builds its body from campFilteredCells: " +
			"the preview's filters would narrow the table while the create still enforces " +
			"every cell in the payload")
	}
	// The narrowed payload must carry a matching user list, since the server takes an
	// uploaded suggest payload verbatim.
	if !strings.Contains(create, "users:") {
		t.Error("the create body no longer narrows `users` to the filtered cells")
	}
	// Guard the specific regression of submitting camp.pending wholesale again.
	if strings.Contains(create, "suggest: camp.pending") ||
		strings.Contains(create, "suggest }") {
		t.Error("the create body submits the whole pending payload again, bypassing the filters")
	}
}

// The train/test panel exists and is wired to the holdout route, and its copy says what the
// train figure actually is. The whole reason the panel was built rather than a simple
// "prediction window" control is that a suggest cell's predicted saving is IN-SAMPLE — a UI
// that offered a time control over that number without saying so would be presenting a
// measure of fit as a forecast, which this codebase's honesty convention forbids.
func TestCampaignHoldoutPanelNamesTheInSampleFigureForWhatItIs(t *testing.T) {
	src := readUI(t, "ui/campaigns.js")

	if !strings.Contains(src, "kvcache/suggest/holdout") {
		t.Fatal("the campaigns tab no longer calls the holdout route")
	}
	for _, id := range []string{
		"camp-train-from", "camp-train-to", "camp-test-from", "camp-test-to",
		"camp-holdout-run", "camp-holdout-table",
	} {
		if !strings.Contains(src, id) {
			t.Errorf("the train/test panel is missing its %q control", id)
		}
	}
	// The copy must call the train figure in-sample somewhere. Not a style preference: it
	// is the one sentence that stops a reader treating the bigger of the two numbers as
	// the prediction.
	if !strings.Contains(src, "IN-SAMPLE") && !strings.Contains(src, "in-sample") {
		t.Error("the train/test copy no longer says the train figure is in-sample; without " +
			"that the panel reads as two equally valid predictions rather than a fit and a " +
			"forecast")
	}
	// A retention percentage must never be rendered unconditionally: the server sets
	// retention_known false where the ratio is undefined (no comparable cells, or a train
	// total of exactly zero).
	if !strings.Contains(src, "retention_known") {
		t.Error("the retention tile no longer checks retention_known, so an undefined ratio " +
			"would render as a number")
	}
	// A missing test figure must render as its own reason, never as $0.00 — "nothing was
	// measured" and "this arm saved nothing" are opposite findings.
	if !strings.Contains(src, "test_known") {
		t.Error("the holdout table no longer checks test_known before printing a test dollar " +
			"figure")
	}
}

// sliceFunc returns the source of one function: from its declaration to the next top-level
// declaration. Crude, and enough — every function in these files starts at column zero.
func sliceFunc(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("could not find %q in the source", decl)
	}
	rest := src[i+len(decl):]
	// The next line that begins a new top-level declaration ends this one.
	for _, next := range []string{"\nfunction ", "\nasync function ", "\nconst ", "\n// ──"} {
		if j := strings.Index(rest, next); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}
