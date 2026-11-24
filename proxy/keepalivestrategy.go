package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/keepalive"
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
// suite (newKeeperWith's keepalive.Options.Now), so a caller reaching for time.Now()
// directly is a real, if usually invisible, gap between the two halves' inputs. A nil
// keeper falls back to time.Now.
func (k *keeper) clockNow() time.Time {
	if k == nil {
		return time.Now()
	}
	return k.Keeper.Now()
}

// The strategy rules (which strategy wins, how it changes a policy, the bounds) live in package
// keepalive (keepalive/strategy.go), so every host resolves the same policy. The methods below
// read this keeper's strategy list under its lock and call them. The names keep the old
// unexported spelling for this package and its tests.

// strategyList returns the current strategy snapshot. The list is swapped wholesale, never mutated.
func (k *keeper) strategyList() []tenant.Strategy {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.strategies
}

// bestStrategyFor is the highest-priority ACTIVE strategy matching tenantID at now (windows
// respected), the walk both the ping half and the write-tier half (resolveHeadTTL) need.
func (k *keeper) bestStrategyFor(tenantID string, now time.Time) *tenant.Strategy {
	return k.bestStrategy(tenantID, now, false)
}

// bestStrategy is bestStrategyFor with the time windows optionally ignored (#858).
func (k *keeper) bestStrategy(tenantID string, now time.Time, ignoreWindows bool) *tenant.Strategy {
	return keepalive.BestStrategy(k.strategyList(), tenantID, now, ignoreWindows)
}

// strategyTZFor is the timezone an account's keep-alive windows are written in.
func (k *keeper) strategyTZFor(tenantID string) string {
	if k == nil {
		return tenant.DefaultStrategyTZ
	}
	return keepalive.StrategyTZ(k.strategyList(), tenantID)
}

// applyStrategy resolves the strategy matching tenantID at now and writes it onto the policy.
func (k *keeper) applyStrategy(tenantID string, pol CachePolicy, now time.Time) (CachePolicy, string) {
	return k.applyStrategyWindows(tenantID, pol, now, false)
}

// applyStrategyWindows is applyStrategy with the time windows optionally ignored (#858).
func (k *keeper) applyStrategyWindows(tenantID string, pol CachePolicy, now time.Time, ignoreWindows bool) (CachePolicy, string) {
	return keepalive.ApplyStrategy(pol, k.bestStrategy(tenantID, now, ignoreWindows))
}

// resolveHeadTTL is the write-tier half of the strategy (keepalive.ResolveHeadTTL). A nil keeper
// (no keep-alive built into this deployment) returns the account's own fallback.
func (k *keeper) resolveHeadTTL(tenantID string, now time.Time, accountHeadTTL1h bool,
	accountHeadTTLMinBytes int) (bool, int) {
	if k == nil {
		return accountHeadTTL1h, accountHeadTTLMinBytes
	}
	return keepalive.ResolveHeadTTL(k.strategyList(), tenantID, now, accountHeadTTL1h, accountHeadTTLMinBytes)
}

const (
	maxOverrideIdle1h  = keepalive.MaxOverrideIdle1h
	maxOverridePings1h = keepalive.MaxOverridePings1h
)

var (
	validStrategyBounds = keepalive.ValidStrategyBounds
	validPredictorRef   = keepalive.ValidPredictorRef
	validWindows        = keepalive.ValidWindows
	// knownPredictorIDs is the same map as keepalive's.
	knownPredictorIDs = keepalive.KnownPredictorIDs
)

