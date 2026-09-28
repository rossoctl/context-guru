package dash

import (
	"encoding/json"
	"net/http"
	"sort"
)

// The Strategies page's "preview before enabling" — see the 2026-08-25 keep-alive predictor
// study (PHASE2.md): a dollar estimate for a not-yet-live config disagreed by ~18x across five
// honest ways of computing the SAME quantity, so nothing here may present a single number as
// settled, and nothing here may write anything — it is a GET over history, never a mutation of
// keepalive_strategies (that table lives in tenant's registry, which this package does not even
// import).
//
// Reuses dash.KeepAliveCalc (dash/keepalive.go) rather than a second replay: it already turns
// one tenant's own idle gaps into a MaxPings ladder with two honest saving figures per rung —
// SavedUSD (the write-premium only, conservative) and ConvertibleUSD (the whole avoided miss,
// generous) — which is exactly the bracket this preview reports instead of a point estimate.
// What it does NOT model is disclosed in StrategyPreviewResult.Caveats rather than guessed at:
// MinPrefixTokens/MaxUSDPerPing/PredictorID all narrow what would really ping in production, so
// every figure here is an upper bound on pings and cost, never a forecast of them.

// strategyPreviewTenantCapIn mirrors tenant.TenantCap without importing the tenant package —
// dash's API deliberately has no dependency on the control plane (see dash/api.go's own doc
// comment: "it never writes a request row" — it also never reads tenant.Registry).
type strategyPreviewTenantCapIn struct {
	MaxPings *int `json:"max_pings"`
	Off      bool `json:"off"`
}

// StrategyPreviewInput is the candidate configuration to preview, decoded from the `candidate`
// query parameter's JSON. TenantIDs is the explicit target: for TargetAll, the caller passes
// every tenant with history (this package has no tenant registry to enumerate them from — see
// the file doc comment), typically the "all accounts" roster the Strategies form already loads
// for its own account picker.
type StrategyPreviewInput struct {
	IdleSeconds     float64                               `json:"idle_seconds"`
	MaxPings        int                                   `json:"max_pings"`
	MaxUSDPerTenant float64                               `json:"max_usd_per_tenant"`
	Models          []string                              `json:"models"`
	TenantIDs       []string                              `json:"tenant_ids"`
	TenantCaps      map[string]strategyPreviewTenantCapIn `json:"tenant_caps"`
}

// StrategyPreviewCohort is one tenant's own replay under the candidate.
type StrategyPreviewCohort struct {
	TenantID string `json:"tenant_id"`
	// Off/EffectiveMaxPings resolve TenantCaps + the candidate's own MaxPings exactly as
	// proxy's applyStrategy would live — see resolveCandidateMaxPings.
	Off               bool    `json:"off,omitempty"`
	EffectiveMaxPings int     `json:"effective_max_pings"`
	DecisionPoints    int64   `json:"decision_points"`
	Requests          int64   `json:"requests"`
	ThinData          bool    `json:"thin_data"`
	Priced            bool    `json:"priced"`
	Pings             int64   `json:"pings,omitempty"`
	PingUSD           float64 `json:"ping_usd,omitempty"`
	// NetUSDLow/NetUSDHigh bracket the same replay two honest ways — see the file doc
	// comment. Neither is "the" answer; the gap between them IS the answer to how much
	// this replay alone can tell you.
	NetUSDLow  float64 `json:"net_usd_low,omitempty"`
	NetUSDHigh float64 `json:"net_usd_high,omitempty"`
	// Harmed is true only when EVEN the generous bound (NetUSDHigh) does not clear zero —
	// a cohort a flat setting would have lost money on regardless of which honest estimate
	// you trusted. Never set on an unpriced or off cohort: absence of a number is not harm.
	Harmed bool `json:"harmed,omitempty"`
	// OverTenantBudget flags PingUSD at the effective rung exceeding the candidate's own
	// MaxUSDPerTenant, when one was declared. Advisory — see the file doc comment on why
	// this is not live-enforced.
	OverTenantBudget bool `json:"over_tenant_budget,omitempty"`
	// ModelCompatiblePct is the share of this tenant's own (non-keepalive) requests on a
	// model the candidate's Models list allows, 0-100. Omitted (not zero) when Models is
	// empty, since then every model is compatible and the figure says nothing.
	ModelCompatiblePct *float64 `json:"model_compatible_pct,omitempty"`
}

