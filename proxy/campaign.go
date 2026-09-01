package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
// vocabulary has no live counterpart at all: campaignArmFor is the one place this
// deployment decides which arms it can actually run, and everything else is reported,
// never silently approximated.
//
// # The 1h-tier arms are gated per tenant by which model that tenant's traffic runs on
//
// keepalive-1h and keepalive-1h-once need CachePolicy.HeadTTL1h, and on this
// deployment's own measurement (apply/headttl.go) Bedrock honors that tier only for
// aws/claude-haiku-4-5 — every other model this service runs, including the one
// carrying most of its traffic, is silently downgraded to 5m. Activating a 1h arm
// blindly would pay the tier's write premium for zero benefit on exactly the traffic
// that dominates. headTTL1hHonoredModels is checked per tenant before a 1h arm is ever
// marked activatable.

// campaignDefaultMinPrefixTokens mirrors config.DefaultKeepAliveMinPrefix. proxy does
// not import config (see cachePolicy's own doc comment in cmd/context-guru-proxy for
// why), so this is a local literal, the same way DefaultMaxUSDPerPing already mirrors
// config.DefaultKeepAliveMaxUSDPerPing. Never left at the zero value: MinPrefixTokens 0
// on a strategy means NO floor at all — pingable() pings every session that clears
// turn >= 1 — which is 20000x looser than the account default, not equal to it.
const campaignDefaultMinPrefixTokens = 20000

// campaignDefaultMaxPings mirrors config.DefaultKeepAliveMaxPings.
const campaignDefaultMaxPings = 2

// campaignDefaultHeadTTLMinTokens mirrors config.DefaultHeadTTLMinTokens. Every 1h-tier
// arm needs a positive value here — see validStrategyBounds' own refusal of
// HeadTTL1h=true paired with a zero token floor, which would silently create a
// strategy that pings on the 1-hour schedule but never actually promotes the head to
// the 1-hour tier.
const campaignDefaultHeadTTLMinTokens = 50000

// campaignDefaultPredictorThreshold is stop-reason-gated's own live threshold,
// matching the value this codebase's own tests already use whenever they exercise this
// predictor manually (proxy/keepaliveoverride_test.go, proxy/keepalivestrategy_test.go)
// — see validPredictorRef's own refusal of a named predictor paired with a
// non-positive threshold, which would make predictorFor's gate a no-op.
const campaignDefaultPredictorThreshold = 0.5

// headTTL1hHonoredModels are the models this deployment has actually measured granting
// the 1h cache tier rather than silently downgrading it to 5m (apply/headttl.go). A
// campaign will not mark a 1h-tier arm activatable for a tenant whose traffic in the
// suggest window used any model outside this set. Extend it only after measuring a new
// model the same way apply/headttl.go's own doc comment did — never by assumption.
var headTTL1hHonoredModels = map[string]bool{"aws/claude-haiku-4-5": true}

// campaignArm is how one suggest arm maps onto a live keepalive_strategies row,
// decided once per ARM NAME, never per cell: every cell recommending the same arm gets
// the same live parameters, which is what makes it safe to coalesce cells sharing an
// arm across tenants and hours into one strategy.
type campaignArm struct {
	// activatable is false for an arm with no live enforcement path at all — a
	// simulation-only arm. reason explains why, for CampaignCell.SkipReason.
	activatable bool
	reason      string
	// baseline is true for an arm that means "change nothing" (no-cache/fixed-5m/
	// fixed-1h) — a SUCCESSFUL outcome that creates no strategy, distinct from
	// activatable=false, which is a limitation, not a recommendation to do nothing.
	baseline bool
	// needsModelHonoring is true for a 1h-tier arm: resolveCampaignCell additionally
	// checks headTTL1hHonoredModels for this specific tenant before activating it.
	needsModelHonoring bool

	idleSeconds int
	maxPings    int
	// predictorID and predictorThreshold always travel together: a predictorID with a
	// zero threshold would make predictorFor's gate a no-op (see validPredictorRef) —
	// pingable() ends up pinging on the arm's idle/max-ping schedule unconditionally,
	// identically to keepalive-5m, while still being labeled and billed as the
	// predictor-gated arm.
	predictorID        string
	predictorThreshold float64
	headTTL1h          bool
	headTTLMinTokens   int
}

