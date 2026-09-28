package proxy

import (
	"net/http"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The Strategies page's per-tenant max_pings economics — see PHASE2.md's findings P2-1c/P2-4/
// P2-5 (2026-09-28 keep-alive predictor study). A single, pooled net-vs-max_pings curve was
// tried first and REJECTED: the study found a flat cap applied everywhere helps some tenants
// and harms others, some of them at every max_pings value — for those, no cap is a fix, because
// keep-alive itself does not pay off on their own traffic. A pooled curve would show one line
// that is wrong for a third of the fleet, which is worse than showing nothing.
//
// So this computes ONE tenant's own curve (dash.KeepAliveCalc, already built and tested for the
// Keep-alive tab's own calculator and the Strategies form's live forecast) for every tenant with
// enough history to have one, and returns the SUMMARY a table needs: the sample size a reader
// must see before trusting the shape, the actually-in-force setting right now (not a second
// resolution engine — keeper.bestStrategyFor is the SAME function the request path itself
// calls), the modelled optimum, and whether that optimum still loses money.
//
// The full 24-rung ladder per tenant is deliberately NOT returned here — GET /api/keepalive/
// calc?tenant=<id> (already built, already manager-overridable) serves that on demand, the same
// click-to-drill-down split the Campaigns tab's own overview/tenant-drilldown already uses.

// tenantMaxPingsRow is one tenant's keep-alive max_pings summary.
type tenantMaxPingsRow struct {
	TenantID string `json:"tenant_id"`
	// DecisionPoints is the addressable-expiry count this tenant's own curve was replayed
	// from — dash.KeepAliveCalc's own Addressable field. ThinData mirrors
	// dash.KeepAliveRecommend's OWN admission floor (dash.KeepAliveMinDecisionPoints /
	// KeepAliveMinRequests) so a tenant flagged thin here is never one KeepAliveRecommend
	// would happily score.
	DecisionPoints int64 `json:"decision_points"`
	Requests       int64 `json:"requests"`
	ThinData       bool  `json:"thin_data"`
	// CurrentMaxPings is what keeper.bestStrategyFor resolves for this tenant RIGHT NOW — the
	// live resolution chain itself, not a second simulation of it. CurrentStrategyID empty
	// means no strategy currently matches (the tenant's own account switch or nothing).
	CurrentStrategyID string `json:"current_strategy_id,omitempty"`
	CurrentMaxPings   int    `json:"current_max_pings,omitempty"`
	// Priced false means every dollar field below is meaningless — this tenant's own addressable
	// misses carry a model this operator's price list does not cover. The counts above still
	// stand; nothing here falls back to a blended rate.
	Priced bool `json:"priced"`
	// OptimalMaxPings/OptimalNetUSD are the CalcRow dash.KeepAliveCalc marked Optimal: the
	// highest-NetUSD rung on THIS tenant's own replay, ties keeping the lower K.
	OptimalMaxPings int     `json:"optimal_max_pings,omitempty"`
	OptimalNetUSD   float64 `json:"optimal_net_usd"`
	// OffRecommended is true when even the optimal rung nets zero or less: no max_pings value
	// on this tenant's own history would have turned a profit, so the honest answer is "off",
	// not "raise your cap" or "lower it a little" — the specific state P2-4 found the fleet
	// needs and a pooled curve could never show.
	OffRecommended bool `json:"off_recommended"`
}

// ctlKeepAliveMaxPingsByTenant serves every tenant's own max_pings summary, thinnest data
// included (flagged, never hidden — the design's whole point is that a manager sees the tenants
// a pooled number would have hidden). Tenants with no addressable expiry at all in this
// deployment's history are omitted: there is no curve to summarise, the same "no cached prompt
// to compute against" state the single-tenant calculator already renders.
func (h *Handler) ctlKeepAliveMaxPingsByTenant(w http.ResponseWriter, r *http.Request) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return
	}
	if h.rec == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"tenants": []tenantMaxPingsRow{},
			"note":    "dashboard capture is not enabled on this deployment",
		})
		return
	}
	tenants, err := h.registry().List()
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list tenants")
		return
	}
	db := h.rec.DB()
	priceFn := func(model string) (modelinfo.Price, bool) {
		if h.opts.Prices == nil || model == "" {
			return modelinfo.Price{}, false
		}
		return h.opts.Prices.Price(r.Context(), model)
	}
	now := time.Now()
	out := make([]tenantMaxPingsRow, 0, len(tenants))
	for _, t := range tenants {
		f := dash.Filter{Tenant: t.ID}
		prefix, model, err := db.AccountMedianPrefix(f)
		if err != nil {
			ctlErr(w, http.StatusInternalServerError, "could not read "+t.ID+"'s own history")
			return
		}
		if prefix <= 0 {
			continue // no addressable expiry at all: nothing to summarise for this tenant
		}
		calc, err := db.KeepAliveCalc(f, 0, prefix, model, priceFn, 0)
		if err != nil {
			ctlErr(w, http.StatusInternalServerError, "could not replay "+t.ID+"'s own gaps")
			return
		}
		row := tenantMaxPingsRow{
			TenantID: t.ID, DecisionPoints: calc.Addressable, Requests: calc.Requests,
			ThinData: calc.Addressable < dash.KeepAliveMinDecisionPoints ||
				calc.Requests < dash.KeepAliveMinRequests,
			Priced: calc.Priced,
		}
		if best := h.keeper.bestStrategyFor(t.ID, now); best != nil {
			row.CurrentStrategyID, row.CurrentMaxPings = best.ID, best.MaxPings
		}
		for _, cr := range calc.Rows {
			if cr.Optimal {
				row.OptimalMaxPings, row.OptimalNetUSD = cr.MaxPings, cr.NetUSD
				row.OffRecommended = calc.Priced && cr.NetUSD <= 0
				break
			}
		}
		out = append(out, row)
	}
	// Worst first: an off-recommended tenant is the case a pooled curve would have hidden
	// entirely, so it belongs at the top rather than wherever tenant.Registry.List's own
	// created_at ordering happens to put it. Ties broken by OptimalNetUSD ascending, so among
	// the tenants keep-alive already pays for, the ones paying it least sit closest to the
	// off-recommended group above them.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OffRecommended != out[j].OffRecommended {
			return out[i].OffRecommended
		}
		return out[i].OptimalNetUSD < out[j].OptimalNetUSD
	})
	writeJSON(w, http.StatusOK, map[string]any{"tenants": out})
}