// StrategyPreviewResult is the whole preview.
type StrategyPreviewResult struct {
	Cohorts         []StrategyPreviewCohort `json:"cohorts"`
	TotalPingUSD    float64                 `json:"total_ping_usd"`
	TotalNetUSDLow  float64                 `json:"total_net_usd_low"`
	TotalNetUSDHigh float64                 `json:"total_net_usd_high"`
	HarmedTenants   []string                `json:"harmed_tenants"`
	Caveats         []string                `json:"caveats"`
}

// previewCaveats is fixed text, not computed per request: every preview carries the same
// disclosed gaps between this replay and what production would actually do.
var previewCaveats = []string{
	"ESTIMATED, not measured: this is a replay of each tenant's own past idle gaps, not a " +
		"forecast and not a record of what this exact config has done live.",
	"Prior study of this same quantity found five otherwise-reasonable ways to compute it " +
		"disagreeing by roughly 18x and flipping sign twice — trust the shape (which tenants " +
		"are harmed, roughly which rung is better) far more than either dollar bound below.",
	"net_usd_low uses only the avoidable cache-write premium; net_usd_high credits the whole " +
		"avoided miss. Neither is settled — the true figure is somewhere in that range, or " +
		"outside it if production behaves differently from this replay.",
	"MinPrefixTokens, MaxUSDPerPing and any predictor gate are NOT modeled here — a candidate " +
		"using any of them would ping, and spend, LESS than shown, so pings/ping_usd/net_usd_low " +
		"are upper/lower bounds on cost, not estimates of it.",
}

// resolveCandidateMaxPings applies TenantCaps to the candidate's own MaxPings, the same
// resolution proxy.applyStrategy performs live (see proxy/keepalivestrategy.go) — kept in sync
// by TestResolveCandidateMaxPingsMatchesLiveResolution rather than by a shared function, since
// dash cannot import proxy (proxy already imports dash) and duplicating four lines is cheaper
// than the import cycle a shared helper package would cost here.
func resolveCandidateMaxPings(in StrategyPreviewInput, tenantID string) (maxPings int, off bool) {
	if c, ok := in.TenantCaps[tenantID]; ok {
		if c.Off {
			return 0, true
		}
		if c.MaxPings != nil {
			return *c.MaxPings, false
		}
	}
	return in.MaxPings, false
}

// strategyPreviewRoutes is this feature's one route, appended in api.go for the same reason
// every other feature's are — see keepAliveStrategyRoutes' own comment.
func (a *API) strategyPreviewRoutes() []route {
	return []route{
		{"GET /api/keepalive/strategies/preview", scopeManager, a.keepAliveStrategyPreview},
	}
}

