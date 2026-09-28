package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/tenant"
)

// Manager-controlled keep-alive strategies: a durable, manager-authored rule that runs a
// schedule ("only during Israel business hours") or a service-wide policy, above every
// tenant's own account config and below a per-session override — see
// docs/superpowers/specs/2026-08-25-keepalive-strategies-design.md for the full design.
//
// This file is the resolution-chain generalization (applyStrategy, alongside
// keepaliveoverride.go's overrideFor) and the control routes. Persistence lives in
// tenant/keepalivestrategy.go, because a strategy is account/control-plane data and,
// unlike a per-session override, is meant to survive a restart.

// setStrategies replaces the keeper's in-memory strategy list wholesale, under lock —
// the same swap-not-mutate pattern k.overrides documents, so a request resolving a
// policy mid-write never sees a half-built list.
func (k *keeper) setStrategies(list []tenant.Strategy) {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.strategies = list
	k.mu.Unlock()
}

// loadStrategies re-reads the strategy table from the registry: at process start
// (newKeeper) and after every create/update/delete through the control routes below, so
// "no re-push needed, since matching is live" is actually true.
//
// Fails open, per this project's hard boundary: a read error is logged and the
// in-memory list is left as it was, rather than resolving every request as if no
// strategy exists on a deployment that plainly has some.
func (k *keeper) loadStrategies() {
	if k == nil || k.h == nil || k.h.opts.Tenants == nil {
		return
	}
	list, err := k.h.registry().ListStrategies()
	if err != nil {
		slog.Warn("context-guru: could not load keep-alive strategies", "err", err)
		return
	}
	k.setStrategies(list)
}

// clockNow is the nil-safe clock resolveHeadTTL's call site in applyMode uses, so the
// write-tier half of the policy is driven by the SAME clock as the ping-scheduling half
// (keeper.record calls applyStrategy with k.now(), never time.Now() directly) — both
// halves' own doc comments already assert they resolve the same matching strategy "so
// the two can never disagree," an invariant that requires them to see the same instant,
// not just the same code path. k.now defaults to time.Now in production (newKeeper) but
// is swapped for a deterministic/simulated clock throughout this package's own test
// suite, so a caller reaching for time.Now() directly is a real, if usually invisible,
// gap between the two halves' inputs. A nil keeper (or one with no clock set, which
// should not happen outside a test that never called newKeeper) falls back to time.Now.
func (k *keeper) clockNow() time.Time {
	if k == nil || k.now == nil {
		return time.Now()
	}
	return k.now()
}

// bestStrategyFor resolves the highest-priority ACTIVE strategy matching tenantID at
// `now`, if any — the one matching walk both applyStrategy (the ping-scheduling half,
// resolved at record time) and resolveHeadTTL (the write-tier half, resolved on the
// request path) need, so the two can never disagree about which strategy won.
func (k *keeper) bestStrategyFor(tenantID string, now time.Time) *tenant.Strategy {
	k.mu.Lock()
	strategies := k.strategies
	k.mu.Unlock()
	var best *tenant.Strategy
	for i := range strategies {
		s := &strategies[i]
		if !s.Matches(now, tenantID) {
			continue
		}
		// A shadow-mode strategy matches (it is fully visible on the list route and in the
		// max-pings economics summary) but never wins live resolution — see tenant.ModeShadow.
		if !s.Enforcing() {
			continue
		}
		// A tenant this strategy has explicitly turned OFF is excluded from matching it at
		// all, even under an all-target strategy — the per-tenant off-switch the study found
		// beats every flat cap (see tenant.TenantCap).
		if c, ok := s.TenantCapFor(tenantID); ok && c.Off {
			continue
		}
		if best == nil || betterStrategy(s, best) {
			best = s
		}
	}
	return best
}

// applyStrategy resolves the highest-priority ACTIVE strategy matching tenantID at
// `now`, if any, and returns the policy with its fields replaced plus the matched
// strategy's id ("" when none matched).
//
// Called from record, between account config and the session override — see the design
// doc's resolution chain. Evaluated once, at record time, and then fixed on the entry
// for its whole lifetime, exactly as overrideFor's own resolution already is: a strategy
// whose window closes mid-hold does not retroactively un-arm a ping already scheduled.
func (k *keeper) applyStrategy(tenantID string, pol CachePolicy, now time.Time) (CachePolicy, string) {
	best := k.bestStrategyFor(tenantID, now)
	if best == nil {
		return pol, ""
	}
	pol.KeepAlive = true
	pol.Idle = time.Duration(best.IdleSeconds) * time.Second
	pol.MaxPings = best.MaxPings
	// This tenant's own override, if the strategy has one — the cap-with-off-switch
	// (bestStrategyFor already excluded an Off tenant outright; this is the narrower
	// "same strategy, lower ceiling for this one tenant" half of the same mechanism).
	if c, ok := best.TenantCapFor(tenantID); ok && c.MaxPings != nil {
		pol.MaxPings = *c.MaxPings
	}
	pol.MinPrefixTokens = best.MinPrefixTokens
	pol.MaxUSDPerPing = best.MaxUSDPerPing
	pol.PredictorID = best.PredictorID
	pol.PredictorThreshold = best.PredictorThreshold
	return pol, best.ID
}

