package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
)

// checkSpend refuses a request when the tenant is over its monthly cap.
//
// The cap exists because the upstream credential is the OPERATOR's: we deliberately
// never ask users for their own provider keys, and the direct consequence is that one
// tenant's runaway agent spends the organisation's budget. So this is a required part
// of the design rather than a knob.
//
// It fails OPEN on a lookup error. A broken metrics query is our problem, not the
// user's, and stopping someone's agent because a SUM failed would be the wrong trade —
// the cap is a budget guard, not a security boundary.
func (h *Handler) checkSpend(tn *Tenancy) error {
	if h.opts.Spend == nil || tn == nil || tn.MonthlyCapUSD <= 0 {
		return nil
	}
	usd, err := h.spend.get(tn.ID, h.opts.Spend.MonthToDateUSD)
	if err != nil {
		slog.Warn("context-guru: could not read month-to-date spend; allowing the request",
			"tenant", tn.ID, "err", err)
		return nil
	}
	if usd < tn.MonthlyCapUSD {
		return nil
	}
	// 402 rather than 429: this is not "slow down", it is "this account has spent its
	// budget", and retrying will not help until someone raises the cap or the month
	// turns over. Saying so precisely is the difference between a user filing a bug and
	// a user asking for a bigger budget.
	//
	// Counted here, not at the write: failAuth cannot tell a 402 from any other refusal
	// and does not count them, so this is the single site and it fires exactly once.
	recordRefusal(refuseSpendCap, tn.ID)
	return statusError{http.StatusPaymentRequired, fmt.Sprintf(
		"monthly spend cap reached: $%.2f of $%.2f used this month. "+
			"Ask a context-guru manager to raise the cap, or wait for the next calendar month.",
		usd, tn.MonthlyCapUSD)}
}

// InvalidateSpend drops a tenant's cached spend figure, so raising a cap takes effect
// on the next request instead of after the cache TTL.
func (h *Handler) InvalidateSpend(tenantID string) {
	if h != nil && h.spend != nil {
		h.spend.invalidate(tenantID)
	}
}
