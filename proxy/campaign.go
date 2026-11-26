package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/tenant"
)

// Strategy campaigns: bulk-create/activate keep-alive strategies from a batch of
// dash.KVCacheSuggestions cells, in one manager action — see
// docs/superpowers/specs/2026-08-25-keepalive-strategies-design.md for the strategy
// model this builds on, tenant/campaign.go for persistence, and
// dash/campaignsavings.go for the real-saving read side this feature's drill-down uses.
//
// # Enforcement is restricted to arms with a real runtime path
//
// A suggest cell names a kvcache.Strategy arm (see kvcache/registry.go) — the
// simulator's vocabulary, built for backtesting, not for the live proxy. Most of that
// vocabulary has no live counterpart at all: campaign.ArmFor is the one place this
// deployment decides which arms it can actually run, and everything else is reported,
// never silently approximated.
//
// # The 1h-tier arms are gated per tenant by which model that tenant's traffic runs on
//
// keepalive-1h and keepalive-1h-once need CachePolicy.HeadTTL1h, and on this
// deployment's own measurement (apply/headttl.go) Bedrock honors that tier only for
// aws/claude-haiku-4-5 — every other model this service runs is a moving target: Sonnet
// 5 was REFUSED on an earlier date and GRANTED on 2026-11-19, with several newer models (see
// apply/headttl.go and #1243). Until a model is measured, activating a 1h arm blindly could pay the
// tier's write premium for no benefit. campaign.HeadTTL1hHonored is checked per tenant before a 1h arm is ever
// marked activatable.

// campaignCellOf is a dashboard suggest cell as the campaign library reads it.
func campaignCellOf(c dash.KVCacheSuggestion) campaign.Cell {
	return campaign.Cell{User: c.User, HourUTC: c.HourUTC, Requests: c.Requests, BestStrategy: c.BestStrategy, SavingUSD: c.SavingUSD,
		BaselineUSD: c.BaselineUSD, OptimalSavingUSD: c.OptimalSavingUSD, InsufficientData: c.InsufficientData, UnpricedRequests: c.UnpricedRequests}
}

// CampaignCellOutcome is what creating a campaign would do with one uploaded suggest cell (campaign.CellOutcome): activate it, or record it with
// skipReason. honorsHeadTTL1h reports whether a tenant's traffic honors the 1h cache tier. It is the SAME decision ctlCreateCampaign makes, so an
// offline tool that builds an upload file can report it without a second copy of the rules.
func CampaignCellOutcome(cell dash.KVCacheSuggestion, honorsHeadTTL1h func(tenantID string) bool) (activatable bool, skipReason string) {
	return campaign.CellOutcome(campaignCellOf(cell), honorsHeadTTL1h)
}

// CampaignArmEnforceable reports whether a campaign can enforce arm on live traffic at all (campaign.ArmEnforceable).
func CampaignArmEnforceable(arm string) bool { return campaign.ArmEnforceable(arm) }

// HeadTTL1hHonored reports whether model is measured to honor the 1h cache tier on this deployment (campaign.HeadTTL1hHonored).
func HeadTTL1hHonored(model string) bool { return campaign.HeadTTL1hHonored(model) }

// validateGroupStrategy runs the same checks ctlCreateKeepAliveStrategy already runs on
// a manually-created strategy before persisting it — defense in depth here, since a
// strategy campaign.StrategyForGroup built from campaign.ArmFor's own vetted constants and
// campaign.ResolveCell's hour-range check should always already be valid, but "should
// always" is not "provably always," and the alternative to checking is trusting a
// window/target/bounds mismatch to surface itself only much later, silently, as a
// strategy that pings and saves nothing forever.
func validateGroupStrategy(s tenant.Strategy) error {
	idle := time.Duration(s.IdleSeconds) * time.Second
	if err := validStrategyBounds(idle, s.MaxPings, s.MinPrefixTokens, s.MaxUSDPerPing,
		s.HeadTTL1h, s.HeadTTLMinTokens); err != nil {
		return err
	}
	if err := s.Target.Validate(); err != nil {
		return err
	}
	if err := validWindows(s.Windows); err != nil {
		return err
	}
	return validPredictorRef(s.PredictorID, s.PredictorThreshold)
}

