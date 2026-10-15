package dash

import (
	"strings"
	"testing"
)

// The chart is a USAGE-OVER-TIME chart: one stacked bar per removable thing, each bar divided into
// time buckets, each bucket coloured by whether the thing was invoked in that period.
//
// WHY THIS FORM, after a heatmap and a ranked bar chart both fell short. The ranked bar answered
// "what costs most" correctly but said nothing about WHEN — and the decision the page exists to
// support is not only "is this expensive" but "did I stop using it", which is a pattern over time.
// The heatmap did carry that pattern and was unreadable: 160 small cells, and no legible way to
// tell a used day from a carried one.
//
// A stacked bar per capability is the same information at a density a reader can take in: far
// fewer, wider marks, and the pattern a reader is hunting — green early, red later — is the shape
// of the bar itself rather than something to be decoded from a colour ramp.
func TestTheChartShowsUsageOverTimePerRemovableThing(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))

	// Stacked, because each bar is one capability divided into periods. An unstacked bar per
	// period would put the same capability in several places.
	if !strings.Contains(code, "insUsageBars") {
		t.Error("no insUsageBars: the chart needs an explicit per-capability, per-bucket row set")
	}
	if !strings.Contains(code, "stack:") {
		t.Error("the bars are not stacked, so a capability's periods are not one bar")
	}
	// Buckets follow the window, so a month is never 30 thin slivers and a week is never one
	// undivided block. This is the rule that keeps the chart legible at any range.
	if !strings.Contains(code, "insBucketDays") {
		t.Error("no insBucketDays: bucket width must follow the selected window, or a long range " +
			"renders as the same dense sliver grid the heatmap did")
	}
	// A coarse range picker: weeks and months, not days. The reader asked for patterns over time,
	// and a per-day picker is both more precision than the question needs and more clicks.
	if !strings.Contains(code, "insRangePicker") {
		t.Error("no insRangePicker: the window is chosen in weeks and months, not by exact date")
	}
}

// GROUPED BY WHAT CAN ACTUALLY BE REMOVED, and nothing else gets its own bar.
//
// A plugin's skills collapse into one bar for the plugin, because `skillOverrides` has no effect
// on them — verified on real traffic — so the plugin is the only unit that can be switched off.
// Giving each of its 15 skills a bar would offer 15 removals that do not exist, and would crowd
// out the items a reader can act on. Measured: 55 removable items become 28 bars.
func TestOnlyIndividuallyRemovableThingsGetTheirOwnBar(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))
	if !strings.Contains(code, "insRemovalGroup") {
		t.Error("no insRemovalGroup: bars must be keyed by the removal unit (plugin, server or " +
			"standalone item), or the chart offers removals that are not possible")
	}
	// BUILT-INS APPEAR, BUT AS A BASELINE — separated, differently coloured, never offered.
	//
	// Excluding them entirely was the first cut and it made the chart useless on real data: zero
	// removable capabilities have ever been invoked here, so every bar was red and the picture
	// carried no pattern at all. The green/red contrast that makes the form readable comes from
	// the tools that ARE used, and on this store those are all built-ins.
	//
	// They are still never actionable: a built-in's weight is real but removing one breaks the
	// agent, so it sits below a divider in its own colour and no removal is offered for it. The
	// distinction has to survive in the code, or the chart starts recommending the unremovable.
	if !strings.Contains(code, "insIsBaseline") {
		t.Error("no insIsBaseline: built-ins must be marked as a baseline group rather than " +
			"either hidden or mixed in with things the reader can act on")
	}
	if !strings.Contains(code, "--ins-baseline") {
		t.Error("the baseline bars share a colour with the actionable ones, so the chart offers " +
			"no way to tell what can be removed from what cannot")
	}
}

// Colour answers one question per segment: was this earning its keep in that period?
//
// Red for carried-and-never-invoked, green for invoked. Not an intensity ramp: a ramp cannot
// distinguish "never used" from "barely used", and that distinction is the whole decision.
func TestASegmentsColourSaysWhetherItWasEarningItsKeep(t *testing.T) {
	css := readUI(t, "ui/insights.css")
	code := stripJSComments(readUI(t, "ui/insights.js"))
	for _, tok := range []string{"--ins-wasted", "--ins-used"} {
		if !strings.Contains(css, tok+":") {
			t.Errorf("%s is not declared in insights.css", tok)
		}
		if !strings.Contains(code, "v('"+tok+"'") {
			t.Errorf("the chart does not read %s, so its segment colours are its own and can "+
				"drift from the tables", tok)
		}
	}
	wasted := cssTokenValue(t, css, "--ins-wasted")
	used := cssTokenValue(t, css, "--ins-used")
	if wasted == used {
		t.Fatalf("wasted and used are both %q; the one distinction the chart exists to draw", used)
	}
	if !strings.Contains(wasted, "var(--bad)") && !strings.Contains(wasted, "var(--pal-red)") {
		t.Errorf("--ins-wasted = %q; waste is the finding to act on and carries the alarm "+
			"colour, which is the direction the verdict palette already follows", wasted)
	}
	if !strings.Contains(used, "var(--good)") && !strings.Contains(used, "var(--pal-green)") {
		t.Errorf("--ins-used = %q; a period where the capability earned its weight is the one "+
			"piece of good news on this page", used)
	}
}

// A DAY'S SHADE CARRIES WHAT IT COST, which is the question "why is every day the same?" answered.
//
// The chart collapsed each day's carriage to a boolean — was this carried, yes or no — so a day
// with 860 requests rendered identically to one with 206, and the reader spotted it: "How it
// possible that i spent same amount of money Wasted on same plugin each day ? if every day i
// doind different amount of requests is should be different wast amount".
//
// The per-day counts were always in the timeline (138, 601, 358, 403, 326, 359 for one skill,
// tracking request volume); the chart threw them away. Now the shade within the day's own colour
// carries the amount, so a heavy day is a deeper red.
//
// SCALED AGAINST THE BUSIEST DAY, not linearly from zero. Measured on the real store: counts run
// from 1 to 601 with a median of 358, so a linear ramp would put almost every day in the top two
// shades and show nothing. The floor matters as much: a day that cost a little must still be
// clearly coloured rather than fading into the surface, or "cheap" becomes indistinguishable from
// "not carried at all" — the distinction the whole chart turns on.
func TestADaysShadeCarriesWhatItCostThatDay(t *testing.T) {
	code := stripJSComments(readUI(t, "ui/insights.js"))

	if !strings.Contains(code, "insDayShade") {
		t.Error("no insDayShade: a day's cost must reach its colour, or every day of a bar looks " +
			"the same however much it actually cost")
	}
	// The boolean collapse must be gone: carried has to keep its COUNT.
	if strings.Contains(code, "row.carried[ci] = 1") {
		t.Error("carried is still collapsed to 1, so the per-day amount is discarded before it " +
			"can reach the chart")
	}
	// A floor, so a cheap day is still visibly carried rather than invisible.
	if !strings.Contains(code, "INS_SHADE_FLOOR") {
		t.Error("no INS_SHADE_FLOOR: without a minimum opacity a cheap day fades into the " +
			"surface and reads as a day the capability was not carried at all")
	}
}
