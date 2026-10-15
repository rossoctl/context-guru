package dash

import (
	"strings"
	"testing"
)

// The chart is a RANKED BAR CHART of waste per capability, not a heatmap of capability against
// time.
//
// WHY THE FORM CHANGED. The heatmap was the right picture for a question this data cannot yet
// answer. It shows WHEN each capability was used, which is worth seeing once there are months of
// history and real phase changes in them — and on the measured store there are eight days in which
// nearly every capability was carried every day. A heatmap of near-constant data is a wall of one
// colour whatever library draws it, and it was read exactly that way: "it's not working good".
//
// Meanwhile the question the page exists to answer — what is costing me money for nothing — has a
// clear ranking behind it: 55 removable items, $31.70 in total, the worst at $7.14. A bar per
// capability, longest first, says that at a glance and needs no hover to be read.
//
// This pins the form rather than the pixels, because the pull back to a heatmap is strong: it
// looks more sophisticated, and it was in fact the first thing I reached for.
func TestTheChartRanksWasteRatherThanPlottingItOverTime(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))

	if !strings.Contains(code, "type: 'bar'") {
		t.Error("the chart is not a bar series. A ranked bar per capability is what answers " +
			"\"what is costing me money for nothing\"; a heatmap answers \"when did I use this\", " +
			"which needs months of history to say anything")
	}
	if strings.Contains(code, "type: 'heatmap'") {
		t.Error("a heatmap series is still configured. On eight days where everything was " +
			"carried every day it renders as a wall of one colour, which is what the reader saw")
	}
	// Ranked, and the ranking has to be by what the reader acts on: money, falling back to tokens
	// where a model has no published rate. A bar chart in arbitrary order is a worse table.
	if !strings.Contains(code, "insBarRows") {
		t.Error("no insBarRows: the chart needs an explicit ranked row set, so the order is a " +
			"decision with a reason rather than whatever the document happened to return")
	}
	// Horizontal, because the labels are capability names: 'mcp__web-search__google_pse_search'
	// does not fit under a vertical bar at any readable font size.
	if !strings.Contains(code, "yAxis: { type: 'category'") {
		t.Error("the bars are not horizontal. Capability names are long, and a vertical bar " +
			"chart would either clip them or rotate them to unreadability")
	}
}

// Only items that can actually be REMOVED get a bar, and the chart says what it left out.
//
// A bar for a built-in would be a bar the reader cannot act on — the weight is real and removing
// it breaks the agent — and putting the biggest unactionable thing at the top of a chart about
// what to remove is the one misreading this view must not invite. That is the same rule the
// tables already follow via scope; the chart has to follow it too or the two disagree.
func TestTheBarsOnlyShowWhatCanBeRemoved(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))
	if !strings.Contains(code, "INS_SCOPE_ITEM") && !strings.Contains(code, "scope !== 'none'") {
		t.Error("insBarRows does not filter on removal scope, so built-ins would be charted as " +
			"waste the reader could act on")
	}
	if !strings.Contains(code, "insBarFootnote") {
		t.Error("no footnote naming what the chart is not showing. A chart capped at N bars out " +
			"of 55 reads as the whole inventory unless it says otherwise")
	}
}
