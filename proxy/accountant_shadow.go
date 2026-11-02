package proxy

import (
	"log/slog"
	"strings"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/dash"
)

// logFallback writes the acc_fallback line for an event whose figures stayed on the legacy rules,
// with the reason (dash.Fallback*).
func logFallback(e *dash.Event) {
	r, _ := e.AccountantResult()
	slog.Info("acc_fallback", "session", e.SessionID, "thread", e.ThreadID,
		"reason", e.AccountantFallback(), "checks", checkNames(r), "recomputed", r.Recomputed)
}

// checkNames lists the names of the failed library checks, comma-separated, in order.
func checkNames(r accountant.Result) string {
	n := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		n[i] = c.Check
	}
	return strings.Join(n, ",")
}
