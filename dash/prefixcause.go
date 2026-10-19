package dash

import "github.com/rossoctl/context-guru/internal/thread"

// PrefixFacts is what one request tells us about its cache prefix, as hashes, counts and flags.
// No request body is stored. Recorder.ObservePrefix keeps the latest one per thread and compares
// the next request with it on a miss.
type PrefixFacts struct {
	Model        string
	DeclDigest   string // tool_declarations.digest; "" when the inventory was not read
	Tools        int
	SystemHash   string // "" when the system prompt was not read
	SystemBlocks int
	Effort       string
	Thinking     string
	Budget       int
	Bypassed     bool
	// Rewrote says context-guru shortened this request (tokens_after < tokens_before).
	Rewrote bool
	// RewroteEarlier is sticky: some earlier, non-bypassed request of the thread was rewritten,
	// so the provider cache holds a history that differs from the original one.
	RewroteEarlier bool
}

// FactsOf reads the prefix facts off a captured event.
func FactsOf(e *Event) PrefixFacts {
	return PrefixFacts{
		Model: e.Model, DeclDigest: e.DeclDigest, Tools: e.Tools,
		SystemHash: e.SystemHash, SystemBlocks: e.SystemBlocks,
		Effort: e.ReasoningEffort, Thinking: e.ThinkingMode, Budget: e.ThinkingBudget,
		Bypassed: e.Bypassed,
		Rewrote:  !e.Bypassed && e.TokensAfter < e.TokensBefore,
	}
}

// ObservePrefix stores cur as the thread's latest facts and returns the cause of a miss on cur:
// one of the Cause* constants, or "" when nothing differed or there is nothing to compare with.
// The caller uses the cause only on a miss. It must run once per request, in request order,
// like ObserveThread, and it takes the same key. fromThread is the thread a new thread
// inherits from (hasFrom says there is one; the primary thread is ""). It is read only when the
// thread has no facts yet.
func (r *Recorder) ObservePrefix(session, threadID, fromThread string, hasFrom bool, cur PrefixFacts, sinceLastMs int64) string {
	if r == nil {
		return ""
	}
	key := thread.Key(session, threadID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastFacts == nil {
		r.lastFacts = map[string]PrefixFacts{}
	}
	prev, ok := r.lastFacts[key]
	if !ok && hasFrom {
		prev, ok = r.lastFacts[thread.Key(session, fromThread)]
	}
	// A bypassed request re-writes the original history into the cache, which ends the effect.
	cur.RewroteEarlier = !cur.Bypassed && (prev.RewroteEarlier || prev.Rewrote)
	r.lastFacts[key] = cur
	if !ok {
		return ""
	}
	return prefixCause(prev, cur, sinceLastMs)
}

// prefixCause compares two consecutive requests of one thread. The first difference wins, in
// the order of the Cause* constants. "" means nothing measured differed.
func prefixCause(prev, cur PrefixFacts, sinceLastMs int64) string {
	switch {
	case prev.Model != cur.Model:
		return CauseModelChanged
	case cur.Bypassed && (prev.RewroteEarlier || prev.Rewrote):
		return CauseCompactionAfterRewrite
	case cur.Bypassed != prev.Bypassed:
		return CauseBypassFlip
	case prev.Tools != cur.Tools || differs(prev.DeclDigest, cur.DeclDigest):
		return CauseToolsChanged
	case prev.SystemBlocks != cur.SystemBlocks || differs(prev.SystemHash, cur.SystemHash):
		return CauseSystemChanged
	case prev.Effort != cur.Effort || prev.Thinking != cur.Thinking || prev.Budget != cur.Budget:
		return CauseEffortChanged
	case sinceLastMs >= 0 && sinceLastMs < concurrentGapMs:
		return CauseConcurrent
	}
	return ""
}

// differs is true when both hashes are known and unequal. An unread hash is no evidence.
func differs(a, b string) bool { return a != "" && b != "" && a != b }