func (a *API) keepAliveStrategyPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requireManager(w, r, "a strategy preview") {
		return
	}
	raw := r.URL.Query().Get("candidate")
	if raw == "" {
		httpErr(w, http.StatusBadRequest, "give a candidate configuration to preview")
		return
	}
	var in StrategyPreviewInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		httpErr(w, http.StatusBadRequest, "candidate is not valid JSON: "+err.Error())
		return
	}
	if in.IdleSeconds <= 0 || in.MaxPings <= 0 {
		httpErr(w, http.StatusBadRequest, "idle_seconds and max_pings must both be positive")
		return
	}
	if len(in.TenantIDs) == 0 {
		writeJSON(w, &StrategyPreviewResult{Cohorts: []StrategyPreviewCohort{},
			HarmedTenants: []string{}, Caveats: previewCaveats})
		return
	}
	db := a.db(r)
	priceFn := a.priceFn(r)
	out := &StrategyPreviewResult{Caveats: previewCaveats, HarmedTenants: []string{}}
	for _, tenantID := range in.TenantIDs {
		f := Filter{Tenant: tenantID}
		cohort := StrategyPreviewCohort{TenantID: tenantID}
		effectivePings, off := resolveCandidateMaxPings(in, tenantID)
		cohort.EffectiveMaxPings, cohort.Off = effectivePings, off
		prefix, model, err := db.AccountMedianPrefix(f)
		if err != nil {
			httpErr(w, http.StatusInternalServerError, "could not read "+tenantID+"'s own history")
			return
		}
		if prefix <= 0 {
			// No addressable expiry at all: nothing to replay for this tenant, same as
			// ctlKeepAliveMaxPingsByTenant's own admission gate.
			out.Cohorts = append(out.Cohorts, cohort)
			continue
		}
		if !off && len(in.Models) > 0 {
			pct, err := modelCompatiblePct(db, f, in.Models)
			if err != nil {
				httpErr(w, http.StatusInternalServerError, "could not check "+tenantID+"'s model mix")
				return
			}
			cohort.ModelCompatiblePct = &pct
		}
		calc, err := db.KeepAliveCalc(f, in.IdleSeconds, prefix, model, priceFn, effectivePings)
		if err != nil {
			httpErr(w, http.StatusInternalServerError, "could not replay "+tenantID+"'s own gaps")
			return
		}
		cohort.DecisionPoints, cohort.Requests = calc.Addressable, calc.Requests
		cohort.ThinData = calc.Addressable < KeepAliveMinDecisionPoints
		cohort.Priced = calc.Priced
		if off {
			out.Cohorts = append(out.Cohorts, cohort)
			continue
		}
		var row *CalcRow
		for i := range calc.Rows {
			if calc.Rows[i].MaxPings == effectivePings {
				row = &calc.Rows[i]
				break
			}
		}
		if row != nil {
			cohort.Pings = row.Pings
			if calc.Priced {
				cohort.PingUSD = row.PingUSD
				cohort.NetUSDLow = row.NetUSD
				cohort.NetUSDHigh = row.ConvertibleUSD - row.PingUSD
				cohort.Harmed = cohort.NetUSDHigh <= 0
				if cohort.Harmed {
					out.HarmedTenants = append(out.HarmedTenants, tenantID)
				}
				if in.MaxUSDPerTenant > 0 && row.PingUSD > in.MaxUSDPerTenant {
					cohort.OverTenantBudget = true
				}
				out.TotalPingUSD += cohort.PingUSD
				out.TotalNetUSDLow += cohort.NetUSDLow
				out.TotalNetUSDHigh += cohort.NetUSDHigh
			}
		}
		out.Cohorts = append(out.Cohorts, cohort)
	}
	// Worst first, matching ctlKeepAliveMaxPingsByTenant's own ordering — the cohorts a flat
	// setting would harm belong at the top, not wherever the caller's tenant_ids happened to
	// list them.
	sort.SliceStable(out.Cohorts, func(i, j int) bool {
		if out.Cohorts[i].Harmed != out.Cohorts[j].Harmed {
			return out.Cohorts[i].Harmed
		}
		return out.Cohorts[i].NetUSDHigh < out.Cohorts[j].NetUSDHigh
	})
	writeJSON(w, out)
}

// modelCompatiblePct is the share of tenantID's own (non-keepalive) requests whose model is one
// of `models` — an exact match, not a substring: this deployment names models by their full
// gateway id (e.g. "aws/claude-sonnet-5"), and a substring match would silently widen a
// candidate's own declared compatibility list. Returns 100 when the tenant has no requests at
// all, matching "nothing to be incompatible with" rather than reading as 0% compatible.
func modelCompatiblePct(db *DB, f Filter, models []string) (float64, error) {
	total, err := db.countRequests(f)
	if err != nil || total == 0 {
		return 100, err
	}
	var compatible int64
	for _, m := range models {
		mf := f
		mf.Model = m
		n, err := db.countRequests(mf)
		if err != nil {
			return 0, err
		}
		compatible += n
	}
	return 100 * float64(compatible) / float64(total), nil
}

// countRequests is a plain COUNT(*) over Filter's own predicate — reused here rather than
// added to Filter/query.go itself, since nothing else in this package needs a bare count (every
// other reader wants the rows, or an aggregate query.go already builds).
func (d *DB) countRequests(f Filter) (int64, error) {
	cond, args := f.where()
	var n int64
	err := d.sql.QueryRowContext(d.readCtx(), `SELECT COUNT(*) FROM requests r WHERE `+cond, args...).Scan(&n)
	return n, err
}
