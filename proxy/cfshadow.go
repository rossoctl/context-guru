package proxy

import (
	"log/slog"
	"sort"
	"strings"

	cf "github.com/rossoctl/context-guru/counterfactual"
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
// Neither figure includes the summarizer's (CGLLM) spend, whereas Play's net subtracts it, so the
// log must not be compared with Play directly.
// Both figures are 0 when the event has no counterfactual Result.
func shadowDiff(e *dash.Event, p modelinfo.Price) (legacy, cfUSD float64) {
	if !e.HasCounterfactual() {
		return 0, 0
	}
	return legacyCF(e, e.CounterfactualFigure(p))
}

// legacyCF splits shadowDiff's work so logShadow prices the Result once.
func legacyCF(e *dash.Event, fig dash.CFFigure) (legacy, cfUSD float64) {
	legacy = (e.BaselineCostUSD - e.CostUSD) + e.KeepAliveSavedUSD + e.CachesplitSavedUSD
	return legacy, fig.DeltaUSD + fig.KeepAliveSavedUSD + fig.SplitUSD + fig.TTLUSD
}

// logShadow writes the cf_shadow line for one completed request. It carries what decides whether
// counterfactual mode would publish the figure: Result.Recomputed, the names of the failed library
// checks, and the fallback reason counterfactual mode would give ("" = it would publish cf_usd).
func logShadow(e *dash.Event, p modelinfo.Price) {
	fig := e.CounterfactualFigure(p)
	legacy, cfUSD := legacyCF(e, fig)
	names := make([]string, 0, len(fig.ByComponent))
	for k := range fig.ByComponent {
		names = append(names, k)
	}
	sort.Strings(names)
	r, _ := e.Counterfactual()
	slog.Info("cf_shadow", "session", e.SessionID, "thread", e.ThreadID,
		"legacy_usd", legacy, "cf_usd", cfUSD, "delta_usd", cfUSD-legacy,
		"components", strings.Join(names, ","), "unpriced", len(fig.Unpriced),
		"recomputed", r.Recomputed, "checks", checkNames(r), "fallback", e.CounterfactualFallback())
}

// logFallback writes the cf_fallback line for an event whose figures counterfactual mode left on
// the legacy rules, with the reason (dash.Fallback*).
func logFallback(e *dash.Event) {
	r, _ := e.Counterfactual()
	slog.Info("cf_fallback", "session", e.SessionID, "thread", e.ThreadID,
		"reason", e.CounterfactualFallback(), "checks", checkNames(r), "recomputed", r.Recomputed)
}

// checkNames lists the names of the failed library checks, comma-separated, in order.
func checkNames(r cf.Result) string {
	n := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		n[i] = c.Check
	}
	return strings.Join(n, ",")
}