// campaignArmFor is the single source of truth the create flow consults. idleSeconds/
// maxPings for the 5-minute arms sit well inside validStrategyBounds' 5m band
// (minOverrideIdle..maxOverrideIdle, minOverridePings..maxOverridePings); for the
// 1h-tier arms they sit inside the 1h band (maxOverrideIdle1h, maxOverridePings1h) —
// see proxy/keepalivestrategy.go for both bands and why they differ.
func campaignArmFor(arm string) campaignArm {
	switch arm {
	case kvcache.StrategyNoCache, kvcache.StrategyFixed5m, kvcache.StrategyFixed1h:
		return campaignArm{activatable: true, baseline: true}
	case kvcache.StrategyKeepAlive5m:
		return campaignArm{activatable: true, idleSeconds: 280, maxPings: campaignDefaultMaxPings}
	case kvcache.StrategyKeepAlive5mOnce:
		return campaignArm{activatable: true, idleSeconds: 280, maxPings: 1}
	case kvcache.StrategyStopReasonGated:
		return campaignArm{activatable: true, idleSeconds: 280, maxPings: campaignDefaultMaxPings,
			predictorID: "stop-reason-gated", predictorThreshold: campaignDefaultPredictorThreshold}
	case kvcache.StrategyKeepAlive1h:
		// 3360s = kvcache.DefaultPingIdle1h — "the same margin against the one-hour
		// lifetime" the simulator itself ships as this arm's default cadence.
		return campaignArm{activatable: true, needsModelHonoring: true, headTTL1h: true,
			idleSeconds: 3360, maxPings: campaignDefaultMaxPings,
			headTTLMinTokens: campaignDefaultHeadTTLMinTokens}
	case kvcache.StrategyKeepAlive1hOnce:
		return campaignArm{activatable: true, needsModelHonoring: true, headTTL1h: true,
			idleSeconds: 3360, maxPings: 1, headTTLMinTokens: campaignDefaultHeadTTLMinTokens}
	}
	return campaignArm{reason: fmt.Sprintf(
		"%q has no live enforcement path yet — it is a simulation-only arm", arm)}
}

// resolvedCampaignCell is one suggest cell's outcome after resolving its arm and, for a
// 1h-tier arm, checking whether this specific tenant's traffic honors it.
type resolvedCampaignCell struct {
	tenantID                  string
	hourUTC                   int
	requests                  int64
	arm                       string
	predictedUSD, baselineUSD float64
	optimalSavingUSD          float64
	insufficientData          bool
	activatable               bool
	skipReason                string
	// strategyID is filled in by coalesceCampaignCells's caller once the group this
	// cell belongs to has an actual keepalive_strategies row.
	strategyID string
	// config is only meaningful when activatable && !config.baseline.
	config campaignArm
}