// keepAliveStrategyCtlRoutes is this feature's control-plane table, appended to
// ctlRoutes in control.go. Every route is ctlManager and audited, table-driven exactly
// like keepAliveCtlRoutes' own table.
func (h *Handler) keepAliveStrategyCtlRoutes() []ctlRoute {
	return []ctlRoute{
		{"GET /api/keepalive/strategies", ctlManager, h.ctlListKeepAliveStrategies},
		{"POST /api/keepalive/strategies", ctlManager, h.ctlCreateKeepAliveStrategy},
		{"PATCH /api/keepalive/strategies/{id}", ctlManager, h.ctlPatchKeepAliveStrategy},
		{"DELETE /api/keepalive/strategies/{id}", ctlManager, h.ctlDeleteKeepAliveStrategy},
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
	OneHourOnWait      bool            `json:"one_hour_on_wait"`
	// OneHourAlways (one_hour_always): the main agent writes at 1 hour from its first request, no pings. ON MOST
	// USERS IT DOES NOT PAY OFF (hosted +22% cost against auto, 77 of 97 sessions worse; laptop -25%). #1261.
	OneHourAlways bool   `json:"one_hour_always"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedBy     string `json:"updated_by"`
	UpdatedAt     int64  `json:"updated_at"`
	InWindow      bool   `json:"in_window"`
}

func viewStrategy(s tenant.Strategy, now time.Time) strategyView {
	return strategyView{
		ID: s.ID, Name: s.Name, IdleSeconds: s.IdleSeconds, MaxPings: s.MaxPings,
		MinPrefixTokens: s.MinPrefixTokens, MaxUSDPerPing: s.MaxUSDPerPing,
		Windows: s.Windows, Target: s.Target, Active: s.Active,
		PredictorID: s.PredictorID, PredictorThreshold: s.PredictorThreshold,
		HeadTTL1h: s.HeadTTL1h, HeadTTLMinTokens: s.HeadTTLMinTokens, OneHourOnWait: s.OneHourOnWait, OneHourAlways: s.OneHourAlways,
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
	OneHourOnWait      bool            `json:"one_hour_on_wait"`
	// OneHourAlways (one_hour_always): the main agent writes at 1 hour from its first request, no pings. ON MOST
	// USERS IT DOES NOT PAY OFF (hosted +22% cost against auto, 77 of 97 sessions worse; laptop -25%). #1261.
	OneHourAlways bool `json:"one_hour_always"`
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
	s, err := h.registry().CreateStrategy(actor.ID, tenant.Strategy{
		Name: in.Name, IdleSeconds: in.IdleSeconds, MaxPings: in.MaxPings,
		MinPrefixTokens: in.MinPrefixTokens, MaxUSDPerPing: in.MaxUSDPerPing,
		Windows: in.Windows, Target: in.Target, Active: in.Active,
		PredictorID: in.PredictorID, PredictorThreshold: in.PredictorThreshold,
		HeadTTL1h: in.HeadTTL1h, HeadTTLMinTokens: in.HeadTTLMinTokens, OneHourOnWait: in.OneHourOnWait, OneHourAlways: in.OneHourAlways,
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
	Name               *string          `json:"name"`
	IdleSeconds        *int             `json:"idle_seconds"`
	MaxPings           *int             `json:"max_pings"`
	MinPrefixTokens    *int             `json:"min_prefix_tokens"`
	MaxUSDPerPing      *float64         `json:"max_usd_per_ping"`
	Windows            *[]tenant.Window `json:"windows"`
	Target             *tenant.Target   `json:"target"`
	Active             *bool            `json:"active"`
	PredictorID        *string          `json:"predictor_id"`
	PredictorThreshold *float64         `json:"predictor_threshold"`
	HeadTTL1h          *bool            `json:"head_ttl_1h"`
	HeadTTLMinTokens   *int             `json:"head_ttl_min_tokens"`
	OneHourOnWait      *bool            `json:"one_hour_on_wait"`
	OneHourAlways      *bool            `json:"one_hour_always"`
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
	if in.OneHourOnWait != nil {
		next.OneHourOnWait = *in.OneHourOnWait
	}
	if in.OneHourAlways != nil {
		next.OneHourAlways = *in.OneHourAlways
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
	s, err := h.registry().UpdateStrategy(actor.ID, id, tenant.StrategyPatch{
		Name: in.Name, IdleSeconds: in.IdleSeconds, MaxPings: in.MaxPings,
		MinPrefixTokens: in.MinPrefixTokens, MaxUSDPerPing: in.MaxUSDPerPing,
		Windows: in.Windows, Target: in.Target, Active: in.Active,
		PredictorID: in.PredictorID, PredictorThreshold: in.PredictorThreshold,
		HeadTTL1h: in.HeadTTL1h, HeadTTLMinTokens: in.HeadTTLMinTokens, OneHourOnWait: in.OneHourOnWait, OneHourAlways: in.OneHourAlways,
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
			HeadTTL1h: &cur.HeadTTL1h, HeadTTLMinTokens: &cur.HeadTTLMinTokens, OneHourOnWait: &cur.OneHourOnWait, OneHourAlways: &cur.OneHourAlways,
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
