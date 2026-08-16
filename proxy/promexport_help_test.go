package proxy

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/metrics"
)

// TestInProcessSeriesDeclareWhyTheyDisagreeWithTheDashboard.
//
// Observed live: /metrics reported 24 requests / 28,644 tokens-before while the dashboard
// beside it reported 26 / 28,656. Both were right. cg_* comes from the in-memory
// aggregator — it starts at 0 when the process starts and sums every tenant — while the
// dashboard reads the persistent database for one tenant. Nothing anywhere said so, so
// the only available reading was "one of these two is broken".
//
// HELP text is where it has to be said, because it travels with the metric into Grafana's
// metric browser and panel tooltips, and because the Grafana dashboards themselves are
// owned elsewhere.
func TestInProcessSeriesDeclareWhyTheyDisagreeWithTheDashboard(t *testing.T) {
	agg := metrics.NewAggregator()
	h := New(nil, nil, agg, Options{})
	body := h.renderMetrics()

	// The families a reader compares against the dashboard's headline numbers.
	for _, name := range []string{
		"cg_requests_total", "cg_tokens_before_total", "cg_tokens_after_total",
		"cg_saved_tokens_unique_total", "cg_billed_tokens_total", "cg_savings_ratio",
	} {
		help := helpLine(body, name)
		if help == "" {
			t.Errorf("%s has no HELP line at all", name)
			continue
		}
		// The two causes, and where the persistent numbers live instead.
		for _, want := range []string{"THIS PROCESS", "every tenant", "cg_tenant_"} {
			if !strings.Contains(help, want) {
				t.Errorf("HELP for %s does not mention %q, so a reader comparing it with the "+
					"dashboard has no way to know why they differ:\n  %s", name, want, help)
			}
		}
	}

	// And it must NOT be claimed of the series that are already database-backed: saying
	// "this resets on restart" about a persistent number is the same class of bug.
	for _, name := range []string{"cg_dash_db_bytes", "cg_archive_sessions_total"} {
		if help := helpLine(body, name); strings.Contains(help, "THIS PROCESS") {
			t.Errorf("%s reads from the store, so the in-process caveat is false for it:\n  %s",
				name, help)
		}
	}
}

// helpLine returns the HELP text for one metric name, or "" when it has none.
func helpLine(body, name string) string {
	for _, ln := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(ln, "# HELP "+name+" "); ok {
			return rest
		}
	}
	return ""
}
