package tooldefer

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// Gate holds the per-account off switches that are not configuration: the user's tool search
// opt-out while its session is not seen yet (from the plugin's session-start hook, on /tool-search;
// a seen session's flag lives in the record store), and the turn-off after a 400 on a request we
// changed. Safe for concurrent use.
type Gate struct {
	mu sync.Mutex
	// pending holds, per tenant, the tool search opt-outs that arrived for a session the proxy has
	// not seen yet (the hook can beat the session's first request). Its first request moves the
	// flag into the record store (TakePending). Capped per tenant, so one tenant cannot push out
	// another's.
	pending map[string]map[string]time.Time
	// broken holds tenant+model pairs a 400/422 turned off.
	broken map[[2]string]time.Time
	now    func() time.Time

	// Counters, for /stats.
	Retries   atomic.Int64 // every 400/422 on a changed request answered with the original, any cause
	TurnedOff atomic.Int64 // the Retries whose original succeeded, which turned deferral off (Break)
	Deferred  atomic.Int64 // requests that carried deferred tools
	Decisions atomic.Int64 // threads decided
	Rebuilds  atomic.Int64 // threads whose set was rebuilt after a store miss
}

// gateTTL bounds the turn-off map and a pending flag: a turn-off is re-tried a day later, after a
// fresh probe, and a flag whose session never sends a request is dropped.
const gateTTL = 24 * time.Hour

// maxGate bounds the turn-off map; past it the oldest entry goes.
const maxGate = 20000

// maxPendingPerTenant and maxPendingTenants bound the pending opt-outs.
const (
	maxPendingPerTenant = 64
	maxPendingTenants   = 10000
)

// NewGate returns an empty Gate.
func NewGate() *Gate {
	return &Gate{pending: map[string]map[string]time.Time{}, broken: map[[2]string]time.Time{}, now: time.Now}
}

// NoteToolSearch records the raw ENABLE_TOOL_SEARCH value a session's hook sent (SearchOffValue).
// For a session the proxy has seen (seen), the flag goes into the record store rs at once; for one
// it has not, it waits in this tenant's pending set until the session's first request. A value that
// is not off clears both.
func (g *Gate) NoteToolSearch(tenant, session, raw string, rs store.Store, seen bool) {
	if g == nil || session == "" {
		return
	}
	off := SearchOffValue(raw)
	g.mu.Lock()
	delete(g.pending[tenant], session)
	g.mu.Unlock()
	if seen || !off {
		// A seen session writes the store; a value that is not off clears a stored flag too.
		if rs != nil && (off || SearchOff(rs, session)) {
			SetSearchOff(rs, session, off)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.pending[tenant]
	if t == nil {
		if len(g.pending) >= maxPendingTenants {
			return // full: the flag is lost, and the session is deferred as without it
		}
		t = map[string]time.Time{}
		g.pending[tenant] = t
	}
	if len(t) >= maxPendingPerTenant {
		pruneOldest(t)
	}
	t[session] = g.now()
}

// TakePending reports whether tenant's session has a pending opt-out, and moves it into rs.
func (g *Gate) TakePending(tenant, session string, rs store.Store) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	at, ok := g.pending[tenant][session]
	if ok {
		delete(g.pending[tenant], session)
		if len(g.pending[tenant]) == 0 {
			delete(g.pending, tenant)
		}
	}
	g.mu.Unlock()
	if !ok || g.now().Sub(at) > gateTTL {
		return false
	}
	SetSearchOff(rs, session, true)
	return true
}

// DropTenant forgets everything held for tenant (a purge).
func (g *Gate) DropTenant(tenant string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, tenant)
	for k := range g.broken {
		if k[0] == tenant {
			delete(g.broken, k)
		}
	}
}

// SearchOffValue reports whether a raw ENABLE_TOOL_SEARCH value turns tool search off on purpose:
// false, 0, no or off, in any case. "auto", "auto:N", "true" and "" are not a decision.
func SearchOffValue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

// Broken reports whether a 400 turned deferral off for this tenant and model.
func (g *Gate) Broken(tenant, model string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.broken[[2]string{tenant, model}]
	return ok && g.now().Sub(at) < gateTTL
}

// Break turns deferral off for this tenant and model, and counts it.
func (g *Gate) Break(tenant, model string) {
	if g == nil {
		return
	}
	g.Retries.Add(1)
	g.TurnedOff.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.broken) >= maxGate {
		pruneOldest(g.broken)
	}
	g.broken[[2]string{tenant, model}] = g.now()
}

// pruneOldest deletes the oldest entry of m.
func pruneOldest[K comparable](m map[K]time.Time) {
	var oldK K
	var oldT time.Time
	first := true
	for k, t := range m {
		if first || t.Before(oldT) {
			oldK, oldT, first = k, t, false
		}
	}
	if !first {
		delete(m, oldK)
	}
}

// DeferralCause reports whether a provider's 400/422 body names a cause deferral can produce:
// defer_loading, a tool reference, the search tool, "all tools deferred", or cache_control on a
// deferred tool. For the log only: the proof that deferral caused a refusal is the original
// request succeeding (the host's retry).
func DeferralCause(body []byte) bool {
	b := strings.ToLower(string(body))
	for _, k := range []string{"defer_loading", "tool_reference", "tool reference", "tool_search", "at least one tool"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}
