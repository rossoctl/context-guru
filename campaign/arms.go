package campaign

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/tenant"
)

// The mapping of a suggest arm to a live keep-alive strategy, and the grouping of a suggest's cells into strategies. A host reads the suggest (the
// dashboard's own type), converts each cell to a Cell, calls this, and persists the result.
//
// The defaults of the 5-minute keep-alive arms are config's own (config.DefaultKeepAliveIdle, DefaultKeepAliveMaxPings, DefaultKeepAliveMinPrefix,
// DefaultHeadTTLMinTokens), never a copy. A MinPrefixTokens of 0 on a strategy means NO floor at all: every session that clears turn >= 1 is
// pinged, which is 80000x looser than the account default. So an arm that has a floor always sets it.

// legacyMinPrefixTokens is the pre-#435 floor. The 80k evidence (analysis B-policy-r2) is for K=2 ping-everyone (measured at 270 s, now run at
// 240 s); the K=1 (keepalive-5m-once), stop-reason-gated and 1h-tier arms have no separate evidence for it, so they keep the old 20k floor.
const legacyMinPrefixTokens = 20000

// The spacing of the arms that keep the simulator's schedule: 280 s on the 5-minute tier, 3360 s (56 min) on the 1-hour tier. They read the
// simulator's constants, so the two cannot drift. The live default is another value (config.DefaultKeepAliveIdle1h is 59 min); see #808.
// A re-fit of kvcache.DefaultPingIdle* now moves the live campaign arms too. The keepalive-5m arm alone runs at config.DefaultKeepAliveIdle
// (240 s): the K>=2 evidence (analysis B-policy-r2, then the owner's 240 s choice so a ping and its 60 s timeout fit the cache, #716) does not cover
// K=1 (which prefers 280) or the gate.
const (
	legacyIdle5m = int(kvcache.DefaultPingIdle / time.Second)
	legacyIdle1h = int(kvcache.DefaultPingIdle1h / time.Second)
)

// headTTLMinTokens is the size gate of a 1h-tier arm. A stored strategy keeps the token-named field (database column and manager API), which
// the head-TTL gate reads as tokens x config.HeadTTLBytesPerToken bytes: 50,000 tokens is the 200,000-byte gate. The tokens constant is
// DEPRECATED in config, but it is the right one here: the strategy field is still in tokens, and the value is config.DefaultHeadTTLMinBytes / 4.
// Every 1h-tier arm needs a positive value: a 1h strategy with a zero floor pings on the 1-hour schedule but never promotes the head to the
// 1-hour tier (the host's bounds check refuses that pair).
const headTTLMinTokens = config.DefaultHeadTTLMinTokens

// defaultPredictorThreshold is stop-reason-gated's own live threshold. A named predictor with a non-positive threshold is refused by the host's
// validator, because it would make the gate a no-op.
const defaultPredictorThreshold = 0.5

// headTTL1hHonoredModels are the models this deployment has actually measured granting
// the 1h cache tier (apply/headttl.go). A
// campaign will not mark a 1h-tier arm activatable for a tenant whose traffic in the
// suggest window used any model outside this set. Extend it only after measuring a new
// model the same way apply/headttl.go's own doc comment did — never by assumption.
var headTTL1hHonoredModels = map[string]bool{"aws/claude-haiku-4-5": true}

// Arm is how one suggest arm maps onto a live keepalive_strategies row,
// decided once per ARM NAME, never per cell: every cell recommending the same arm gets
// the same live parameters, which is what makes it safe to coalesce cells sharing an
// arm across tenants and hours into one strategy.
type Arm struct {
	// activatable is false for an arm with no live enforcement path at all — a
	// simulation-only arm. reason explains why, for CampaignCell.SkipReason.
	Activatable bool
	Reason      string
	// baseline is true for an arm that means "change nothing" (no-cache/fixed-5m/
	// fixed-1h) — a SUCCESSFUL outcome that creates no strategy, distinct from
	// activatable=false, which is a limitation, not a recommendation to do nothing.
	Baseline bool
	// needsModelHonoring is true for a 1h-tier Arm: ResolveCell additionally
	// checks headTTL1hHonoredModels for this specific tenant before activating it.
	NeedsModelHonoring bool

	IdleSeconds int
	MaxPings    int
	// predictorID and predictorThreshold always travel together: a predictorID with a
	// zero threshold would make predictorFor's gate a no-op (see validPredictorRef) —
	// pingable() ends up pinging on the arm's idle/max-ping schedule unconditionally,
	// identically to keepalive-5m, while still being labeled and billed as the
	// predictor-gated arm.
	PredictorID        string
	PredictorThreshold float64
	HeadTTL1h          bool
	HeadTTLMinTokens   int
	MinPrefixTokens    int
}