// resolveCampaignCell turns one suggest cell into its outcome. honorsHeadTTL1h reports
// whether tenantID's own traffic honors the 1h tier — a function rather than a
// precomputed map so the caller can memoize per tenant however it likes and tests can
// fake it cheaply.
//
// HourUTC is bounds-checked FIRST, before the arm is even resolved: the live-fetch
// source can never produce one outside [0,23] (it's always time.Time.UTC().Hour()), but
// the upload source accepts a hand-edited suggest payload verbatim, and an out-of-range
// hour reaching tileHours would build a Window string like "24:00" that
// tenant.Window.Validate rejects — silently, if nothing catches it first, producing a
// strategy that is created successfully but can never match anything.
func resolveCampaignCell(cell dash.KVCacheSuggestion, honorsHeadTTL1h func(tenantID string) bool,
) resolvedCampaignCell {
	out := resolvedCampaignCell{
		tenantID: cell.User, hourUTC: cell.HourUTC, requests: cell.Requests,
		arm: cell.BestStrategy, predictedUSD: cell.SavingUSD, baselineUSD: cell.BaselineUSD,
		optimalSavingUSD: cell.OptimalSavingUSD, insufficientData: cell.InsufficientData,
	}
	if cell.HourUTC < 0 || cell.HourUTC > 23 {
		out.skipReason = fmt.Sprintf("hour_utc %d is out of range (must be 0-23)", cell.HourUTC)
		return out
	}
	a := campaignArmFor(cell.BestStrategy)
	if !a.activatable {
		out.skipReason = a.reason
		return out
	}
	if a.needsModelHonoring && !honorsHeadTTL1h(cell.User) {
		out.skipReason = "this tenant's traffic does not run on a model that honors the " +
			"1h cache tier on this deployment (see apply/headttl.go)"
		return out
	}
	// InsufficientData means the suggest engine itself does not trust this cell's
	// winning arm as a pattern (too few requests to backtest from) — activating a real,
	// live strategy off it anyway would act on exactly what that flag warns against.
	// Baseline arms are exempt: "change nothing" carries no risk regardless of sample
	// size, unlike committing to a real ping/write-tier schedule.
	if cell.InsufficientData && !a.baseline {
		out.skipReason = "too few requests in this cell to trust its recommendation as a pattern"
		return out
	}
	// The same objection, on the other axis: too few PRICED requests. BestStrategy is an argmax
	// over whichever of the cell's requests had rates, so a cell with unpriced traffic hands us
	// a winner chosen on a subset of the pattern we are about to enforce. This is not about the
	// dollars — an unpriced request contributes $0 to the baseline and $0 to every arm, so
	// SavingUSD is understated, not inflated — it is about the ranking, which really does move:
	// pricing one missing model reordered ranks 5 through 8 and flipped stop-reason-gated from
	// -0.35% to +0.75%, and stop-reason-gated buys 249 pings.
	//
	// Baseline arms are exempt for exactly the reason InsufficientData exempts them: "change
	// nothing" needs no evidence. Everything else here is a live ping or write-tier schedule.
	if cell.UnpricedRequests > 0 && !a.baseline {
		out.skipReason = fmt.Sprintf("%d of this cell's %d requests could not be priced, so its "+
			"winning arm was chosen on a subset of its own traffic",
			cell.UnpricedRequests, cell.Requests)
		return out
	}
	out.activatable = true
	out.config = a
	return out
}

// markCampaignOverlaps turns any cell whose (tenant, hour) an ACTIVE campaign already
// enforces into a non-activatable one, naming the campaign that holds it.
//
// A per-cell skip, not a refusal of the whole request. The two alternatives are both
// worse: rejecting the campaign outright makes one already-covered hour block a batch of
// two hundred good cells, and a manager's only recovery is to hand-edit the payload to
// remove it — while creating the strategy anyway leaves two live strategies competing for
// one hour, where the resolution chain silently picks one and both campaigns claim it.
// Skipping reuses the machinery every other unenforceable cell already flows through:
// recorded with a reason, grouped by that reason in the create result, never hidden.
//
// Deliberately blind to the ARM: a second campaign recommending a DIFFERENT arm for an
// hour is still a conflict, not a refinement. Only one of the two strategies can win the
// resolution chain, so letting the different-arm case through would recreate exactly the
// ambiguity this gate exists to remove. Re-issuing an hour under a new arm is a real
// need, and the honest path for it is to archive the campaign that holds it first.
func markCampaignOverlaps(resolved []*resolvedCampaignCell, owners []tenant.CampaignCellOwner) {
	type key struct {
		tenantID string
		hour     int
	}
	held := make(map[key]tenant.CampaignCellOwner, len(owners))
	for _, o := range owners {
		held[key{o.TenantID, o.HourUTC}] = o
	}
	for _, c := range resolved {
		if !c.activatable {
			continue
		}
		// Baseline cells ("change nothing") create no strategy at all, so they cannot
		// collide with one — excluding them keeps a campaign whose recommendation for an
		// hour is "keep the current policy" from being reported as conflicting with the
		// campaign that already decided the same thing.
		if c.config.baseline {
			continue
		}
		if o, ok := held[key{c.tenantID, c.hourUTC}]; ok {
			c.activatable = false
			c.skipReason = fmt.Sprintf(
				"the active campaign %q already enforces this tenant's hour %02d:00 UTC — "+
					"archive it first to re-issue this hour", o.CampaignName, c.hourUTC)
		}
	}
}

