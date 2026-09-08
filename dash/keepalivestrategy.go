package dash

import (
	"database/sql"
	"net/http"
)

// The manager-controlled keep-alive strategy tab's read side: one ledger, grouped by
// tenant, for one strategy — see proxy/keepalivestrategy.go for how a strategy is
// resolved and audited, and docs/superpowers/specs/2026-08-25-keepalive-strategies-design.md
// for the design.

// StrategyLedgerRow is one tenant's economics under a strategy.
type StrategyLedgerRow struct {
	TenantID string  `json:"tenant_id"`
	Pings    int64   `json:"pings"`
	PingUSD  float64 `json:"ping_usd"`
	SavedUSD float64 `json:"saved_usd"`
	NetUSD   float64 `json:"net_usd"`
}

// StrategyLedgerView is one strategy's whole answer: the totals, and the per-tenant
// breakdown behind them, costliest first.
type StrategyLedgerView struct {
	StrategyID string              `json:"strategy_id"`
	Pings      int64               `json:"pings"`
	PingUSD    float64             `json:"ping_usd"`
	SavedUSD   float64             `json:"saved_usd"`
	NetUSD     float64             `json:"net_usd"`
	Tenants    []StrategyLedgerRow `json:"tenants"`
}

// StrategyLedger computes one strategy's economics, built the same way KeepAliveSessions
// already is (dash/keepalive.go), filtered by keepalive_strategy_id instead of by
// Filter.Tenant.
//
// Pings, PingUSD, and now SavedUSD are all EXACT: every ping row this strategy caused
// carries its id (proxy's record1), and every REAL row a ping later rescued carries the
// same id (keeper.arrive, threaded through capture — see dash/schema.go's
// keepalive_strategy_id comment). A tenant running more than one strategy over time no
// longer has its whole lifetime credit repeated under each one — a credited request is
// attributed to whichever strategy's ping(s) actually preceded it, once.
func (d *DB) StrategyLedger(strategyID string) (*StrategyLedgerView, error) {
	out := &StrategyLedgerView{StrategyID: strategyID, Tenants: []StrategyLedgerRow{}}
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT tenant_id, COUNT(*), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE keepalive = 1 AND keepalive_strategy_id = ?
		GROUP BY tenant_id ORDER BY 3 DESC`, strategyID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row StrategyLedgerRow
		if err := rows.Scan(&row.TenantID, &row.Pings, &row.PingUSD); err != nil {
			rows.Close()
			return nil, err
		}
		out.Tenants = append(out.Tenants, row)
		out.Pings += row.Pings
		out.PingUSD += row.PingUSD
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out.Tenants {
		row := &out.Tenants[i]
		var saved sql.NullFloat64
		if err := d.sql.QueryRowContext(d.readCtx(), `SELECT SUM(`+kaSaved("r.")+`) FROM requests r
			WHERE r.tenant_id = ? AND r.keepalive_saved_usd > 0 AND r.keepalive_strategy_id = ?`,
			row.TenantID, strategyID).Scan(&saved); err != nil {
			return nil, err
		}
		row.SavedUSD = saved.Float64
		row.NetUSD = row.SavedUSD - row.PingUSD
		out.SavedUSD += row.SavedUSD
	}
	out.NetUSD = out.SavedUSD - out.PingUSD
	return out, nil
}

// keepAliveStrategyRoutes is this feature's one read route, appended to routes in
// api.go for the same reason every other feature's are: that table is what both
// scoping tests walk.
func (a *API) keepAliveStrategyRoutes() []route {
	return []route{
		{"GET /api/keepalive/strategies/{id}/ledger", scopeManager, a.keepAliveStrategyLedger},
	}
}

// keepAliveStrategyLedger serves one strategy's ledger. Not tenant-scoped by a.scope —
// like /api/config and /api/benchmarks, this is a server-wide view spanning every
// tenant a strategy touched, not one tenant's own data.
func (a *API) keepAliveStrategyLedger(w http.ResponseWriter, r *http.Request) {
	if !a.requireManager(w, r, "a strategy's ledger") {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		httpErr(w, http.StatusBadRequest, "name the strategy")
		return
	}
	led, err := a.db(r).StrategyLedger(id)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, led)
}