// ArmFor is the single source of truth the create flow consults. idleSeconds/
// maxPings for the 5-minute arms sit well inside validStrategyBounds' 5m band
// (minOverrideIdle..maxOverrideIdle, minOverridePings..maxOverridePings); for the
// 1h-tier arms they sit inside the 1h band (maxOverrideIdle1h, maxOverridePings1h) —
// see proxy/keepalivestrategy.go for both bands and why they differ.
func ArmFor(arm string) Arm {
	switch arm {
	case kvcache.StrategyNoCache, kvcache.StrategyFixed5m, kvcache.StrategyFixed1h:
		return Arm{Activatable: true, Baseline: true}
	case kvcache.StrategyKeepAlive5m:
		return Arm{Activatable: true, IdleSeconds: config.DefaultKeepAliveIdle, MaxPings: config.DefaultKeepAliveMaxPings,
			MinPrefixTokens: config.DefaultKeepAliveMinPrefix}
	case kvcache.StrategyKeepAlive5mOnce:
		return Arm{Activatable: true, IdleSeconds: legacyIdle5m, MaxPings: 1,
			MinPrefixTokens: legacyMinPrefixTokens}
	case kvcache.StrategyStopReasonGated:
		return Arm{Activatable: true, IdleSeconds: legacyIdle5m, MaxPings: config.DefaultKeepAliveMaxPings,
			PredictorID: "stop-reason-gated", PredictorThreshold: defaultPredictorThreshold,
			MinPrefixTokens: legacyMinPrefixTokens}
	case kvcache.StrategyKeepAlive1h:
		// legacyIdle1h is the simulator's own default cadence for this arm.
		return Arm{Activatable: true, NeedsModelHonoring: true, HeadTTL1h: true,
			IdleSeconds: legacyIdle1h, MaxPings: config.DefaultKeepAliveMaxPings,
			HeadTTLMinTokens: headTTLMinTokens,
			MinPrefixTokens:  legacyMinPrefixTokens}
	case kvcache.StrategyKeepAlive1hOnce:
		return Arm{Activatable: true, NeedsModelHonoring: true, HeadTTL1h: true,
			IdleSeconds: legacyIdle1h, MaxPings: 1, HeadTTLMinTokens: headTTLMinTokens,
			MinPrefixTokens: legacyMinPrefixTokens}
	}
	// keepalive-5m-xK: the 5-minute keep-alive with exactly K pings per idle span. The live strategy
	// carries K as MaxPings, which the simulator replays as the arm's own PingCap. Idle and floor are
	// keepalive-5m's for K >= 2 (the 80k evidence is K>=2) and keepalive-5m-once's for K = 1.
	if k, ok := kvcache.KeepAlivePingsOf(arm); ok {
		if k == 1 {
			return Arm{Activatable: true, IdleSeconds: legacyIdle5m, MaxPings: 1,
				MinPrefixTokens: legacyMinPrefixTokens}
		}
		return Arm{Activatable: true, IdleSeconds: config.DefaultKeepAliveIdle, MaxPings: k,
			MinPrefixTokens: config.DefaultKeepAliveMinPrefix}
	}
	return Arm{Reason: fmt.Sprintf(
		"%q has no live enforcement path yet — it is a simulation-only arm", arm)}
}

// Resolved is one suggest cell's outcome after resolving its arm and, for a
// 1h-tier arm, checking whether this specific tenant's traffic honors it.
type Resolved struct {
	TenantID                  string
	HourUTC                   int
	Requests                  int64
	Arm                       string
	PredictedUSD, BaselineUSD float64
	OptimalSavingUSD          float64
	InsufficientData          bool
	Activatable               bool
	SkipReason                string
	// StrategyID is filled in by Coalesce's caller once the group this
	// cell belongs to has an actual keepalive_strategies row.
	StrategyID string
	// Config is only meaningful when Activatable && !Config.Baseline.
	Config Arm
}