// campaignCtlRoutes is this feature's control-plane table, appended to ctlRoutes in
// control.go. Every route is ctlManager and audited, table-driven exactly like
// keepAliveStrategyCtlRoutes' own table.
func (h *Handler) campaignCtlRoutes() []ctlRoute {
	return []ctlRoute{
		{"GET /api/keepalive/campaigns", ctlManager, h.ctlListCampaigns},
		{"POST /api/keepalive/campaigns", ctlManager, h.ctlCreateCampaign},
		{"GET /api/keepalive/campaigns/{id}", ctlManager, h.ctlGetCampaign},
		{"GET /api/keepalive/campaigns/{id}/tenants/{tenantID}", ctlManager, h.ctlGetCampaignTenant},
		{"DELETE /api/keepalive/campaigns/{id}", ctlManager, h.ctlArchiveCampaign},
	}
}

// campaignCellView is the wire shape for one frozen cell.
type campaignCellView struct {
	TenantID     string  `json:"tenant_id"`
	HourUTC      int     `json:"hour_utc"`
	Requests     int64   `json:"requests"`
	Arm          string  `json:"arm"`
	PredictedUSD float64 `json:"predicted_usd"`
	BaselineUSD  float64 `json:"baseline_usd"`
	// OptimalSavingUSD is this cell's own frozen exact-ceiling saving over BaselineUSD (see
	// dash.KVCacheSuggestion.OptimalSavingUSD) — "how much was there to capture", beside
	// PredictedUSD's "how much this campaign's own choice actually captured".
	OptimalSavingUSD float64 `json:"optimal_saving_usd"`
	InsufficientData bool    `json:"insufficient_data"`
	Activatable      bool    `json:"activatable"`
	SkipReason       string  `json:"skip_reason,omitempty"`
	StrategyID       string  `json:"strategy_id,omitempty"`
}

func viewCampaignCell(c tenant.CampaignCell) campaignCellView {
	return campaignCellView{
		TenantID: c.TenantID, HourUTC: c.HourUTC, Requests: c.Requests, Arm: c.Arm,
		PredictedUSD: c.PredictedUSD, BaselineUSD: c.BaselineUSD,
		OptimalSavingUSD: c.OptimalSavingUSD,
		InsufficientData: c.InsufficientData, Activatable: c.Activatable,
		SkipReason: c.SkipReason, StrategyID: c.StrategyID,
	}
}

// campaignTenantSummary is one tenant's whole-campaign economics: predicted (frozen,
// summed across every cell this tenant appears in) beside real (live, summed across
// every hour since the campaign's own activated_at) — the aggregated view a campaign
// overview needs, as opposed to ctlGetCampaignTenant's per-hour drill-down.
type campaignTenantSummary struct {
	TenantID     string  `json:"tenant_id"`
	PredictedUSD float64 `json:"predicted_usd"`
	// OptimalSavingUSD sums the same cells PredictedUSD does (StrategyID != "", see
	// campaignTenantSummaries), so the two are always compared over the exact same
	// population — this tenant's own exact-ceiling headroom beside what was actually
	// captured.
	OptimalSavingUSD float64 `json:"optimal_saving_usd"`
	RealPingUSD      float64 `json:"real_ping_usd"`
	RealSavedUSD     float64 `json:"real_saved_usd"`
	RealNetUSD       float64 `json:"real_net_usd"`
}