// duplicateCampaignCell reports the first cell whose (tenantID, hourUTC) pair also
// appears earlier in resolved, or nil if every pair is unique — the same key shape
// tenant.CreateCampaign's own guard checks, run here up front so a duplicate is
// refused before any strategy is created for it, not just before the campaign record
// that would have referenced it.
func duplicateCampaignCell(resolved []*resolvedCampaignCell) *resolvedCampaignCell {
	seen := map[[2]any]bool{}
	for _, c := range resolved {
		key := [2]any{c.tenantID, c.hourUTC}
		if seen[key] {
			return c
		}
		seen[key] = true
	}
	return nil
}

// campaignDefaultWeekdays is the work week every suggest cell's PredictedUSD is
// actually backtested against, on either input source: the live path always sets
// KVCacheSuggestions.Weekdays to this exact set (dash's own kvSuggestWeekdays), and the
// upload path accepts a hand-edited copy of that same payload. It is the fallback
// weekdaysFromNames uses when it cannot recover any day at all from the input, rather
// than "no restriction" — see weekdaysFromNames' own doc comment for why "every day" is
// the one outcome that must never happen silently here.
var campaignDefaultWeekdays = []time.Weekday{
	time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
}

// weekdaysFromNames converts a suggest payload's Weekdays (["Sunday", "Monday", ...])
// into tenant.Window's []time.Weekday. An unrecognized name is silently dropped rather
// than erroring — the caller has no better recovery than "run with fewer restricted
// days", and the alternative (refusing the whole campaign for one bad string in a list
// this deployment itself generated) is a worse failure mode.
//
// If NOTHING is recognized — Weekdays omitted entirely by a hand-edited upload, or
// every name in it misspelled — this returns campaignDefaultWeekdays, never an empty
// slice: tenant.Window.contains treats an empty Days as "every day matches", not "no
// day restricted", so a silent drop-to-empty here would create a live strategy that
// fires on Friday and Saturday too — traffic the frozen PredictedUSD figures were never
// backtested against (both this suggester and its own callers only ever read
// Sunday-Thursday history). "Fewer restricted days" must have a floor.
func weekdaysFromNames(names []string) []time.Weekday {
	byName := map[string]time.Weekday{
		"Sunday": time.Sunday, "Monday": time.Monday, "Tuesday": time.Tuesday,
		"Wednesday": time.Wednesday, "Thursday": time.Thursday, "Friday": time.Friday,
		"Saturday": time.Saturday,
	}
	out := make([]time.Weekday, 0, len(names))
	for _, n := range names {
		if d, ok := byName[n]; ok {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return campaignDefaultWeekdays
	}
	return out
}

// tileHours turns a set of UTC hours into the fewest tenant.Window values that cover
// exactly those hours and no others, merging consecutive hours into one window.
//
// Hour 23 never merges past midnight into hour 0: tenant.Window has no overnight span
// (Window.Validate rejects end <= start), so a run ending at 23 always closes its own
// window at "23:59" — the latest End the model can express — rather than "24:00", which
// Window.Validate rejects outright as an invalid HH:MM. The minute 23:59:00-23:59:59 is
// therefore never coverable by any window this function builds; a known, documented
// gap in tenant.Window itself, not something introduced here.
func tileHours(hours []int, days []time.Weekday) []tenant.Window {
	sorted := append([]int(nil), hours...)
	sort.Ints(sorted)
	var out []tenant.Window
	for i := 0; i < len(sorted); {
		start := sorted[i]
		end := start
		for i+1 < len(sorted) && sorted[i+1] == end+1 {
			i++
			end = sorted[i]
		}
		i++
		endStr := fmt.Sprintf("%02d:00", end+1)
		if end == 23 {
			endStr = "23:59"
		}
		out = append(out, tenant.Window{
			Days: days, Start: fmt.Sprintf("%02d:00", start), End: endStr,
		})
	}
	return out
}

// campaignGroup is one set of activatable, non-baseline cells that will share exactly
// one keepalive_strategies row: the same arm, AND the same set of hours. Grouping any
// looser than "same hour set" would be wrong, not just coarser — a strategy's Target
// and Windows both apply to the WHOLE strategy, not per-tenant, so folding a tenant
// recommended for hours {9} together with one recommended for {9,14} into one strategy
// would activate hour 14 for the first tenant too, an hour it was never recommended
// for.
type campaignGroup struct {
	arm       string
	config    campaignArm
	hourSet   []int
	tenantIDs []string
	cells     []*resolvedCampaignCell
}

// coalesceCampaignCells groups activatable, non-baseline cells by (arm, exact hour
// set) — see campaignGroup's doc comment for why that is the loosest safe grouping.
// Baseline cells (campaignArm.baseline) and non-activatable cells are excluded: neither
// needs, nor can have, a strategy.
func coalesceCampaignCells(cells []*resolvedCampaignCell) []*campaignGroup {
	type tenantArm struct{ tenantID, arm string }
	hoursByTenantArm := map[tenantArm][]int{}
	for _, c := range cells {
		if !c.activatable || c.config.baseline {
			continue
		}
		key := tenantArm{c.tenantID, c.arm}
		hoursByTenantArm[key] = append(hoursByTenantArm[key], c.hourUTC)
	}
	for k, hrs := range hoursByTenantArm {
		sort.Ints(hrs)
		hoursByTenantArm[k] = hrs
	}

	type groupKey struct{ arm, hourSetKey string }
	keyFor := func(tenantID, arm string) groupKey {
		return groupKey{arm, hourSetKey(hoursByTenantArm[tenantArm{tenantID, arm}])}
	}
	groups := map[groupKey]*campaignGroup{}
	var order []groupKey
	for ta, hours := range hoursByTenantArm {
		gk := groupKey{ta.arm, hourSetKey(hours)}
		g := groups[gk]
		if g == nil {
			g = &campaignGroup{arm: ta.arm, config: campaignArmFor(ta.arm), hourSet: hours}
			groups[gk] = g
			order = append(order, gk)
		}
		g.tenantIDs = append(g.tenantIDs, ta.tenantID)
	}
	for _, g := range groups {
		sort.Strings(g.tenantIDs)
	}
	for _, c := range cells {
		if !c.activatable || c.config.baseline {
			continue
		}
		g := groups[keyFor(c.tenantID, c.arm)]
		g.cells = append(g.cells, c)
	}

	out := make([]*campaignGroup, 0, len(order))
	for _, gk := range order {
		out = append(out, groups[gk])
	}
	return out
}

func hourSetKey(hours []int) string {
	parts := make([]string, len(hours))
	for i, h := range hours {
		parts[i] = strconv.Itoa(h)
	}
	return strings.Join(parts, ",")
}

// strategyForGroup builds the tenant.Strategy one campaignGroup resolves to: the
// coalesced Target and the tiled Windows, at the arm's own live parameters.
func strategyForGroup(campaignName string, g *campaignGroup, days []time.Weekday) tenant.Strategy {
	return tenant.Strategy{
		Name:               fmt.Sprintf("%s: %s (%d tenants)", campaignName, g.arm, len(g.tenantIDs)),
		Active:             true,
		Windows:            tileHours(g.hourSet, days),
		Target:             tenant.Target{Mode: tenant.TargetList, TenantIDs: g.tenantIDs},
		IdleSeconds:        g.config.idleSeconds,
		MaxPings:           g.config.maxPings,
		MinPrefixTokens:    campaignDefaultMinPrefixTokens,
		PredictorID:        g.config.predictorID,
		PredictorThreshold: g.config.predictorThreshold,
		HeadTTL1h:          g.config.headTTL1h,
		HeadTTLMinTokens:   g.config.headTTLMinTokens,
	}
}

// validateGroupStrategy runs the same checks ctlCreateKeepAliveStrategy already runs on
// a manually-created strategy before persisting it — defense in depth here, since a
// strategy strategyForGroup built from campaignArmFor's own vetted constants and
// resolveCampaignCell's hour-range check should always already be valid, but "should
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
//  2. Resolve every remaining cell's arm (campaignArmFor), gating a 1h-tier arm on
//     whether THIS tenant's own traffic honors it (headTTL1hHonoredModels).
//  3. Coalesce the activatable, non-baseline cells into the fewest safe strategies
//     (coalesceCampaignCells) and create each one.
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

	resolved := make([]*resolvedCampaignCell, 0, len(suggest.Cells))
	for _, cell := range suggest.Cells {
		if cell.User == "" {
			continue
		}
		c := resolveCampaignCell(cell, honorsHeadTTL1h)
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
	if dup := duplicateCampaignCell(resolved); dup != nil {
		ctlErr(w, http.StatusBadRequest, fmt.Sprintf(
			"duplicate cell for tenant %q at hour %d", dup.tenantID, dup.hourUTC))
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
	markCampaignOverlaps(resolved, owners)

	days := weekdaysFromNames(suggest.Weekdays)
	groups := coalesceCampaignCells(resolved)
	created := make([]tenant.Strategy, 0, len(groups))
	for _, g := range groups {
		strategy := strategyForGroup(in.Name, g, days)
		if err := validateGroupStrategy(strategy); err != nil {
			// Defense in depth: campaignArmFor's own constants and resolveCampaignCell's
			// hour-range check should make this unreachable, but a coalesced strategy this
			// deployment could not actually enforce must never be silently persisted as if
			// it could — every cell that would have shared it becomes non-activatable with
			// the reason, the same as any other unenforceable arm, instead of aborting the
			// whole campaign over one bad group.
			for _, c := range g.cells {
				c.activatable = false
				c.skipReason = "could not create a valid strategy: " + err.Error()
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
		for _, c := range g.cells {
			c.strategyID = s.ID
		}
	}

	cells := make([]tenant.CampaignCell, 0, len(resolved))
	for _, c := range resolved {
		cells = append(cells, tenant.CampaignCell{
			TenantID: c.tenantID, HourUTC: c.hourUTC, Requests: c.requests, Arm: c.arm,
			PredictedUSD: c.predictedUSD, BaselineUSD: c.baselineUSD,
			OptimalSavingUSD: c.optimalSavingUSD,
			InsufficientData: c.insufficientData, Activatable: c.activatable,
			SkipReason: c.skipReason, StrategyID: c.strategyID,
		})
	}
	campaign, err := h.registry().CreateCampaign(actor.ID, tenant.Campaign{
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
			"created via campaign: "+campaign.Name); err != nil {
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
	view := viewCampaign(campaign)
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
// to live traffic (strategyForGroup sets Active: true, and nothing gates matching on
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
// live suggest call would have scanned, is in headTTL1hHonoredModels. Fails CLOSED: no
// models found (unknown traffic, or the dashboard unreachable) means "not confirmed",
// never "assume yes" — the same direction every other check in this file errs in.
func (h *Handler) tenantHonorsHeadTTL1h(r *http.Request, tenantID string, since, until int64) bool {
	if h.rec == nil || tenantID == "" {
		return false
	}
	models, err := h.rec.DB().WithContext(r.Context()).KVCacheModels(
		dash.Filter{Tenant: tenantID, Since: since, Until: until})
	if err != nil || len(models) == 0 {
		return false
	}
	for _, m := range models {
		if !headTTL1hHonoredModels[m] {
			return false
		}
	}
	return true
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