// ResolveCell turns one suggest cell into its outcome. honorsHeadTTL1h reports
// whether tenantID's own traffic honors the 1h tier — a function rather than a
// precomputed map so the caller can memoize per tenant however it likes and tests can
// fake it cheaply.
//
// HourUTC is bounds-checked FIRST, before the arm is even resolved: the live-fetch
// source can never produce one outside [0,23] (it's always time.Time.UTC().Hour()), but
// the upload source accepts a hand-edited suggest payload verbatim, and an out-of-range
// hour reaching TileHours would build a Window string like "24:00" that
// tenant.Window.Validate rejects — silently, if nothing catches it first, producing a
// strategy that is created successfully but can never match anything.
func ResolveCell(cell Cell, honorsHeadTTL1h func(tenantID string) bool,
) Resolved {
	out := Resolved{
		TenantID: cell.User, HourUTC: cell.HourUTC, Requests: cell.Requests,
		Arm: cell.BestStrategy, PredictedUSD: cell.SavingUSD, BaselineUSD: cell.BaselineUSD,
		OptimalSavingUSD: cell.OptimalSavingUSD, InsufficientData: cell.InsufficientData,
	}
	if cell.HourUTC < 0 || cell.HourUTC > 23 {
		out.SkipReason = fmt.Sprintf("hour_utc %d is out of range (must be 0-23)", cell.HourUTC)
		return out
	}
	a := ArmFor(cell.BestStrategy)
	if !a.Activatable {
		out.SkipReason = a.Reason
		return out
	}
	if a.NeedsModelHonoring && !honorsHeadTTL1h(cell.User) {
		out.SkipReason = "this tenant's traffic does not run on a model that honors the " +
			"1h cache tier on this deployment (see apply/headttl.go)"
		return out
	}
	// InsufficientData means the suggest engine itself does not trust this cell's
	// winning arm as a pattern (too few requests to backtest from) — activating a real,
	// live strategy off it anyway would act on exactly what that flag warns against.
	// Baseline arms are exempt: "change nothing" carries no risk regardless of sample
	// size, unlike committing to a real ping/write-tier schedule.
	if cell.InsufficientData && !a.Baseline {
		out.SkipReason = "too few requests in this cell to trust its recommendation as a pattern"
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
	if cell.UnpricedRequests > 0 && !a.Baseline {
		out.SkipReason = fmt.Sprintf("%d of this cell's %d requests could not be priced, so its "+
			"winning arm was chosen on a subset of its own traffic",
			cell.UnpricedRequests, cell.Requests)
		return out
	}
	out.Activatable = true
	out.Config = a
	return out
}

// MarkOverlaps turns any cell whose (tenant, hour) an ACTIVE campaign already
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
func MarkOverlaps(resolved []*Resolved, owners []tenant.CampaignCellOwner) {
	type key struct {
		tenantID string
		hour     int
	}
	held := make(map[key]tenant.CampaignCellOwner, len(owners))
	for _, o := range owners {
		held[key{o.TenantID, o.HourUTC}] = o
	}
	for _, c := range resolved {
		if !c.Activatable {
			continue
		}
		// Baseline cells ("change nothing") create no strategy at all, so they cannot
		// collide with one — excluding them keeps a campaign whose recommendation for an
		// hour is "keep the current policy" from being reported as conflicting with the
		// campaign that already decided the same thing.
		if c.Config.Baseline {
			continue
		}
		if o, ok := held[key{c.TenantID, c.HourUTC}]; ok {
			c.Activatable = false
			c.SkipReason = fmt.Sprintf(
				"the active campaign %q already enforces this tenant's hour %02d:00 UTC — "+
					"archive it first to re-issue this hour", o.CampaignName, c.HourUTC)
		}
	}
}

// DuplicateCell reports the first cell whose (tenantID, hourUTC) pair also
// appears earlier in resolved, or nil if every pair is unique — the same key shape
// tenant.CreateCampaign's own guard checks, run here up front so a duplicate is
// refused before any strategy is created for it, not just before the campaign record
// that would have referenced it.
func DuplicateCell(resolved []*Resolved) *Resolved {
	seen := map[[2]any]bool{}
	for _, c := range resolved {
		key := [2]any{c.TenantID, c.HourUTC}
		if seen[key] {
			return c
		}
		seen[key] = true
	}
	return nil
}

// defaultWeekdays is the work week every suggest cell's PredictedUSD is
// actually backtested against, on either input source: the live path always sets
// KVCacheSuggestions.Weekdays to this exact set (dash's own kvSuggestWeekdays), and the
// upload path accepts a hand-edited copy of that same payload. It is the fallback
// WeekdaysFromNames uses when it cannot recover any day at all from the input, rather
// than "no restriction" — see WeekdaysFromNames' own doc comment for why "every day" is
// the one outcome that must never happen silently here.
var defaultWeekdays = []time.Weekday{
	time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
}

// DefaultWeekdays returns a copy of that work week, so a caller cannot change the next caller's days.
func DefaultWeekdays() []time.Weekday { return slices.Clone(defaultWeekdays) }

// WeekdaysFromNames converts a suggest payload's Weekdays (["Sunday", "Monday", ...])
// into tenant.Window's []time.Weekday. An unrecognized name is silently dropped rather
// than erroring — the caller has no better recovery than "run with fewer restricted
// days", and the alternative (refusing the whole campaign for one bad string in a list
// this deployment itself generated) is a worse failure mode.
//
// If NOTHING is recognized — Weekdays omitted entirely by a hand-edited upload, or
// every name in it misspelled — this returns DefaultWeekdays, never an empty
// slice: tenant.Window.contains treats an empty Days as "every day matches", not "no
// day restricted", so a silent drop-to-empty here would create a live strategy that
// fires on Friday and Saturday too — traffic the frozen PredictedUSD figures were never
// backtested against (both this suggester and its own callers only ever read
// Sunday-Thursday history). "Fewer restricted days" must have a floor.
func WeekdaysFromNames(names []string) []time.Weekday {
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
		return DefaultWeekdays()
	}
	return out
}

