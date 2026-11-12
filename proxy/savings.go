package proxy

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

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

	// CapCorrectionUSD and CapNotApplied are dash.SavingsTotals' fields of the same name.
	// CapNotApplied is true for the "all" scope by design: a lifetime read would stream every row
	// through the ledgers on each status-line refresh (#502), so it keeps the stored credit and
	// can be higher than the dashboard's headline.
	CapCorrectionUSD float64 `json:"cap_correction_usd"`
	CapNotApplied    bool    `json:"cap_not_applied"`
}

func savingsScope(t *dash.SavingsTotals) *SavingsScope {
	return &SavingsScope{
		CostUSD: t.CostUSD, BaselineCostUSD: t.BaselineCostUSD, CGLLMCostUSD: t.CGLLMCostUSD,
		NetSavedUSD:        t.NetSavedUSD,
		CachesplitSavedUSD: t.CachesplitSavedUSD,
		KeepAlivePingUSD:   t.KeepAlivePingUSD, KeepAliveSavedUSD: t.KeepAliveSavedUSD,
		KeepAliveNetUSD:  t.KeepAliveNetUSD,
		TotalSavedUSD:    t.TotalSavedUSD,
		CapCorrectionUSD: t.CapCorrectionUSD, CapNotApplied: t.CapNotApplied,
	}
}

// capCacheTTL is how long a scope's cap correction is reused. /stats is read by a status line that
// refreshes often, and the correction streams the scope's rows through two ledgers. The base totals
// stay live on every read; only the correction is up to this old, which is the same trade the
// dashboard's lean read makes with its own cache.
const capCacheTTL = 15 * time.Second

// capCorrectionCache holds one correction per scope key, and at most one scan per key at a time.
type capCorrectionCache struct {
	mu      sync.Mutex
	entries map[string]capCacheEntry
}

type capCacheEntry struct {
	at  time.Time
	res dash.CapResult
}

// capKey names a scope. A grouped scope is one key: the keeper lists its live sessions in map order,
// so they are sorted (on a copy) for the same set to hit the same entry.
func capKey(f dash.Filter) string {
	in := append([]string(nil), f.SessionIn...)
	sort.Strings(in)
	return f.Session + "\x00" + strings.Join(in, "\x00")
}

// get returns the cached correction for f, or runs compute under the lock and caches it. The lock
// is held across compute on purpose: a second reader of the same status line waits for the scan
// in flight and takes its result, where without it both would run the same scan.
func (c *capCorrectionCache) get(f dash.Filter, compute func() dash.CapResult) dash.CapResult {
	key := capKey(f)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && time.Since(e.at) < capCacheTTL {
		return e.res
	}
	res := compute()
	if c.entries == nil || len(c.entries) > 64 {
		c.entries = map[string]capCacheEntry{}
	}
	c.entries[key] = capCacheEntry{at: time.Now(), res: res}
	return res
}

// savingsCapped is SavingsTotals with the cap applied, the correction taken from the cache.
func (h *Handler) savingsCapped(db *dash.DB, f dash.Filter) (*dash.SavingsTotals, error) {
	s, err := db.SavingsTotals(f)
	if err != nil {
		return nil, err
	}
	s.ApplyCap(h.capCache.get(f, func() dash.CapResult {
		return db.CapCorrection(f, h.opts.Windows, h.opts.Prices)
	}))
	return s, nil
}

// SavingsStats is /stats' savings block: the same combined figure the dashboard shows,
// in three scopes, computed through dash.DB.SavingsTotals directly rather than through
// dash.API — this proxy already holds the DB (h.rec), and for a single local user's
// data the totals query is cheap enough to run synchronously. The cap's correction is not:
// it streams the scope's rows, so it comes from a short-lived cache (capCacheTTL).
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
	// "all" stays uncapped: see SavingsScope.CapNotApplied.
	if all, err := db.SavingsTotals(dash.Filter{TenantAll: true}); err == nil {
		all.CapNotApplied = true
		out.All = savingsScope(all)
	} else {
		slog.Default().Warn("cg.stats_savings_all_failed", "err", err)
	}
	if live := h.keeper.LiveSessionKeys(); len(live) > 0 {
		// One grouped query for every live session (Filter.SessionIn), not one query per
		// session — at maxKeepAliveSessions that would be up to 1024 sequential round trips.
		if sum, err := h.savingsCapped(db, dash.Filter{TenantAll: true, SessionIn: live}); err == nil {
			out.Live = savingsScope(sum)
		} else {
			slog.Default().Warn("cg.stats_savings_live_failed", "err", err)
		}
	}
	if session := h.getLastSession(); session != "" {
		if cur, err := h.savingsCapped(db, dash.Filter{TenantAll: true, Session: session}); err == nil {
			out.Current = savingsScope(cur)
		} else {
			slog.Default().Warn("cg.stats_savings_current_failed", "err", err)
		}
	}
	return out
}
