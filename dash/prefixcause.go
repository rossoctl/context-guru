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
	// TS is the request time in epoch ms.
	TS int64
	// RewroteEarlier is sticky: some earlier, non-bypassed request of the thread was rewritten,
	// so the provider cache holds a history that differs from the original one.
	RewroteEarlier bool
}

// FactsOf reads the prefix facts off a captured event.
func FactsOf(e *Event) PrefixFacts {
	return PrefixFacts{
		TS: e.TS, Model: e.Model, DeclDigest: e.DeclDigest, Tools: e.Tools,
		SystemHash: e.SystemHash, SystemBlocks: e.SystemBlocks,
		Effort: e.ReasoningEffort, Thinking: e.ThinkingMode, Budget: e.ThinkingBudget,
		Bypassed: e.Bypassed,
		Rewrote:  !e.Bypassed && e.TokensAfter < e.TokensBefore,
	}
}

// ObservePrefix compares cur with the thread's previous request of the SAME model, and returns:
//
//   - cause: one of the Cause* constants, or "" when nothing differed or there is nothing to
//     compare with. The caller uses it only on a miss.
//   - gapMs: the time since that same-model request, the figure the TTL test must use. A cache
//     entry belongs to one model, so a request on model B in between does not refresh model A's
//     entry. -1 means the thread has no same-model request: the entry never existed, so no TTL
//     can have expired it.
//   - known: false when the thread has no facts at all. The caller then keeps the gap it
//     measured itself (a restart forgets facts but not recency).
//
// A thread whose earlier requests were all on another model reads CauseModelChanged. That is a
// miss no TTL can rescue, and it is a choice the agent made, so it stays visible as a
// prefix_change rather than hiding in cold_start.
//
// commit says whether cur becomes the thread's new baseline. A failed request must not: its
// tools or effort would then stand in for the last request that really reached the cache.
// It must run once per request, in request order, with the key ObserveThread uses. fromThread is
// the thread a new thread inherits from (hasFrom says there is one); it is read only when the
// thread has no facts yet.
func (r *Recorder) ObservePrefix(session, threadID, fromThread string, hasFrom bool, cur PrefixFacts, commit bool) (cause string, gapMs int64, known bool) {
	if r == nil {
		return "", 0, false
	}
	key := thread.Key(session, threadID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastFacts == nil {
		r.lastFacts = map[string]PrefixFacts{}
	}
	any := func(k string) (PrefixFacts, bool) { f, ok := r.lastFacts[k]; return f, ok }
	perModel := func(k string) string { return k + "\x00" + cur.Model }
	prev, same := any(perModel(key))
	_, hadAny := any(key)
	if !same && !hadAny && hasFrom {
		pk := thread.Key(session, fromThread)
		prev, same = any(perModel(pk))
		_, hadAny = any(pk)
	}
	if commit {
		// A bypassed request re-writes the original history into the cache, which ends the
		// effect of an earlier rewrite.
		cur.RewroteEarlier = same && !cur.Bypassed && (prev.RewroteEarlier || prev.Rewrote)
		r.lastFacts[key] = cur
		r.lastFacts[perModel(key)] = cur
	}
	switch {
	case same:
		gap := cur.TS - prev.TS
		return prefixCause(prev, cur, gap), gap, true
	case hadAny:
		return CauseModelChanged, -1, true
	}
	return "", 0, false
}

// prefixCause compares two consecutive requests of one thread. The first difference wins, in
// the order of the Cause* constants. Both requests are on one model (ObservePrefix picks them
// so); a model switch is named there. "" means nothing measured differed.
func prefixCause(prev, cur PrefixFacts, sinceLastMs int64) string {
	switch {
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