// TileHours turns a set of UTC hours into the fewest tenant.Window values that cover
// exactly those hours and no others, merging consecutive hours into one window.
//
// Hour 23 never merges past midnight into hour 0: tenant.Window has no overnight span
// (Window.Validate rejects end <= start), so a run ending at 23 closes its own window at
// "24:00" — the end-of-day sentinel tenant.parseHHMM accepts for exactly this reason. It used
// to close at "23:59", which as an exclusive end left 23:59:00-23:59:59 covered by nothing.
func TileHours(hours []int, days []time.Weekday) []tenant.Window {
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
		// end+1 == 24 spells itself "24:00", the end-of-day sentinel tenant.parseHHMM accepts.
		// It used to be written "23:59", which as an EXCLUSIVE end stopped at 23:58:59.999 and
		// left 23:59:00-23:59:59 covered by nothing — the gap the comment above used to describe
		// as unavoidable. It is not unavoidable any more.
		endStr := fmt.Sprintf("%02d:00", end+1)
		out = append(out, tenant.Window{
			Days: days, Start: fmt.Sprintf("%02d:00", start), End: endStr,
			TZ: "UTC", // cells are UTC hours on UTC weekdays; unset would resolve to Asia/Jerusalem
		})
	}
	return out
}

// Group is one set of activatable, non-baseline cells that will share exactly
// one keepalive_strategies row: the same arm, AND the same set of hours. Grouping any
// looser than "same hour set" would be wrong, not just coarser — a strategy's Target
// and Windows both apply to the WHOLE strategy, not per-tenant, so folding a tenant
// recommended for hours {9} together with one recommended for {9,14} into one strategy
// would activate hour 14 for the first tenant too, an hour it was never recommended
// for.
type Group struct {
	Arm       string
	Config    Arm
	HourSet   []int
	TenantIDs []string
	Cells     []*Resolved
}