// campaignView is the wire shape for one campaign, without its cells (the list route)
// or with them plus the per-tenant aggregate (the get route) — the same list/detail
// split strategyView's own CRUD already uses.
type campaignView struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Source      string             `json:"source"`
	Baseline    string             `json:"baseline"`
	MinRequests int                `json:"min_requests"`
	Weekdays    []string           `json:"weekdays"`
	Status      string             `json:"status"`
	CreatedBy   string             `json:"created_by"`
	CreatedAt   int64              `json:"created_at"`
	ActivatedAt int64              `json:"activated_at"`
	Cells       []campaignCellView `json:"cells,omitempty"`

	// Tenants and the totals below are populated on the get route only, never the list
	// one — see ctlGetCampaign. Left off the wire (Tenants nil) rather than sent as a
	// numeric 0 in that case: omitempty on a SLICE means "not computed", but the four
	// float64 totals carry no omitempty at all — a genuinely zero real saving must
	// still render as $0, not vanish and read as "unknown" (usd(undefined) -> "—").
	Tenants               []campaignTenantSummary `json:"tenants,omitempty"`
	TotalPredictedUSD     float64                 `json:"total_predicted_usd"`
	TotalOptimalSavingUSD float64                 `json:"total_optimal_saving_usd"`
	TotalRealPingUSD      float64                 `json:"total_real_ping_usd"`
	TotalRealSavedUSD     float64                 `json:"total_real_saved_usd"`
	TotalRealNetUSD       float64                 `json:"total_real_net_usd"`
	Caveat                string                  `json:"caveat,omitempty"`
}

func viewCampaign(c tenant.Campaign) campaignView {
	return campaignView{
		ID: c.ID, Name: c.Name, Source: c.Source, Baseline: c.Baseline,
		MinRequests: c.MinRequests, Weekdays: c.Weekdays, Status: c.Status,
		CreatedBy: c.CreatedBy, CreatedAt: msOrZero(c.CreatedAt), ActivatedAt: msOrZero(c.ActivatedAt),
	}
}