// resolveHeadTTL resolves the same matching strategy applyStrategy would, but for the
// WRITE-TIER half of the policy — the half read on the request path (applyMode), not
// inside keeper.record where the ping half resolves. Returns the account's own
// fallback (accountHeadTTL1h, accountHeadTTLMinTokens) unchanged when no strategy
// matches, or when k itself is nil (keepalive not built into this deployment at all):
// a tenant with no matching strategy sees exactly the behaviour it had before
// strategies could touch this field.
//
// A strategy can only turn HeadTTL1h ON, never off, mirroring applyStrategy's own
// pol.KeepAlive = true — there is no "deny" strategy mode for either mechanism.
func (k *keeper) resolveHeadTTL(tenantID string, now time.Time, accountHeadTTL1h bool,
	accountHeadTTLMinTokens int) (bool, int) {
	if k == nil {
		return accountHeadTTL1h, accountHeadTTLMinTokens
	}
	best := k.bestStrategyFor(tenantID, now)
	if best == nil || !best.HeadTTL1h {
		return accountHeadTTL1h, accountHeadTTLMinTokens
	}
	return true, best.HeadTTLMinTokens
}

// betterStrategy reports whether candidate beats current under the design doc's fixed
// precedence: a list-target strategy beats an all-target one (more specific wins);
// among equally specific matches, the most recently updated wins. Not configurable —
// "no priority field to expose in the UI, one less knob a manager can get wrong."
func betterStrategy(candidate, current *tenant.Strategy) bool {
	cSpecific := candidate.Target.Mode == tenant.TargetList
	curSpecific := current.Target.Mode == tenant.TargetList
	if cSpecific != curSpecific {
		return cSpecific
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

// The 1h-tier idle/ping band, parallel to minOverrideIdle/maxOverrideIdle/
// minOverridePings/maxOverridePings but scaled to the hour-long lifetime a
// HeadTTL1h-true strategy actually holds.
//
// maxOverrideIdle1h keeps the SAME 10-second margin maxOverrideIdle leaves against the
// 5-minute lifetime (290 = 300 - 10), applied to the 1-hour one (3600 - 10) — past it
// the first ping arrives after the lifetime has already lapsed, exactly the failure
// maxOverrideIdle exists to refuse. kvcache.DefaultPingIdle1h (3360s, "the same margin
// against the one-hour lifetime" scaled up) sits comfortably inside this band as the
// value the campaign arm-mapping (a later change) actually uses; the wider band here
// only bounds what a manager could otherwise type into the form.
//
// maxOverridePings1h is capped far tighter than maxOverridePings (11): a strategy's
// caller-pays credential is held in memory for up to (MaxPings+1) x Idle for as long as
// this deployment's request path might need it to sign a ping (see kaEntry's own
// security notes in keepalive.go) — at the 5-minute tier that ceiling is documented as
// currently unreachable at 58 minutes (12 x 290s); at the 1-hour tier the SAME ping
// count would hold a credential for up to half a day, which is not a bound this
// mechanism's existing security reasoning was written to justify. 3 keeps the worst
// case at (3+1) x 3590s ≈ 4 hours, and the 1-hour tier needs far fewer refreshes to
// hold the same span in the first place (KeepAlive.Describe(): "one twelfth as many
// refreshes as the 5-minute arm").
const (
	maxOverrideIdle1h  = 3590 * time.Second
	maxOverridePings1h = 3
)

// validStrategyBounds checks the numeric fields against the SAME bounds validOverride
// already enforces for Idle/MaxPings/MinPrefixTokens — a strategy is a
// broader-blast-radius version of the same spend authorization, so it gets at least as
// tight a check, not a looser one. When headTTL1h is set the idle/ping band widens to
// maxOverrideIdle1h/maxOverridePings1h instead, since a 1-hour hold cannot be expressed
// inside the 5-minute-tier band at all.
//
// MaxUSDPerPing is the one field an override does not expose at all; a strategy may set
// it because this is an audited manager action rather than an ephemeral grant (see the
// design doc's "Model"). Checked on its own terms: non-negative, with 0 left to mean
// "use the package default" exactly like account config does (CachePolicy.Ceiling()).
//
// headTTLMinTokens only needs to be non-negative while headTTL1h is false, the same
// convention CachePolicy.HeadTTLMinTokens follows — but once headTTL1h IS set, zero is
// refused outright rather than silently accepted: apply.Opts's own gate
// (`o.HeadTTL1h && o.HeadTTLMinTokens > 0`) treats 0 as "never upgrade", so a strategy
// with HeadTTL1h=true and HeadTTLMinTokens<=0 would be created successfully, ping on the
// 1-hour schedule, and never once actually promote the head to the 1-hour tier — paying
// the ping cost for a benefit that can never happen. There is no legitimate reason to
// want that combination; account config's own Resolved() never lets this pairing exist
// either (0 there means "use DefaultHeadTTLMinTokens", not "disable the upgrade").
func validStrategyBounds(idle time.Duration, pings, minPrefix int, maxUSDPerPing float64,
	headTTL1h bool, headTTLMinTokens int) error {
	maxIdle, maxPings := maxOverrideIdle, maxOverridePings
	lifetime := 300
	if headTTL1h {
		maxIdle, maxPings = maxOverrideIdle1h, maxOverridePings1h
		lifetime = 3600
	}
	if idle < minOverrideIdle || idle > maxIdle {
		return fmt.Errorf(
			"the idle interval must be between %d and %d seconds; past %d the first ping "+
				"arrives after the provider's %d-second lifetime has already lapsed and pays "+
				"a cache WRITE instead of a read",
			int(minOverrideIdle.Seconds()), int(maxIdle.Seconds()), int(maxIdle.Seconds()), lifetime)
	}
	if pings < minOverridePings || pings > maxPings {
		return fmt.Errorf("the ping count must be between %d and %d", minOverridePings, maxPings)
	}
	if minPrefix < 0 {
		return fmt.Errorf("the prefix floor cannot be negative")
	}
	if maxUSDPerPing < 0 {
		return fmt.Errorf("the per-ping cost ceiling cannot be negative")
	}
	if headTTLMinTokens < 0 {
		return fmt.Errorf("the head-TTL token floor cannot be negative")
	}
	if headTTL1h && headTTLMinTokens <= 0 {
		return fmt.Errorf("a strategy asking for the 1h head-TTL tier needs a positive token " +
			"floor; 0 means the upgrade never fires, which pays the 1h ping schedule for no benefit")
	}
	return nil
}

// validMode checks Mode against the only two values Strategy.Enforcing() knows how to read.
// "" is refused here even though Enforcing() would read it as enforcing — every write path
// (create defaults a blank Mode to ModeShadow before this runs; patch leaves an unset Mode
// alone rather than sending "") should always have a real value by the time this checks it,
// so a caller that reaches this with "" has a bug worth surfacing rather than a row worth
// defaulting silently a second time.
func validMode(m string) error {
	if m != tenant.ModeShadow && m != tenant.ModeEnforce {
		return fmt.Errorf("mode must be %q or %q, got %q", tenant.ModeShadow, tenant.ModeEnforce, m)
	}
	return nil
}

// validTenantCaps checks only the shape: a negative override is never meaningful (zero
// already means "no pings", matching CachePolicy.MaxPings's own "zero disables"). It does
// not check that a tenant id NAMED here actually exists — a strategy may be prepared for an
// account that signs up later, the same latitude Target.TenantIDs already has.
func validTenantCaps(caps map[string]tenant.TenantCap) error {
	for id, c := range caps {
		if c.MaxPings != nil && *c.MaxPings < 0 {
			return fmt.Errorf("tenant %q's max_pings override cannot be negative", id)
		}
	}
	return nil
}

// knownPredictorIDs is the set of predictor names a strategy is allowed to reference —
// kept in lockstep with predictorFor's switch by TestKnownPredictorIDsMatchPredictorFor,
// so accepting a strategy at creation time can never promise a gate that pingable() does
// not actually know how to evaluate.
//
// "stop-reason-gated" is the first and, so far, only entry: write the five-minute tier on
// every request, ping while idle only when the request just served's own stop_reason
// clusters as kvcache.ClusterActuallyDone. Measured (docs/results/kv-ttl-predictor-arms.md)
// as +1.54% vs fixed-5m pooled (CI95 [0.60%, 2.79%]), not statistically distinguishable
// from a trained logistic regression on the same window — which is why it is a rule
// rather than a model: predictorFor never runs anything heavier than kvcache.ClusterOf in
// the hot path.
var knownPredictorIDs = map[string]bool{
	"stop-reason-gated":                    true,
	"stop-reason-gated-proceed-on-no-data": true,
}

// predictorFor resolves a predictor id to a probability function over the entry's own
// stop_reason, and reports whether the id is known. This is the whole production-safe
// predictor class: a rule, or (a future entry) a portable logistic-regression dot product
// — never an embedded model or a call out of the hot path. See kaEntry.stopReason and
// CachePolicy.PredictorID/PredictorThreshold for where the result is used.
func predictorFor(id string) (func(stopReason string) float64, bool) {
	switch id {
	case "stop-reason-gated":
		return func(stopReason string) float64 {
			if kvcache.ClusterOf(stopReason) == kvcache.ClusterActuallyDone {
				return 1.0
			}
			return 0.0
		}, true
	// stop-reason-gated-proceed-on-no-data is the same gate, with one explicit difference:
	// an EMPTY stop_reason (no signal at all, never recorded — not "recorded and
	// ambiguous") proceeds on the strategy's own idle/max-pings schedule rather than
	// silently refusing forever. Without this a deployment, or a provider, that never
	// populates stop_reason would have a stop-reason-gated strategy that LOOKS active
	// (in_window: true, pings scheduled) and pings zero times ever — the same silent
	// no-op trap head_ttl_1h already has on a gateway that honours no 1h requests at all.
	// This is a distinct, opt-in catalog entry rather than a second field on Strategy: the
	// live gate only ever sees CachePolicy.PredictorID, a single string, so the fallback IS
	// the predictor choice.
	case "stop-reason-gated-proceed-on-no-data":
		return func(stopReason string) float64 {
			if stopReason == "" {
				return 1.0
			}
			if kvcache.ClusterOf(stopReason) == kvcache.ClusterActuallyDone {
				return 1.0
			}
			return 0.0
		}, true
	}
	return nil, false
}

// validPredictorRef checks predictorID against knownPredictorIDs (empty is always
// valid — "no predictor gate") and the threshold's own shape.
//
// A named predictor paired with a threshold <= 0 is refused outright: predictorFor's
// probability functions only ever return values in [0,1], so pingable()'s
// `p(stopReason) >= threshold` is satisfied by EVERY possible prediction once threshold
// hits 0 — the gate becomes a no-op that still pings on the arm's own idle/max-ping
// schedule, exactly the "paying for a mechanism that can never actually gate anything"
// mistake validStrategyBounds already refuses for HeadTTL1h/HeadTTLMinTokens.
func validPredictorRef(predictorID string, threshold float64) error {
	s := tenant.Strategy{PredictorID: predictorID, PredictorThreshold: threshold}
	if err := s.ValidatePredictor(); err != nil {
		return err
	}
	if predictorID != "" && !knownPredictorIDs[predictorID] {
		return fmt.Errorf("%q is not a registered predictor; no predictor-gated strategies "+
			"can be created yet", predictorID)
	}
	if predictorID != "" && threshold <= 0 {
		return fmt.Errorf("a strategy naming a predictor needs a positive threshold; 0 makes " +
			"the gate a no-op, since every possible prediction satisfies \">= 0\"")
	}
	return nil
}

// validWindows checks that a strategy has at least one window (one with no schedule can
// never fire, which is never what a manager who created it meant) and that each one is
// individually valid.
func validWindows(windows []tenant.Window) error {
	if len(windows) == 0 {
		return fmt.Errorf("a strategy needs at least one window; one with no schedule can never fire")
	}
	for _, w := range windows {
		if err := w.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// keepAliveStrategyCtlRoutes is this feature's control-plane table, appended to
// ctlRoutes in control.go. Every route is ctlManager and audited, table-driven exactly
// like keepAliveCtlRoutes' own table.
func (h *Handler) keepAliveStrategyCtlRoutes() []ctlRoute {
	return []ctlRoute{
		{"GET /api/keepalive/strategies", ctlManager, h.ctlListKeepAliveStrategies},
		{"POST /api/keepalive/strategies", ctlManager, h.ctlCreateKeepAliveStrategy},
		{"PATCH /api/keepalive/strategies/{id}", ctlManager, h.ctlPatchKeepAliveStrategy},
		{"DELETE /api/keepalive/strategies/{id}", ctlManager, h.ctlDeleteKeepAliveStrategy},
		// The per-tenant max_pings economics summary — see keepalivetenanteconomics.go.
		{"GET /api/keepalive/strategies/max-pings-by-tenant", ctlManager,
			h.ctlKeepAliveMaxPingsByTenant},
		// The predictor catalog the Strategies form's dropdown reads instead of hardcoding
		// its own mirror of knownPredictorIDs — see ctlListKeepAlivePredictors.
		{"GET /api/keepalive/predictors", ctlManager, h.ctlListKeepAlivePredictors},
	}
}

// strategyView is the wire shape for one strategy, plus InWindow — the live-resolved
// "currently in a matching window: yes/no" the design doc asks the list route for, so the
// UI's at-a-glance state needs no arithmetic of its own.
type strategyView struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	IdleSeconds        int             `json:"idle_seconds"`
	MaxPings           int             `json:"max_pings"`
	MinPrefixTokens    int             `json:"min_prefix_tokens"`
	MaxUSDPerPing      float64         `json:"max_usd_per_ping"`
	Windows            []tenant.Window `json:"windows"`
	Target             tenant.Target   `json:"target"`
	Active             bool            `json:"active"`
	PredictorID        string          `json:"predictor_id"`
	PredictorThreshold float64         `json:"predictor_threshold"`
	HeadTTL1h          bool            `json:"head_ttl_1h"`
	HeadTTLMinTokens   int             `json:"head_ttl_min_tokens"`
	// Mode, TenantCaps, MaxUSDPerTenant and Models are the Strategies page's own composed
	// gates — see tenant.Strategy's doc comments. Enforcing is the live-resolved
	// counterpart to InWindow: whether this strategy is even eligible to change traffic
	// right now, so the list can show "shadow" state without the reader computing it.
	Mode            string                      `json:"mode"`
	TenantCaps      map[string]tenant.TenantCap `json:"tenant_caps,omitempty"`
	MaxUSDPerTenant float64                     `json:"max_usd_per_tenant,omitempty"`
	Models          []string                    `json:"models,omitempty"`
	Enforcing       bool                        `json:"enforcing"`
	CreatedBy       string                      `json:"created_by"`
	CreatedAt       int64                       `json:"created_at"`
	UpdatedBy       string                      `json:"updated_by"`
	UpdatedAt       int64                       `json:"updated_at"`
	InWindow        bool                        `json:"in_window"`
}

func viewStrategy(s tenant.Strategy, now time.Time) strategyView {
	return strategyView{
		ID: s.ID, Name: s.Name, IdleSeconds: s.IdleSeconds, MaxPings: s.MaxPings,
		MinPrefixTokens: s.MinPrefixTokens, MaxUSDPerPing: s.MaxUSDPerPing,
		Windows: s.Windows, Target: s.Target, Active: s.Active,
		PredictorID: s.PredictorID, PredictorThreshold: s.PredictorThreshold,
		HeadTTL1h: s.HeadTTL1h, HeadTTLMinTokens: s.HeadTTLMinTokens,
		Mode: s.Mode, TenantCaps: s.TenantCaps, MaxUSDPerTenant: s.MaxUSDPerTenant,
		Models: s.Models, Enforcing: s.Enforcing(),
		CreatedBy: s.CreatedBy, CreatedAt: msOrZero(s.CreatedAt),
		UpdatedBy: s.UpdatedBy, UpdatedAt: msOrZero(s.UpdatedAt),
		InWindow: s.InWindow(now),
	}
}

// ctlListKeepAliveStrategies lists every strategy.
func (h *Handler) ctlListKeepAliveStrategies(w http.ResponseWriter, r *http.Request) {
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
	list, err := h.registry().ListStrategies()
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not list keep-alive strategies")
		return
	}
	now := time.Now()
	out := make([]strategyView, 0, len(list))
	for _, s := range list {
		out = append(out, viewStrategy(s, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"strategies": out})
}

// strategyIn is a create request's body.
type strategyIn struct {
	Name               string          `json:"name"`
	IdleSeconds        int             `json:"idle_seconds"`
	MaxPings           int             `json:"max_pings"`
	MinPrefixTokens    int             `json:"min_prefix_tokens"`
	MaxUSDPerPing      float64         `json:"max_usd_per_ping"`
	Windows            []tenant.Window `json:"windows"`
	Target             tenant.Target   `json:"target"`
	Active             bool            `json:"active"`
	PredictorID        string          `json:"predictor_id"`
	PredictorThreshold float64         `json:"predictor_threshold"`
	HeadTTL1h          bool            `json:"head_ttl_1h"`
	HeadTTLMinTokens   int             `json:"head_ttl_min_tokens"`
	// Mode "" is filled in as tenant.ModeShadow below, before validation — the opt-in
	// default for anything new. Send tenant.ModeEnforce explicitly to skip shadow.
	Mode            string                      `json:"mode"`
	TenantCaps      map[string]tenant.TenantCap `json:"tenant_caps"`
	MaxUSDPerTenant float64                     `json:"max_usd_per_tenant"`
	Models          []string                    `json:"models"`
}

// ctlCreateKeepAliveStrategy creates a strategy: validated at least as strictly as an
// override, audited, and loaded into the keeper before the response is written — so the
// very next request may match it, with no restart.
func (h *Handler) ctlCreateKeepAliveStrategy(w http.ResponseWriter, r *http.Request) {
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
	var in strategyIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// The opt-in default for anything new: a caller that does not name a mode gets
	// shadow, never enforce. Filled in before validMode runs, so a bad explicit value is
	// still refused rather than silently corrected.
	if in.Mode == "" {
		in.Mode = tenant.ModeShadow
	}
	idle := time.Duration(in.IdleSeconds) * time.Second
	if err := validStrategyBounds(idle, in.MaxPings, in.MinPrefixTokens, in.MaxUSDPerPing,
		in.HeadTTL1h, in.HeadTTLMinTokens); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Target.Validate(); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validWindows(in.Windows); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validPredictorRef(in.PredictorID, in.PredictorThreshold); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validMode(in.Mode); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validTenantCaps(in.TenantCaps); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.MaxUSDPerTenant < 0 {
		ctlErr(w, http.StatusBadRequest, "the per-tenant budget cannot be negative")
		return
	}
	s, err := h.registry().CreateStrategy(actor.ID, tenant.Strategy{
		Name: in.Name, IdleSeconds: in.IdleSeconds, MaxPings: in.MaxPings,
		MinPrefixTokens: in.MinPrefixTokens, MaxUSDPerPing: in.MaxUSDPerPing,
		Windows: in.Windows, Target: in.Target, Active: in.Active,
		PredictorID: in.PredictorID, PredictorThreshold: in.PredictorThreshold,
		HeadTTL1h: in.HeadTTL1h, HeadTTLMinTokens: in.HeadTTLMinTokens,
		Mode: in.Mode, TenantCaps: in.TenantCaps, MaxUSDPerTenant: in.MaxUSDPerTenant,
		Models: in.Models,
	})
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The audited object is the strategy itself, named by its id in the field column —
	// there is no single target tenant for an all-target strategy, so actor and target
	// are both the manager's own id, unlike every other audited action in this codebase.
	if err := h.registry().AuditWrite(actor.ID, actor.ID, s.ID, "", "created: "+s.Name); err != nil {
		// A strategy we cannot account for is a strategy we do not keep — the same refusal
		// ctlKeepAliveArm makes when its own audit write fails.
		_ = h.registry().DeleteStrategy(s.ID)
		ctlErr(w, http.StatusInternalServerError,
			"could not record this in the audit log, so it was not created")
		return
	}
	h.keeper.loadStrategies()
	writeJSON(w, http.StatusCreated, viewStrategy(s, time.Now()))
}

// strategyPatchIn is an update request's body: pointers, so "not sent" and "set to
// zero/empty" are different things, matching tenant.Patch's own convention.
type strategyPatchIn struct {
	Name               *string                      `json:"name"`
	IdleSeconds        *int                         `json:"idle_seconds"`
	MaxPings           *int                         `json:"max_pings"`
	MinPrefixTokens    *int                         `json:"min_prefix_tokens"`
	MaxUSDPerPing      *float64                     `json:"max_usd_per_ping"`
	Windows            *[]tenant.Window             `json:"windows"`
	Target             *tenant.Target               `json:"target"`
	Active             *bool                        `json:"active"`
	PredictorID        *string                      `json:"predictor_id"`
	PredictorThreshold *float64                     `json:"predictor_threshold"`
	HeadTTL1h          *bool                        `json:"head_ttl_1h"`
	HeadTTLMinTokens   *int                         `json:"head_ttl_min_tokens"`
	Mode               *string                      `json:"mode"`
	TenantCaps         *map[string]tenant.TenantCap `json:"tenant_caps"`
	MaxUSDPerTenant    *float64                     `json:"max_usd_per_tenant"`
	Models             *[]string                    `json:"models"`
}

// ctlPatchKeepAliveStrategy updates any field; takes effect on the next request that
// would match, since matching is live.
func (h *Handler) ctlPatchKeepAliveStrategy(w http.ResponseWriter, r *http.Request) {
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
	cur, err := h.registry().StrategyByID(id)
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such strategy")
		return
	}
	var in strategyPatchIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// Validated against the RESOLVED strategy, not only the fields this call sent: a
	// patch that only touches MaxPings must not let a previously-stored Idle drift out of
	// bounds unchecked.
	next := cur
	if in.Name != nil {
		next.Name = *in.Name
	}
	if in.IdleSeconds != nil {
		next.IdleSeconds = *in.IdleSeconds
	}
	if in.MaxPings != nil {
		next.MaxPings = *in.MaxPings
	}
	if in.MinPrefixTokens != nil {
		next.MinPrefixTokens = *in.MinPrefixTokens
	}
	if in.MaxUSDPerPing != nil {
		next.MaxUSDPerPing = *in.MaxUSDPerPing
	}
	if in.Windows != nil {
		next.Windows = *in.Windows
	}
	if in.Target != nil {
		next.Target = *in.Target
	}
	if in.Active != nil {
		next.Active = *in.Active
	}
	if in.PredictorID != nil {
		next.PredictorID = *in.PredictorID
	}
	if in.PredictorThreshold != nil {
		next.PredictorThreshold = *in.PredictorThreshold
	}
	if in.HeadTTL1h != nil {
		next.HeadTTL1h = *in.HeadTTL1h
	}
	if in.HeadTTLMinTokens != nil {
		next.HeadTTLMinTokens = *in.HeadTTLMinTokens
	}
	if in.Mode != nil {
		next.Mode = *in.Mode
	}
	if in.TenantCaps != nil {
		next.TenantCaps = *in.TenantCaps
	}
	if in.MaxUSDPerTenant != nil {
		next.MaxUSDPerTenant = *in.MaxUSDPerTenant
	}
	if in.Models != nil {
		next.Models = *in.Models
	}
	idle := time.Duration(next.IdleSeconds) * time.Second
	if err := validStrategyBounds(idle, next.MaxPings, next.MinPrefixTokens, next.MaxUSDPerPing,
		next.HeadTTL1h, next.HeadTTLMinTokens); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := next.Target.Validate(); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validWindows(next.Windows); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validPredictorRef(next.PredictorID, next.PredictorThreshold); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validMode(next.Mode); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validTenantCaps(next.TenantCaps); err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if next.MaxUSDPerTenant < 0 {
		ctlErr(w, http.StatusBadRequest, "the per-tenant budget cannot be negative")
		return
	}
	s, err := h.registry().UpdateStrategy(actor.ID, id, tenant.StrategyPatch{
		Name: in.Name, IdleSeconds: in.IdleSeconds, MaxPings: in.MaxPings,
		MinPrefixTokens: in.MinPrefixTokens, MaxUSDPerPing: in.MaxUSDPerPing,
		Windows: in.Windows, Target: in.Target, Active: in.Active,
		PredictorID: in.PredictorID, PredictorThreshold: in.PredictorThreshold,
		HeadTTL1h: in.HeadTTL1h, HeadTTLMinTokens: in.HeadTTLMinTokens,
		Mode: in.Mode, TenantCaps: in.TenantCaps, MaxUSDPerTenant: in.MaxUSDPerTenant,
		Models: in.Models,
	})
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.registry().AuditWrite(actor.ID, actor.ID, id, "updated", "updated: "+s.Name); err != nil {
		// An update we cannot account for is an update we do not keep — the same refusal
		// ctlCreateKeepAliveStrategy makes when its own audit write fails. Reverted to the
		// pre-patch values fetched above, rather than left standing with no audit row.
		_, _ = h.registry().UpdateStrategy(actor.ID, id, tenant.StrategyPatch{
			Name: &cur.Name, IdleSeconds: &cur.IdleSeconds, MaxPings: &cur.MaxPings,
			MinPrefixTokens: &cur.MinPrefixTokens, MaxUSDPerPing: &cur.MaxUSDPerPing,
			Windows: &cur.Windows, Target: &cur.Target, Active: &cur.Active,
			PredictorID: &cur.PredictorID, PredictorThreshold: &cur.PredictorThreshold,
			HeadTTL1h: &cur.HeadTTL1h, HeadTTLMinTokens: &cur.HeadTTLMinTokens,
			Mode: &cur.Mode, TenantCaps: &cur.TenantCaps, MaxUSDPerTenant: &cur.MaxUSDPerTenant,
			Models: &cur.Models,
		})
		ctlErr(w, http.StatusInternalServerError,
			"could not record this in the audit log, so the update was not applied")
		return
	}
	h.keeper.loadStrategies()
	writeJSON(w, http.StatusOK, viewStrategy(s, time.Now()))
}

// ctlDeleteKeepAliveStrategy deletes a strategy. Anything currently held under it is not
// retroactively un-pinged — a ping already sent already happened — but it stops
// matching new requests immediately.
func (h *Handler) ctlDeleteKeepAliveStrategy(w http.ResponseWriter, r *http.Request) {
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
	// No body to read, so readJSON's cross-site guard does not cover it: check directly,
	// as ctlLogout and ctlManagerReset do for the same reason.
	if err := checkOrigin(r); err != nil {
		readErr(w, err)
		return
	}
	id := r.PathValue("id")
	s, err := h.registry().StrategyByID(id)
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such strategy")
		return
	}
	if err := h.registry().DeleteStrategy(id); err != nil {
		ctlErr(w, http.StatusNotFound, "no such strategy")
		return
	}
	if err := h.registry().AuditWrite(actor.ID, actor.ID, id, s.Name, "deleted"); err != nil {
		ctlErr(w, http.StatusInternalServerError, "deleted, but the audit log could not be written")
		return
	}
	h.keeper.loadStrategies()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// predictorCatalogEntry is one entry the Strategies form's predictor dropdown can show —
// see ctlListKeepAlivePredictors.
type predictorCatalogEntry struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // "baseline" | "rule" | "trained"
	Description string `json:"description"`
	// Validated is whatever check this deployment can run for that kind: a rule is
	// validated by construction (it is reviewed source, not a fit), a trained artifact by
	// its own Validate(). Selectable is the code-enforced half of "visible for comparison,
	// ineligible for live enforcement" — validPredictorRef refuses any id this is false
	// for, regardless of what a client sends, so this field is a courtesy for the UI, not
	// the actual gate.
	Validated  bool   `json:"validated"`
	Selectable bool   `json:"selectable_for_enforcement"`
	Version    string `json:"version,omitempty"`
	TrainedOn  string `json:"trained_on,omitempty"`
}

// ctlListKeepAlivePredictors lists every predictor a strategy's PredictorID could name,
// naive baseline first, then the validated rules knownPredictorIDs actually knows how to
// evaluate, then — for comparison only — kvcache's own trained reuse model. That last entry
// is NEVER added to knownPredictorIDs: predictorFor does not know how to turn a
// stop_reason-shaped signature into the richer feature vector ReuseModel needs, so it stays
// unselectable (see predictorCatalogEntry.Selectable) until someone builds that wiring and
// proves it safe — an unvalidated arm visible for comparison, refused for enforcement in
// validPredictorRef itself, not only by this field.
func (h *Handler) ctlListKeepAlivePredictors(w http.ResponseWriter, r *http.Request) {
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
	out := []predictorCatalogEntry{
		{ID: "", Kind: "baseline", Validated: true, Selectable: true,
			Description: "No predictor gate — every ping the schedule (windows, idle, max_pings) allows fires."},
		{ID: "stop-reason-gated", Kind: "rule", Validated: true, Selectable: true,
			Description: "Ping only when the request just served ended with a stop_reason that " +
				"clusters as actually-done. Measured +1.54% vs fixed-5m (CI95 [0.60%, 2.79%]); " +
				"if this deployment or provider never records stop_reason, this gate silently " +
				"pings zero times — see the next entry."},
		{ID: "stop-reason-gated-proceed-on-no-data", Kind: "rule", Validated: true, Selectable: true,
			Description: "The same gate, except an entirely absent stop_reason proceeds on the " +
				"schedule instead of refusing — the explicit no-data fallback for a deployment " +
				"where that signal is missing rather than merely ambiguous."},
	}
	if rm := kvcache.ReuseModelV1; rm != nil {
		out = append(out, predictorCatalogEntry{
			ID:   "reuse-model-" + rm.Version,
			Kind: "trained", Version: rm.Version, TrainedOn: rm.TrainedOn,
			Validated:  rm.Validate() == nil,
			Selectable: false,
			Description: "A frozen survival fit over this deployment's own keep-alive sweeps " +
				"(kvcache.ReuseModel), used today by the KV-cache page's own keepalive-budget " +
				"arm. Shown for comparison only: it reads a feature vector (prefix size, prior " +
				"gap, turn, hour, day-of-week, per-account stats) this strategy's live gate has " +
				"no path to supply — only a stop_reason. Not in knownPredictorIDs, so " +
				"validPredictorRef refuses it as a Strategy.PredictorID regardless of this flag.",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"predictors": out})
}
