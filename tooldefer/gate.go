package tooldefer

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Gate holds the per-account off switches that are not configuration: the user's literal
// ENABLE_TOOL_SEARCH=false (from the plugin's session-start hook, on /tool-search), and the
// turn-off after a 400 on a request we changed. Safe for concurrent use.
type Gate struct {
	mu sync.Mutex
	// searchOff holds the sessions whose user set ENABLE_TOOL_SEARCH=false, with the time.
	searchOff map[string]time.Time
	// broken holds tenant+model pairs a 400/422 turned off.
	broken map[[2]string]time.Time
	now    func() time.Time

	// Counters, for /stats.
	Retries   atomic.Int64 // every 400/422 on a changed request answered with the original, any cause
	TurnedOff atomic.Int64 // the Retries whose message named a deferral cause (Break)
	Deferred  atomic.Int64 // requests that carried deferred tools
	Decisions atomic.Int64 // threads decided
	Rebuilds  atomic.Int64 // threads whose set was rebuilt after a store miss
}

// gateTTL bounds both maps. A session flag outlives any session; a turn-off is re-tried a day
// later, after a fresh probe.
const gateTTL = 24 * time.Hour

// maxGate bounds each map; past it the oldest entries go.
const maxGate = 20000

// NewGate returns an empty Gate.
func NewGate() *Gate {
	return &Gate{searchOff: map[string]time.Time{}, broken: map[[2]string]time.Time{}, now: time.Now}
}

// SearchOff reports whether the user of session turned tool search off deliberately.
func (g *Gate) SearchOff(session string) bool {
	if g == nil || session == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.searchOff[session]
	return ok && g.now().Sub(at) < gateTTL
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

// NoteToolSearch records the raw ENABLE_TOOL_SEARCH value a session's hook sent (SearchOffValue).
func (g *Gate) NoteToolSearch(session, raw string) {
	if g == nil || session == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !SearchOffValue(raw) {
		delete(g.searchOff, session)
		return
	}
	if len(g.searchOff) >= maxGate {
		pruneOldest(g.searchOff)
	}
	g.searchOff[session] = g.now()
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
// deferred tool. Only such a refusal turns deferral off; any other 400 is the request's own fault
// (another component, the client), and the retry of the original answers it.
func DeferralCause(body []byte) bool {
	b := strings.ToLower(string(body))
	for _, k := range []string{"defer_loading", "tool_reference", "tool reference", "tool_search", "at least one tool"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}
