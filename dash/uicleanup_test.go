package dash

import (
	"strings"
	"testing"
)

// A PROJECT WITH NO MEASURED TRAFFIC GETS AN EXPLANATION, NOT A BROKEN OR BLANK PAGE.
//
// THE BUG, reported as "if I select ai-native-company I get a bug": that project's 115 requests
// were all imported from Claude Code's transcripts, so Cleanup has nothing to price and the
// document is legitimately empty. The view had no empty state at all — it went straight to the
// range picker and the chart, both of which index into a timeline that has no days.
//
// So the view checks coverage FIRST, says plainly that nothing here was measured and why that
// means no figures rather than no waste, says that traffic from now on is measured, and keeps the
// project picker so the reader can choose another.
func TestTheCleanupViewExplainsAProjectWithNoMeasuredTraffic(t *testing.T) {
	src := readUI(t, "ui/insights.js")
	code := stripComments(src)
	if !strings.Contains(code, "measured_requests") {
		t.Fatal("the Cleanup view never reads coverage.measured_requests, so it cannot tell a " +
			"project with nothing to report from one that was never measured")
	}
	lits := strings.ToLower(strings.Join(
		jsStringLiterals(code), " "))
	for _, want := range []string{"before", "does not mean nothing was wasted", "from now on is measured"} {
		if !strings.Contains(lits, want) {
			t.Errorf("the empty state does not mention %q. A reader who selected a project and "+
				"got no figures has to be told that it was never MEASURED — the alternative "+
				"reading, that it carried nothing, is false — and that it will be from now on", want)
		}
	}
	// The History tab is gone, so the page must not send a reader there.
	if strings.Contains(lits, "history tab") {
		t.Error("the Cleanup view still points at the removed History tab")
	}
	// The early return has to come BEFORE the chart is built, or the chart still throws.
	i := strings.Index(code, "measured_requests")
	j := strings.Index(code, "const chart = insChart()")
	if i < 0 || j < 0 || i > j {
		t.Error("the coverage check runs after insChart(), so the chart still reads an empty " +
			"timeline before the explanation can render")
	}
}

// EACH TAB STATES ITS SPAN AND WHETHER IT HAS MONEY, AND POINTS AT THE OTHER.
//
// The two tabs answer different questions over different spans: History covers all of Claude
// Code's history including before context-guru existed, and has no money; Cleanup covers only the
// time since install, and is the only place a dollar figure comes from. A reader told neither reads
// History's longer span as better evidence for money it does not have. Agreed with the user as the
// framing for the dashboard skill, which opens both tabs side by side.