// Coalesce groups activatable, non-baseline cells by (arm, exact hour
// set) — see Group's doc comment for why that is the loosest safe grouping.
// Baseline cells (Arm.Baseline) and non-activatable cells are excluded: neither
// needs, nor can have, a strategy.
func Coalesce(cells []*Resolved) []*Group {
	type tenantArm struct{ tenantID, arm string }
	hoursByTenantArm := map[tenantArm][]int{}
	for _, c := range cells {
		if !c.Activatable || c.Config.Baseline {
			continue
		}
		key := tenantArm{c.TenantID, c.Arm}
		hoursByTenantArm[key] = append(hoursByTenantArm[key], c.HourUTC)
	}
	for k, hrs := range hoursByTenantArm {
		sort.Ints(hrs)
		hoursByTenantArm[k] = hrs
	}

	type groupKey struct{ arm, hourSetKey string }
	keyFor := func(tenantID, arm string) groupKey {
		return groupKey{arm, hourSetKey(hoursByTenantArm[tenantArm{tenantID, arm}])}
	}
	groups := map[groupKey]*Group{}
	var order []groupKey
	for ta, hours := range hoursByTenantArm {
		gk := groupKey{ta.arm, hourSetKey(hours)}
		g := groups[gk]
		if g == nil {
			g = &Group{Arm: ta.arm, Config: ArmFor(ta.arm), HourSet: hours}
			groups[gk] = g
			order = append(order, gk)
		}
		g.TenantIDs = append(g.TenantIDs, ta.tenantID)
	}
	for _, g := range groups {
		sort.Strings(g.TenantIDs)
	}
	for _, c := range cells {
		if !c.Activatable || c.Config.Baseline {
			continue
		}
		g := groups[keyFor(c.TenantID, c.Arm)]
		g.Cells = append(g.Cells, c)
	}

	out := make([]*Group, 0, len(order))
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

// StrategyForGroup builds the tenant.Strategy one Group resolves to: the
// coalesced Target and the tiled Windows, at the arm's own live parameters.
func StrategyForGroup(campaignName string, g *Group, days []time.Weekday) tenant.Strategy {
	return tenant.Strategy{
		Name:               fmt.Sprintf("%s: %s (%d tenants)", campaignName, g.Arm, len(g.TenantIDs)),
		Active:             true,
		Windows:            TileHours(g.HourSet, days),
		Target:             tenant.Target{Mode: tenant.TargetList, TenantIDs: g.TenantIDs},
		IdleSeconds:        g.Config.IdleSeconds,
		MaxPings:           g.Config.MaxPings,
		MinPrefixTokens:    g.Config.MinPrefixTokens,
		PredictorID:        g.Config.PredictorID,
		PredictorThreshold: g.Config.PredictorThreshold,
		HeadTTL1h:          g.Config.HeadTTL1h,
		HeadTTLMinTokens:   g.Config.HeadTTLMinTokens,
	}
}

// Cell is one suggest cell as a campaign reads it. A host fills it from its own suggest type.
type Cell struct {
	User             string
	HourUTC          int
	Requests         int64
	BestStrategy     string
	SavingUSD        float64
	BaselineUSD      float64
	OptimalSavingUSD float64
	InsufficientData bool
	UnpricedRequests int64
}

// CellOutcome is what creating a campaign would do with one uploaded suggest cell: activate it (or keep it on a baseline arm), or record it with
// skipReason. honorsHeadTTL1h reports whether a tenant's traffic honors the 1h cache tier. It is the SAME decision a host's campaign create makes,
// so an offline tool that builds an upload file reports it without a second copy of the rules.
func CellOutcome(cell Cell, honorsHeadTTL1h func(tenantID string) bool) (activatable bool, skipReason string) {
	c := ResolveCell(cell, honorsHeadTTL1h)
	return c.Activatable, c.SkipReason
}

// ArmEnforceable reports whether a campaign can enforce arm on live traffic at all. A cell can still be skipped for its own reasons (too few
// requests, unpriced rows, a 1h-tier arm on a model that does not honor it): CellOutcome answers that.
func ArmEnforceable(arm string) bool { return ArmFor(arm).Activatable }

// HeadTTL1hHonored reports whether model is measured to honor the 1h cache tier on this deployment: the set a campaign checks before it activates
// a 1h-tier arm.
func HeadTTL1hHonored(model string) bool { return headTTL1hHonoredModels[model] }

// HonorsHeadTTL1h reports whether every model a tenant used honors the 1h cache tier. It fails CLOSED: no models (unknown traffic, or the history
// unreachable) means "not confirmed", never "assume yes".
func HonorsHeadTTL1h(models []string) bool {
	if len(models) == 0 {
		return false
	}
	for _, m := range models {
		if !HeadTTL1hHonored(m) {
			return false
		}
	}
	return true
}

// TenantCell is the cell as a campaign records it.
func (c *Resolved) TenantCell() tenant.CampaignCell {
	return tenant.CampaignCell{
		TenantID: c.TenantID, HourUTC: c.HourUTC, Requests: c.Requests, Arm: c.Arm,
		PredictedUSD: c.PredictedUSD, BaselineUSD: c.BaselineUSD,
		OptimalSavingUSD: c.OptimalSavingUSD,
		InsufficientData: c.InsufficientData, Activatable: c.Activatable,
		SkipReason: c.SkipReason, StrategyID: c.StrategyID,
	}
}
