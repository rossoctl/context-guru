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
	// Messages is the message count of the request. 0 means it was not read.
	Messages int
	// PromptTokens is the request's whole prompt: fresh input + cache read + cache write.
	// A token count, so metadata only. 0 means the usage block was not read.
	PromptTokens int64
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
		Bypassed: e.Bypassed, Messages: e.Messages,
		PromptTokens: e.FreshInput + e.CacheRead + e.CacheWrite,
		Rewrote:      !e.Bypassed && e.TokensAfter < e.TokensBefore,
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
//   - lastWasSame: the thread's latest committed request ran this model, so a keep-alive ping
//     after it refreshed this model's entry (see entryAgeMs).
//   - known: false when the thread has no facts at all. The caller then keeps the gap it
//     measured itself (a restart forgets facts but not recency).
//   - prevPromptTokens: that same-model previous request's whole prompt (fresh input + cache
//     read + cache write), 0 when there is none. AttributeCache uses it to tell a hit that
//     covers the prompt from a partial read (#496).
//
// A thread whose earlier requests were all on another model reads CauseModelChanged, with two
// exceptions that read CauseFirstOnModel (stored as cold_start). Both are a first request on
// this model, and not a switch the user made:
//
//   - the request is short (1 to shortFirstMessages messages). Claude Code sends small helper
//     requests on another model; they are not a /model switch.
//   - an earlier request on this model failed. A failed request writes no cache entry, so the
//     next success starts cold. Only a 2xx request counts as "seen" for a model.
//
// commit says whether cur becomes the thread's new baseline. A failed request must not: its
// tools or effort would then stand in for the last request that really reached the cache.
// It must run once per request, in request order, with the key ObserveThread uses. fromThread is
// the thread a new thread inherits from (hasFrom says there is one); it is read only when the
// thread has no facts yet.
func (r *Recorder) ObservePrefix(session, threadID, fromThread string, hasFrom bool, cur PrefixFacts, commit bool) (cause string, gapMs int64, known, lastWasSame bool, prevPromptTokens int64) {
	if r == nil {
		return "", 0, false, false, 0
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
	last, hadAny := any(key)
	if !same && !hadAny && hasFrom {
		pk := thread.Key(session, fromThread)
		prev, same = any(perModel(pk))
		last, hadAny = any(pk)
	}
	// The thread's latest committed request was on this model, so a keep-alive ping after it
	// refreshed THIS model's entry.
	lastWasSame = hadAny && last.Model == cur.Model
	failedKey := perModel(key) + "\x00failed"
	_, failedSame := any(failedKey)
	if !commit {
		r.lastFacts[failedKey] = cur
	}
	if commit {
		delete(r.lastFacts, failedKey)
		// A bypassed request re-writes the original history into the cache, which ends the
		// effect of an earlier rewrite.
		cur.RewroteEarlier = same && !cur.Bypassed && (prev.RewroteEarlier || prev.Rewrote)
		r.lastFacts[key] = cur
		r.lastFacts[perModel(key)] = cur
	}
	switch {
	case same:
		gap := cur.TS - prev.TS
		return prefixCause(prev, cur, gap), gap, true, lastWasSame, prev.PromptTokens
	case failedSame, hadAny && cur.Messages > 0 && cur.Messages <= shortFirstMessages:
		return CauseFirstOnModel, -1, hadAny, false, 0
	case hadAny:
		return CauseModelChanged, -1, true, false, 0
	}
	return "", 0, false, false, 0
}

// shortFirstMessages is the largest message count of a request that counts as a short first
// request on a model.
const shortFirstMessages = 2

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
