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
	// broken holds, per tenant, the models a 400/422 turned off. Same shape and same bounds as
	// pending (putCapped, same caps), so one tenant's 400s cannot push out another's turn-offs.
	broken map[string]map[string]time.Time
	// disabled holds, per tenant, the ToolSearch description hashes whose client-search mode is
	// disabled (#898). Same shape, TTL and caps as broken: the hash plays the model's part.
	disabled map[string]map[string]time.Time
	now      func() time.Time

	// Counters, for /stats.
	Retries   atomic.Int64 // every 400/422 on a changed request answered with the original, any cause
	TurnedOff atomic.Int64 // the Retries whose original succeeded, which turned deferral off (Break)
	Deferred  atomic.Int64 // requests that carried deferred tools
	Decisions atomic.Int64 // threads decided
	Rebuilds  atomic.Int64 // threads whose set was rebuilt after a store miss
	Unproven  atomic.Int64 // store misses sent as the original: the rule deferred a tool the transcript does not prove
	// Our own search (#825).
	AddBacks      atomic.Int64 // deferred tools our own search (#825) added back to `tools`
	ClientLoads   atomic.Int64 // deferred tools a ToolSearch answer loaded in client-search mode (#898)
	DirectCalls   atomic.Int64 // deferred tools called with no search having loaded them (#898)
	MixedSearches atomic.Int64 // searches in the same response as a client tool call
	// Client-search mode (#898): disables by reason. The production monitor reads these.
	DisabledUnknown atomic.Int64 // reason unknown_description
	DisabledBreak   atomic.Int64 // reason break
}

// The reasons ClientDisabled is set, as logged and exported.
const (
	ReasonUnknownDescription = "unknown_description"
	ReasonBreak              = "break"
)

// gateTTL bounds the turn-off map and a pending flag: a turn-off is re-tried a day later, after a
// fresh probe, and a flag whose session never sends a request is dropped.
const gateTTL = 24 * time.Hour

// maxPerTenant and maxTenants bound each per-tenant map of the Gate: the pending opt-outs and the
// turn-offs. A turn-off lost to the cap costs one more 400 and retry.
const (
	maxPerTenant = 64
	maxTenants   = 10000
)

// NewGate returns an empty Gate.
func NewGate() *Gate {
	return &Gate{pending: map[string]map[string]time.Time{}, broken: map[string]map[string]time.Time{},
		disabled: map[string]map[string]time.Time{}, now: time.Now}
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
	// Full: the flag is lost, and the session is deferred as without it.
	putCapped(g.pending, tenant, session, g.now(), maxPerTenant, maxTenants)
}

// putCapped sets m[tenant][key] to now. A tenant at perTenant entries loses its oldest one first
// (a key that is already there is only refreshed). A new tenant, when m holds maxTenants tenants,
// first takes the slot of one tenant whose entries all passed gateTTL, and is refused if there is
// none. TakePending frees a slot when a tenant's last flag is taken; a turn-off has no such taker,
// so expiry frees it. It reports whether it stored.
func putCapped(m map[string]map[string]time.Time, tenant, key string, now time.Time, perTenant, maxTenants int) bool {
	t := m[tenant]
	if t == nil {
		if len(m) >= maxTenants && !dropExpiredTenant(m, now) {
			return false
		}
		t = map[string]time.Time{}
		m[tenant] = t
	}
	if _, ok := t[key]; !ok && len(t) >= perTenant {
		pruneOldest(t)
	}
	t[key] = now
	return true
}

// dropExpiredTenant deletes one tenant of m whose entries all passed gateTTL, and reports whether
// it found one. It scans only when m is full.
func dropExpiredTenant(m map[string]map[string]time.Time, now time.Time) bool {
	for tenant, t := range m {
		live := false
		for _, at := range t {
			if now.Sub(at) < gateTTL {
				live = true
				break
			}
		}
		if !live {
			delete(m, tenant)
			return true
		}
	}
	return false
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
	delete(g.broken, tenant)
	delete(g.disabled, tenant)
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
	at, ok := g.broken[tenant][model]
	return ok && g.now().Sub(at) < gateTTL
}

// Break turns deferral off for this tenant and model, and counts it (TurnedOff only when it stored).
func (g *Gate) Break(tenant, model string) {
	if g == nil {
		return
	}
	g.Retries.Add(1)
	g.mu.Lock()
	defer g.mu.Unlock()
	if putCapped(g.broken, tenant, model, g.now(), maxPerTenant, maxTenants) {
		g.TurnedOff.Add(1)
	}
}

// ClientDisabled reports whether client-search mode is disabled for this tenant and ToolSearch
// description hash. The mark expires after gateTTL; clear it sooner by restarting the proxy.
func (g *Gate) ClientDisabled(tenant, hash string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.disabled[tenant][hash]
	return ok && g.now().Sub(at) < gateTTL
}

// DisableClient disables client-search mode for this tenant and description hash, and counts it by
// reason. It counts a reason once per live mark, so a client that sends the same unknown description
// on every request is one event, not thousands. It reports whether it counted.
func (g *Gate) DisableClient(tenant, hash, reason string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if at, live := g.disabled[tenant][hash]; live && g.now().Sub(at) < gateTTL && reason != ReasonBreak {
		return false
	}
	if !putCapped(g.disabled, tenant, hash, g.now(), maxPerTenant, maxTenants) {
		return false
	}
	switch reason {
	case ReasonUnknownDescription:
		g.DisabledUnknown.Add(1)
	default:
		g.DisabledBreak.Add(1)
	}
	return true
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