// ctlListCampaigns lists every campaign, no cells.
func (h *Handler) ctlListCampaigns(w http.ResponseWriter, r *http.Request) {
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
	list, err := h.registry().ListCampaigns()
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list campaigns")
		return
	}
	out := make([]campaignView, 0, len(list))
	for _, c := range list {
		out = append(out, viewCampaign(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": out})
}

// campaignCreateIn is a create request's body. Source "live" makes the server call
// KVCacheSuggest itself (Baseline/Since/Until narrow that call); source "upload" uses
// Suggest verbatim, matching the exact wire shape GET /api/kvcache/suggest returns —
// the same JSON a manager could fetch, hand-edit, and re-submit.
type campaignCreateIn struct {
	Name     string                   `json:"name"`
	Source   string                   `json:"source"`
	Baseline string                   `json:"baseline,omitempty"`
	Since    int64                    `json:"since,omitempty"`
	Until    int64                    `json:"until,omitempty"`
	Suggest  *dash.KVCacheSuggestions `json:"suggest,omitempty"`
}

// ctlCreateCampaign resolves a batch of suggest cells (live-fetched or uploaded) into
// keep-alive strategies and a frozen campaign record, in one action:
//
//  1. Skip any cell for tenant_id "" — an ambiguous pool (single-tenant traffic, or
//     pre-tenancy rows), never a safe campaign target in a hosted deployment.
//  2. Resolve every remaining cell's arm (campaign.ArmFor), gating a 1h-tier arm on
//     whether THIS tenant's own traffic honors it (campaign.HeadTTL1hHonored).
//  3. Coalesce the activatable, non-baseline cells into the fewest safe strategies
//     (campaign.Coalesce) and create each one.
//  4. Persist the campaign and every cell's frozen outcome (tenant.CreateCampaign),
//     including baseline and non-activatable cells — nothing is hidden, only skipped.
//
// If persistence fails after strategies were already created, every strategy this
// call created is deleted — a campaign that could not be recorded is a campaign that
// must not be left running unaccounted for, the same refusal ctlCreateKeepAliveStrategy
// makes when ITS OWN audit write fails.
func (h *Handler) ctlCreateCampaign(w http.ResponseWriter, r *http.Request) {
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
	var in campaignCreateIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		ctlErr(w, http.StatusBadRequest, "a campaign needs a name")
		return
	}

	suggest, err := h.resolveCampaignSuggest(r, in)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}

	honorCache := map[string]bool{}
	honorsHeadTTL1h := func(tenantID string) bool {
		if v, ok := honorCache[tenantID]; ok {
			return v
		}
		v := h.tenantHonorsHeadTTL1h(r, tenantID, in.Since, in.Until)
		honorCache[tenantID] = v
		return v
	}

	resolved := make([]*campaign.Resolved, 0, len(suggest.Cells))
	for _, cell := range suggest.Cells {
		if cell.User == "" {
			continue
		}
		c := campaign.ResolveCell(campaignCellOf(cell), honorsHeadTTL1h)
		resolved = append(resolved, &c)
	}
	if len(resolved) == 0 {
		ctlErr(w, http.StatusBadRequest,
			"no cells to campaign over — every suggest cell had no tenant, or the suggest payload was empty")
		return
	}
	// Checked here, before any strategy is created — not only inside
	// tenant.CreateCampaign's own persistence-time guard on the same (tenant, hour)
	// pair. By the time that guard would run, the loop below has already created a
	// real, live keepalive_strategies row for every coalesced group; rejecting only
	// there still means a duplicate-cell upload creates-then-rolls-back strategies
	// instead of never creating them at all.
	if dup := campaign.DuplicateCell(resolved); dup != nil {
		ctlErr(w, http.StatusBadRequest, fmt.Sprintf(
			"duplicate cell for tenant %q at hour %d", dup.TenantID, dup.HourUTC))
		return
	}
	// Cross-campaign overlap, checked in the same place and for the same reason the
	// within-payload duplicate above is: before any strategy exists. A failure to READ the
	// existing owners aborts the whole create rather than proceeding unchecked — a
	// campaign created while this gate was blind is exactly the double-enforcement it
	// exists to prevent, and it cannot be un-created without hunting down its strategies
	// by hand afterwards.
	owners, err := h.registry().ActiveCampaignCellOwners()
	if err != nil {
		slog.Error("context-guru: could not read active campaign cell owners", "err", err)
		ctlErr(w, http.StatusInternalServerError,
			"could not check this campaign against the hours already enforced")
		return
	}
	campaign.MarkOverlaps(resolved, owners)

	days := campaign.WeekdaysFromNames(suggest.Weekdays)
	groups := campaign.Coalesce(resolved)
	created := make([]tenant.Strategy, 0, len(groups))
	for _, g := range groups {
		strategy := campaign.StrategyForGroup(in.Name, g, days)
		if err := validateGroupStrategy(strategy); err != nil {
			// Defense in depth: campaign.ArmFor's own constants and campaign.ResolveCell's
			// hour-range check should make this unreachable, but a coalesced strategy this
			// deployment could not actually enforce must never be silently persisted as if
			// it could — every cell that would have shared it becomes non-activatable with
			// the reason, the same as any other unenforceable arm, instead of aborting the
			// whole campaign over one bad group.
			for _, c := range g.Cells {
				c.Activatable = false
				c.SkipReason = "could not create a valid strategy: " + err.Error()
			}
			continue
		}
		s, err := h.registry().CreateStrategy(actor.ID, strategy)
		if err != nil {
			slog.Error("context-guru: could not create a campaign strategy", "err", err)
			h.rollbackCampaignStrategies(created)
			ctlErr(w, http.StatusInternalServerError, "could not create this campaign's strategies")
			return
		}
		created = append(created, s)
		for _, c := range g.Cells {
			c.StrategyID = s.ID
		}
	}

	cells := make([]tenant.CampaignCell, 0, len(resolved))
	for _, c := range resolved {
		cells = append(cells, c.TenantCell())
	}
	camp, err := h.registry().CreateCampaign(actor.ID, tenant.Campaign{
		Name: in.Name, Source: in.Source, Baseline: suggest.Baseline,
		MinRequests: suggest.MinRequests, Weekdays: suggest.Weekdays,
	}, cells)
	if err != nil {
		slog.Error("context-guru: could not record a strategy campaign", "err", err)
		h.rollbackCampaignStrategies(created)
		ctlErr(w, http.StatusInternalServerError,
			"could not record this campaign, so its strategies were not created")
		return
	}
	// Every strategy in `created` is live and already referenced by committed
	// campaign_cells rows by this point, so an audit-write failure on one of them is
	// not a reason to stop attempting the rest — unlike the create-strategy loop
	// above, where a failure DOES stop everything (nothing downstream depends on the
	// failed one yet). Collect failures rather than bailing on the first, so a
	// transient error on strategy 2 of 3 doesn't also skip the attempt for strategy 3.
	var auditFailed bool
	for _, s := range created {
		if err := h.registry().AuditWrite(actor.ID, actor.ID, s.ID, "",
			"created via campaign: "+camp.Name); err != nil {
			auditFailed = true
		}
	}
	if auditFailed {
		// The strategies and the campaign both exist at this point; unwinding a
		// campaign that already has real cells recorded against it would discard more
		// than it protects, unlike the single-strategy create path.
		ctlErr(w, http.StatusInternalServerError,
			"the campaign was created, but its audit trail could not be fully written")
		return
	}
	h.keeper.loadStrategies()
	view := viewCampaign(camp)
	view.Cells = make([]campaignCellView, 0, len(cells))
	for _, c := range cells {
		view.Cells = append(view.Cells, viewCampaignCell(c))
	}
	writeJSON(w, http.StatusCreated, view)
}

