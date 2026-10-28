package proxy

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// shadowDiff returns the two savings figures of one priced event, in dollars, so shadow mode can
// log them side by side.
//
// legacy is the bundle the old logic books: the baseline-minus-billed saving plus the keep-alive
// and cachesplit credits, which dash.Event.Price keeps in their own columns outside
// BaselineCostUSD. It reads the columns the legacy Price already set and never calls Price, so call
// it after e.Price.
//
// cfUSD is the counterfactual bundle: DeltaUSD (ops plus the breaks charged to a component) plus
// the rescue, split and TTL effects. A break nobody owns (UnattributedBreakUSD) is not in it.
// Both figures are 0 when the event has no counterfactual Result.
func shadowDiff(e *dash.Event, p modelinfo.Price) (legacy, cfUSD float64) {
	if !e.HasCounterfactual() {
		return 0, 0
	}
	legacy = (e.BaselineCostUSD - e.CostUSD) + e.KeepAliveSavedUSD + e.CachesplitSavedUSD
	fig := e.CounterfactualFigure(p)
	return legacy, fig.DeltaUSD + fig.KeepAliveSavedUSD + fig.SplitUSD + fig.TTLUSD
}

// logShadow writes the cf_shadow line for one completed request.
func logShadow(e *dash.Event, p modelinfo.Price) {
	legacy, cfUSD := shadowDiff(e, p)
	fig := e.CounterfactualFigure(p)
	names := make([]string, 0, len(fig.ByComponent))
	for k := range fig.ByComponent {
		names = append(names, k)
	}
	sort.Strings(names)
	slog.Info("cf_shadow", "session", e.SessionID, "thread", e.ThreadID,
		"legacy_usd", legacy, "cf_usd", cfUSD, "delta_usd", cfUSD-legacy,
		"components", strings.Join(names, ","), "unpriced", len(fig.Unpriced))
}
