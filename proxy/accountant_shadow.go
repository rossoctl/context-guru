package proxy

import (
	"log/slog"
	"strings"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/dash"
)

// logFallback writes the acc_fallback line for an event the accountant library could not price, so it
// books no saving,
// with the reason (dash.Fallback*).
//
// why is set for no_result only: it names which of that reason's causes applied (noResultWhy).
func logFallback(e *dash.Event, why string) {
	r, _ := e.AccountantResult()
	attrs := []any{"session", e.SessionID, "thread", e.ThreadID,
		"reason", e.AccountantFallback(), "checks", checkNames(r), "recomputed", r.Recomputed}
	if why != "" && e.AccountantFallback() == dash.FallbackNoResult {
		attrs = append(attrs, "why", why)
	}
	slog.Info("acc_fallback", attrs...)
}

// checkNames lists the names of the failed library checks, comma-separated, in order.
func checkNames(r accountant.Result) string {
	n := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		n[i] = c.Check
	}
	return strings.Join(n, ",")
}