// rollbackCampaignStrategies deletes every strategy a failed campaign create already
// made. Best-effort: a delete error here is not reported back, since the caller is
// already reporting the create failure that triggered it and a second error about the
// cleanup would obscure the first — but it IS logged, so an orphaned row is at least
// discoverable rather than silently unaccounted for.
//
// Reloads the keeper's in-memory strategy list once, after every delete attempt, no
// matter how many succeeded: a strategy this call just created was already matchable
// to live traffic (campaign.StrategyForGroup sets Active: true, and nothing gates matching on
// the campaign it came from), so a rolled-back strategy left in memory would keep
// pinging and toggling KeepAlive/HeadTTL1h for its targeted tenants — invisible to
// every control route, since neither the campaign nor its cells were ever persisted —
// until some unrelated strategy/campaign mutation happened to trigger the next reload.
func (h *Handler) rollbackCampaignStrategies(created []tenant.Strategy) {
	for _, s := range created {
		if err := h.registry().DeleteStrategy(s.ID); err != nil {
			slog.Error("context-guru: could not roll back a campaign strategy; it may be orphaned",
				"strategy_id", s.ID, "err", err)
		}
	}
	h.keeper.loadStrategies()
}

// resolveCampaignSuggest answers the create flow's two input modes: "live" calls
// KVCacheSuggest itself; "upload" uses the client-supplied payload verbatim.
func (h *Handler) resolveCampaignSuggest(r *http.Request, in campaignCreateIn) (*dash.KVCacheSuggestions, error) {
	if in.Source == tenant.CampaignSourceUpload {
		if in.Suggest == nil {
			return nil, fmt.Errorf("source %q needs a suggest payload", tenant.CampaignSourceUpload)
		}
		return in.Suggest, nil
	}
	if h.rec == nil {
		return nil, fmt.Errorf("the dashboard is not enabled on this deployment")
	}
	baseline := in.Baseline
	if baseline == "" {
		baseline = kvcache.StrategyFixed5m
	}
	f := dash.Filter{TenantAll: true, Since: in.Since, Until: in.Until}
	suggest, err := h.rec.DB().WithContext(r.Context()).KVCacheSuggest(
		f, dash.KVCacheOptions{}, h.opts.Prices, dash.KVCacheSimConfig{Baseline: baseline})
	if err != nil {
		// Unlike the two hand-authored errors above, this one comes from a live DB
		// query and may carry internal detail (a raw driver error, a decode failure
		// naming a Go field) that has no business reaching an HTTP client — logged
		// server-side instead, same as the create-strategy/create-campaign failures
		// below.
		slog.Error("context-guru: could not compute live kv-cache suggestions for a campaign", "err", err)
		return nil, fmt.Errorf("could not compute live suggestions")
	}
	return suggest, nil
}

