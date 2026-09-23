package proxy

import (
	"log/slog"

	"github.com/rossoctl/context-guru/dash"
)

// SavingsScope is one dash.SavingsTotals, reshaped for /stats' JSON wire — same fields,
// same meaning, so a caller reading dash's own doc comments understands this shape too.
type SavingsScope struct {
	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	NetSavedUSD     float64 `json:"net_saved_usd"`

	CachesplitSavedUSD float64 `json:"cachesplit_saved_usd"`

	KeepAlivePingUSD  float64 `json:"keepalive_ping_usd"`
	KeepAliveSavedUSD float64 `json:"keepalive_saved_usd"`
	KeepAliveNetUSD   float64 `json:"keepalive_net_usd"`

	TotalSavedUSD float64 `json:"total_saved_usd"`
}

func savingsScope(t *dash.SavingsTotals) *SavingsScope {
	return &SavingsScope{
		CostUSD: t.CostUSD, BaselineCostUSD: t.BaselineCostUSD, CGLLMCostUSD: t.CGLLMCostUSD,
		NetSavedUSD:        t.NetSavedUSD,
		CachesplitSavedUSD: t.CachesplitSavedUSD,
		KeepAlivePingUSD:   t.KeepAlivePingUSD, KeepAliveSavedUSD: t.KeepAliveSavedUSD,
		KeepAliveNetUSD: t.KeepAliveNetUSD,
		TotalSavedUSD:   t.TotalSavedUSD,
	}
}

// SavingsStats is /stats' savings block: the same combined figure the dashboard shows,
// in three scopes, computed through dash.DB.SavingsTotals directly rather than through
// dash.API — this proxy already holds the DB (h.rec), and for a single local user's
// data the query is cheap enough to run synchronously.
type SavingsStats struct {
	// Current is the session that most recently sent a real (non-ping) request through
	// this proxy. Absent (not zeroed) until the first real request arrives.
	Current *SavingsScope `json:"current,omitempty"`
	// Live sums every session the keep-alive keeper currently considers active.
	Live *SavingsScope `json:"live,omitempty"`
	// All is process-wide, all time, all sessions — Overview()'s own figure, minus the
	// columns /stats does not need.
	All *SavingsScope `json:"all,omitempty"`
}

// setLastSession records the session id of a real (non-ping) request that just arrived,
// for /stats' "current" scope. Never called from the keep-alive ping path.
func (h *Handler) setLastSession(session string) {
	if session == "" {
		return
	}
	h.lastSessionMu.Lock()
	h.lastSession = session
	h.lastSessionMu.Unlock()
}

func (h *Handler) getLastSession() string {
	h.lastSessionMu.Lock()
	defer h.lastSessionMu.Unlock()
	return h.lastSession
}

// savingsStats builds /stats' three-scope savings block. Single-tenant + h.rec != nil
// only (same guard as snap.Pipeline above — hosted stays excluded, a manager's
// service-wide view belongs on the dashboard). Fails open: any query error is logged
// and that scope is simply omitted, never a 500.
func (h *Handler) savingsStats() *SavingsStats {
	if h.opts.Tenants != nil || h.rec == nil {
		return nil
	}
	db := h.rec.DB()
	out := &SavingsStats{}
	if all, err := db.SavingsTotals(dash.Filter{TenantAll: true}); err == nil {
		out.All = savingsScope(all)
	} else {
		slog.Default().Warn("cg.stats_savings_all_failed", "err", err)
	}
	if live := h.keeper.LiveSessionKeys(); len(live) > 0 {
		// One grouped query for every live session (Filter.SessionIn), not one query per
		// session — at maxKeepAliveSessions that would be up to 1024 sequential round trips.
		if sum, err := db.SavingsTotals(dash.Filter{TenantAll: true, SessionIn: live}); err == nil {
			out.Live = savingsScope(sum)
		} else {
			slog.Default().Warn("cg.stats_savings_live_failed", "err", err)
		}
	}
	if session := h.getLastSession(); session != "" {
		if cur, err := db.SavingsTotals(dash.Filter{TenantAll: true, Session: session}); err == nil {
			out.Current = savingsScope(cur)
		} else {
			slog.Default().Warn("cg.stats_savings_current_failed", "err", err)
		}
	}
	return out
}