// tenantHonorsHeadTTL1h reports whether every model tenantID used, in the same window a
// live suggest call would have scanned, honors the 1h tier (campaign.HonorsHeadTTL1h). Fails CLOSED: no
// models found (unknown traffic, or the dashboard unreachable) means "not confirmed",
// never "assume yes" — the same direction every other check in this file errs in.
func (h *Handler) tenantHonorsHeadTTL1h(r *http.Request, tenantID string, since, until int64) bool {
	if h.rec == nil || tenantID == "" {
		return false
	}
	models, err := h.rec.DB().WithContext(r.Context()).KVCacheModels(
		dash.Filter{Tenant: tenantID, Since: since, Until: until})
	if err != nil {
		return false
	}
	return campaign.HonorsHeadTTL1h(models)
}

// ctlGetCampaign returns one campaign with its full cell set.
func (h *Handler) ctlGetCampaign(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	c, err := h.registry().CampaignByID(id)
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such campaign")
		return
	}
	cells, err := h.registry().CampaignCells(id)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not load this campaign's cells")
		return
	}
	view := viewCampaign(c)
	view.Cells = make([]campaignCellView, 0, len(cells))
	for _, cell := range cells {
		view.Cells = append(view.Cells, viewCampaignCell(cell))
	}
	tenants, caveat, err := h.campaignTenantSummaries(r, c, cells)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not compute this campaign's real savings")
		return
	}
	view.Tenants, view.Caveat = tenants, caveat
	for _, t := range tenants {
		view.TotalPredictedUSD += t.PredictedUSD
		view.TotalOptimalSavingUSD += t.OptimalSavingUSD
		view.TotalRealPingUSD += t.RealPingUSD
		view.TotalRealSavedUSD += t.RealSavedUSD
		view.TotalRealNetUSD += t.RealNetUSD
	}
	writeJSON(w, http.StatusOK, view)
}

// campaignTenantSummaries aggregates every tenant this campaign's cells named: the
// frozen predicted total (summed straight from the cells this call already has, no
// extra read) and the real total (one CampaignRealSavings call across every strategy
// and every tenant at once, summed per tenant across hours) — the overview's per-tenant
// table, as opposed to ctlGetCampaignTenant's per-hour drill-down for one of them.
func (h *Handler) campaignTenantSummaries(r *http.Request, campaign tenant.Campaign,
	cells []tenant.CampaignCell) ([]campaignTenantSummary, string, error) {
	byTenant := map[string]*campaignTenantSummary{}
	var order []string
	strategyIDs := map[string]bool{}
	// tenantsWithStrategy is deliberately NARROWER than `order`: `order` lists every
	// tenant any cell named, so the overview still shows a $0-predicted row for a
	// tenant this campaign never activated, but the REAL half must only ever be read
	// for tenants that population actually includes — otherwise a tenant with zero
	// activated cells here, but real credited savings from something else entirely
	// (another campaign, a manually-created strategy), would show a nonzero Real next
	// to a correctly-$0 Predicted, exactly the two-different-populations bug the
	// PredictedUSD gate below already closed for the other side.
	tenantsWithStrategy := map[string]bool{}
	for _, c := range cells {
		s := byTenant[c.TenantID]
		if s == nil {
			s = &campaignTenantSummary{TenantID: c.TenantID}
			byTenant[c.TenantID] = s
			order = append(order, c.TenantID)
		}
		// Only count a cell's predicted saving when it actually became a strategy
		// (StrategyID != ""): a simulation-only or model-gated-out arm's PredictedUSD
		// is frozen for the record, but it was never attempted, so there is no
		// "reality" a real number could ever be checked against. Summing it in here
		// would inflate Predicted against a population Real can never match, over
		// exactly the arms this deployment couldn't enforce in the first place.
		if c.StrategyID != "" {
			s.PredictedUSD += c.PredictedUSD
			s.OptimalSavingUSD += c.OptimalSavingUSD
			strategyIDs[c.StrategyID] = true
			tenantsWithStrategy[c.TenantID] = true
		}
	}
	if h.rec != nil && len(strategyIDs) > 0 {
		ids := make([]string, 0, len(strategyIDs))
		for id := range strategyIDs {
			ids = append(ids, id)
		}
		realTenants := make([]string, 0, len(tenantsWithStrategy))
		for id := range tenantsWithStrategy {
			realTenants = append(realTenants, id)
		}
		rows, err := h.rec.DB().WithContext(r.Context()).
			CampaignRealSavings(ids, realTenants, campaign.ActivatedAt.UnixMilli())
		if err != nil {
			return nil, campaignRealSavingsCaveat, err
		}
		for _, row := range rows {
			if s := byTenant[row.TenantID]; s != nil {
				s.RealPingUSD += row.PingUSD
				s.RealSavedUSD += row.SavedUSD
				s.RealNetUSD += row.NetUSD
			}
		}
	}
	out := make([]campaignTenantSummary, 0, len(order))
	for _, id := range order {
		out = append(out, *byTenant[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PredictedUSD > out[j].PredictedUSD })
	return out, campaignRealSavingsCaveat, nil
}

// campaignCellDrilldown is one (hour) cell of one tenant's drill-down: the frozen
// historical/predicted half, alongside the real half read live — see
// dash.CampaignSavingCell, whose ceiling caveat applies here just as much.
type campaignCellDrilldown struct {
	campaignCellView
	RealRequests   int64   `json:"real_requests"`
	RealActiveDays int64   `json:"real_active_days"`
	RealPings      int64   `json:"real_pings"`
	RealPingUSD    float64 `json:"real_ping_usd"`
	RealSavedUSD   float64 `json:"real_saved_usd"`
	RealNetUSD     float64 `json:"real_net_usd"`
	// Normalized, present only where the denominator is nonzero — a $-per-1k-requests
	// or $-per-active-day figure over zero of either is not a number, it is a division
	// this response refuses to fabricate. Pointers, not omitempty float64s: the
	// computed rate can itself legitimately be exactly 0 (real traffic, zero credited
	// saving that hour), and omitempty on a float64 cannot tell that apart from "never
	// computed" — the same "zero looks like absence" mistake fixed once already on
	// campaignView's totals. nil here means genuinely not computed; a non-nil *0 means
	// the rate really is zero.
	RealSavedUSDPer1kRequests *float64 `json:"real_saved_usd_per_1k_requests,omitempty"`
	RealSavedUSDPerActiveDay  *float64 `json:"real_saved_usd_per_active_day,omitempty"`
}

// ctlGetCampaignTenant is the per-user drill-down: every hour cell for one tenant in
// one campaign, frozen predicted numbers beside the live real ones.
func (h *Handler) ctlGetCampaignTenant(w http.ResponseWriter, r *http.Request) {
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
	id, tenantID := r.PathValue("id"), r.PathValue("tenantID")
	campaign, err := h.registry().CampaignByID(id)
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such campaign")
		return
	}
	cells, err := h.registry().CampaignCells(id)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not load this campaign's cells")
		return
	}
	real, caveat, err := h.campaignRealSavings(r, campaign, cells, tenantID)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not compute real savings")
		return
	}
	out := make([]campaignCellDrilldown, 0)
	for _, cell := range cells {
		if cell.TenantID != tenantID {
			continue
		}
		d := campaignCellDrilldown{campaignCellView: viewCampaignCell(cell)}
		if rc, ok := real[cell.HourUTC]; ok {
			d.RealRequests, d.RealActiveDays = rc.Requests, rc.ActiveDays
			d.RealPings, d.RealPingUSD = rc.Pings, rc.PingUSD
			d.RealSavedUSD, d.RealNetUSD = rc.SavedUSD, rc.NetUSD
			if rc.Requests > 0 {
				per1k := rc.SavedUSD / float64(rc.Requests) * 1000
				d.RealSavedUSDPer1kRequests = &per1k
			}
			if rc.ActiveDays > 0 {
				perDay := rc.SavedUSD / float64(rc.ActiveDays)
				d.RealSavedUSDPerActiveDay = &perDay
			}
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant_id": tenantID, "cells": out, "caveat": caveat})
}

// campaignRealSavingsCaveat is carried on every response that includes a real-saving
// figure. It used to declare the figure a CEILING, on the premise that a credited request
// carried no strategy id and so could only ever be attributed to a tenant — which was
// already untrue when it shipped, and had the campaigns tab reporting every strategy's
// saving under whichever campaign named the tenant. dash/campaignsavings.go now scopes the
// credit by strategy id, so the number is exact and this says so instead.
//
// What replaces the old warning is the one caveat that IS still true and is easy for a
// reader to trip over: these figures do not add up to the Keep-Alive or Overview totals,
// because a credit whose ping matched no strategy belongs to no campaign. That is the same
// sentence renderStrategiesList already shows for a single strategy's ledger, deliberately
// worded the same way so the two tabs read as one product.
const campaignRealSavingsCaveat = "Real saved-$ figures are EXACT and additive across " +
	"campaigns: each credited request is attributed once, to whichever strategy's pings " +
	"actually rescued it. They will not sum to the Overview or Keep-Alive tab's total, " +
	"though — a credit whose ping matched no strategy (plain account config or a session " +
	"override) belongs to no campaign, and neither does traffic from before this campaign " +
	"was activated."

// campaignRealSavings computes real savings for every strategy this campaign created,
// bounded to since tenantID is only used to filter which cells to bother computing —
// the underlying query still reads every targeted tenant, since a strategy's Target
// commonly names more than one.
//
// Returns nothing at all unless tenantID ITSELF has at least one activated cell in
// this campaign — not merely "some tenant in the campaign does" — matching
// campaignTenantSummaries' own population gate on the aggregate view. Without this, a
// tenant whose every cell here is simulation-only or model-gated-out (so its own
// StrategyID is always "") could still see nonzero real numbers in its drill-down as
// long as ANY OTHER tenant in the same campaign happened to get a strategy, since the
// old check only asked whether the campaign as a whole had created one.
func (h *Handler) campaignRealSavings(r *http.Request, campaign tenant.Campaign,
	cells []tenant.CampaignCell, tenantID string) (map[int]dash.CampaignSavingCell, string, error) {
	if h.rec == nil || tenantID == "" {
		return nil, campaignRealSavingsCaveat, nil
	}
	strategyIDs := map[string]bool{}
	tenantActivated := false
	for _, c := range cells {
		if c.StrategyID != "" {
			strategyIDs[c.StrategyID] = true
			if c.TenantID == tenantID {
				tenantActivated = true
			}
		}
	}
	if !tenantActivated {
		return nil, campaignRealSavingsCaveat, nil
	}
	ids := make([]string, 0, len(strategyIDs))
	for id := range strategyIDs {
		ids = append(ids, id)
	}
	rows, err := h.rec.DB().WithContext(r.Context()).
		CampaignRealSavings(ids, []string{tenantID}, campaign.ActivatedAt.UnixMilli())
	if err != nil {
		return nil, campaignRealSavingsCaveat, err
	}
	// The cost half of CampaignRealSavings is deliberately NOT tenant-scoped (a
	// coalesced strategy commonly targets more than one tenant, and its ping cost is
	// exact per row regardless), so rows for every tenant that strategy touched come
	// back here, not only tenantID's own. Filtering to tenantID here, rather than
	// relying on the caller to have passed only one tenant in, is what keeps two
	// tenants sharing a coalesced strategy from overwriting each other's cell in this
	// map when both have activity in the same hour.
	out := make(map[int]dash.CampaignSavingCell, len(rows))
	for _, row := range rows {
		if row.TenantID != tenantID {
			continue
		}
		out[row.HourUTC] = row
	}
	return out, campaignRealSavingsCaveat, nil
}

// ctlArchiveCampaign archives a campaign. It does not touch the strategies it
// created — see tenant.Registry.ArchiveCampaign's own doc comment.
func (h *Handler) ctlArchiveCampaign(w http.ResponseWriter, r *http.Request) {
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
	if err := checkOrigin(r); err != nil {
		readErr(w, err)
		return
	}
	id := r.PathValue("id")
	if err := h.registry().ArchiveCampaign(id); err != nil {
		ctlErr(w, http.StatusNotFound, "no such campaign")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "archived": true})
}
