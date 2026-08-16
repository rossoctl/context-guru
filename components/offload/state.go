package offload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// State reuse over the generic Store (key→bytes), session-scoped by key prefix
// so a prior compaction is reused across turns instead of re-calling the LLM.
// Reusing the previous output byte-for-byte also keeps the request prefix stable
// (KV-cache friendly) — re-deriving a different summary/extraction each turn is
// both costly and cache-hostile.

// Nothing in this file is namespaced by a "compaction epoch", and that is a decision, not
// an omission. When the AGENT compacts its own transcript the session id deliberately
// stays put (one conversation, one session in the dashboard) and only the cached-prefix
// boundary restarts — see modes.Boundary. Everything here survives, because none of it
// encodes a claim about a message POSITION:
//
//   - resultKey / frozenKey are keyed by a CONTENT hash. "This content reduces to these
//     bytes" is as true after a compaction as before, and replaying it is what keeps the
//     bytes the provider already cached stable. Resetting them would re-derive — for
//     extract_llm, a fresh sampled model call that may emit DIFFERENT bytes, so the reset
//     would spend money to cause the cache-write it was supposed to avoid.
//   - sumKey holds CoveredCount, which IS an index — but it is guarded by CoveredHash, so
//     tryReuse already declines when the covered span changed, and its boundary > end
//     check already declines when the transcript got shorter than the checkpoint. A
//     compaction moves both, so the checkpoint self-invalidates and the next turn
//     summarizes fresh. An explicit reset would be a second mechanism for an outcome the
//     hash already produces.
//   - seenKey / keptKey / ownerKey are session-independent or observational by design.
//
// Keeping them is also what keeps PRE-compaction content readable: the expand markers that
// survive into the agent's summary resolve through the same content-hash keys.

// resultKey namespaces a per-content reduced output (extract) by session.
func resultKey(session, id string) string { return store.ResultPrefix + session + ":" + id }

// cachedResult is the whole replayed decision for one content id: the compacted
// projection AND the one-line summary re-emitted beside it. They live under ONE key
// because they must live and die together — as two independently-TTL'd, independently-
// pinned keys, losing only the summary made the replay HIT and emit different bytes (the
// "[summary] " segment silently vanishing) with nothing reported lost.
type cachedResult struct {
	Projected string `json:"p"`
	Summary   string `json:"s,omitempty"`
}

// getResult returns a previously cached reduced output for content id, if any. This is
// extract_llm's replay lookup, so it feeds the same hit/miss counters as reapplyFrozen —
// otherwise the shipped coding config (no mask, failed_run self-skipping) would report
// zero freeze activity while doing all of its replay through here.
func getResult(c *components.Ctx, id string) (cachedResult, bool) {
	b, ok := c.Store.Get(resultKey(c.Session, id))
	if !ok {
		frozenMisses.Add(1)
		return cachedResult{}, false
	}
	var r cachedResult
	if json.Unmarshal(b, &r) != nil || r.Projected == "" {
		frozenMisses.Add(1) // unreadable => treat as absent, never splice half a decision
		return cachedResult{}, false
	}
	frozenHits.Add(1)
	return r, true
}

// putResult caches a reduced output so a later turn re-sending the same content
// reuses it (no LLM call, byte-identical result).
func putResult(c *components.Ctx, id, projected, summary string) {
	if b, err := json.Marshal(cachedResult{Projected: projected, Summary: summary}); err == nil {
		c.Store.Put(resultKey(c.Session, id), b)
	}
}

// --- Global (session-independent) extraction result cache (#28 C) -----------
//
// The session prefix above was throwing away most of the available reuse: an extraction
// is a CONTEXT-FREE derived result, so the same content under the same extractor
// semantics reduces the same way in any session. Measured on Terminal-Bench: 82 of 103
// unique contents recurred ACROSS sessions, and ~93% of the component's realized value
// came from cache reuse rather than from new LLM calls.
//
// Contrast issue #27's xdedup index, which is session-scoped ON PURPOSE: it mints a
// conversational reference ("same as step N") that is meaningless outside its session.
// The distinction is reference vs derived result, and it decides the namespace.
//
// The key is built by extract.ResultKey (content + prompt version + model + config
// fingerprint), so a prompt bump, a model switch, or a config change MISSES rather than
// silently serving a stale extraction. The store is already bounded (TTL + LRU), which
// bounds this namespace too.

// getResultGlobal returns a previously cached reduced output for a global key.
//
// ONE key holding the whole decision, matching the session-scoped cachedResult above and
// for the same reason: as two independently-TTL'd, independently-pinned keys, losing only
// the summary made the replay HIT and emit different bytes (the "[summary] " segment
// silently vanishing) with nothing reported lost. That failure is if anything worse in the
// global namespace, where a half-decision can propagate into a session that never produced
// it.
func getResultGlobal(c *components.Ctx, gkey string) (cachedResult, bool) {
	b, ok := c.Store.Get(gkey)
	if !ok {
		return cachedResult{}, false
	}
	var r cachedResult
	if json.Unmarshal(b, &r) != nil || r.Projected == "" {
		return cachedResult{}, false // unreadable => absent, never splice half a decision
	}
	return r, true
}

// putResultGlobal caches a reduced output under its global key.
func putResultGlobal(c *components.Ctx, gkey, projected, summary string) {
	if b, err := json.Marshal(cachedResult{Projected: projected, Summary: summary}); err == nil {
		c.Store.Put(gkey, b)
	}
}

// --- Content recurrence (the economic gate's reuse signal) -------------------
//
// The gate needs to know whether content is likely to RECUR, because a compaction's
// saving is collected on every later turn it is replayed on — recurrence is what makes an
// extraction call pay for itself under caching. Recording each content key we considered
// (session-independent, like the result cache) turns "have I seen this before anywhere?"
// into a cheap store lookup.

func seenKey(ck string) string { return "cg:xseen:" + ck }

// markSeenContent records that this content was OBSERVED and reports whether it had
// ALREADY been seen (on an earlier turn, or in another session).
//
// Test-and-set in one call so the gate cannot read a flag that this same sighting just
// wrote. Marking after the gate allowed a call made first sight reclassify itself as
// recurring and collect a 50% valuation bump (6 expected reuses vs 4) it had not earned.
// Marking on observation also means a SUPPRESSED candidate still counts as seen, which is
// correct: recurrence is a property of the content, not of what we chose to spend on it.
func markSeenContent(c *components.Ctx, ck string) bool {
	k := seenKey(ck)
	_, seen := c.Store.Get(k)
	if !seen {
		c.Store.Put(k, []byte{1})
	}
	return seen
}

// --- Freeze + reapply (cache stability) -------------------------------------
//
// The cache-safety invariant: once an offloader compacts an output, it must send the
// SAME bytes for that output on every later turn — otherwise the agent (which re-sends
// the ORIGINAL each turn) makes the output flip compacted→full→compacted, churning the
// provider KV cache. A tail gate alone is not enough: it compacts in the tail then
// skips once the content is in the prefix, which is exactly that churn. So a tail-gated
// offloader FREEZES its decision here (keyed by component + original-content hash) and
// REAPPLIES it on every turn, regardless of the tail boundary. New decisions are still
// gated to the tail; frozen ones are replayed everywhere.

func frozenKey(session, comp, ck string) string {
	return store.FrozenPrefix + session + ":" + comp + ":" + ck
}

// freeze records the replacement text a component produced for an original content, so
// later turns replay it byte-for-byte.
func freeze(c *components.Ctx, comp, original, replacement string) {
	c.Store.Put(frozenKey(c.Session, comp, contentKey(original)), []byte(replacement))
}

// frozenLost reports that this component DID freeze a decision for this content and the
// store has since dropped it (TTL expiry / pin cap). It is the counterpart to a plain
// reapplyFrozen miss, which is indistinguishable from "never frozen" — and the two call
// for OPPOSITE behavior:
//
//   - never frozen: obey the tail gate. Compacting content the provider already cached
//     flips it and forces a suffix cache-write, so NEW decisions stay in the tail.
//   - frozen, then lost: the provider ALREADY holds the compacted bytes for this message,
//     so leaving it verbatim is itself the cache-destructive move. Re-derive the decision
//     even at depth. This is not a fail-open exception — an offloader's replacement text
//     is a pure function of (content, component config), and the marker key is
//     sha256(original), so re-deriving reproduces the SAME bytes the provider cached and
//     re-establishes the freeze. For an ESTABLISHED compaction the safe direction is the
//     opposite of the usual "forward the original" (see docs/design.md).
//
// Only the FACT of the freeze has to survive, not its payload — one key in a bounded set,
// which is why the signal lives in the store instead of a second content index.
func frozenLost(c *components.Ctx, key string) bool {
	fl, ok := c.Store.(store.FrozenLoser)
	return ok && fl.FrozenLost(key)
}

// reapplyFrozen replays a component's frozen decision for the message at m, if one
// exists and still shrinks it. It also refreshes the expand originals for any markers
// in the replacement (the agent re-sent the full original as m's content), so
// restoration keeps working across turns. Returns the marker keys + whether it acted.
func reapplyFrozen(c *components.Ctx, comp string, m *bschemas.ChatMessage) ([]string, int, bool) {
	content := schema.MessageText(*m)
	if isKeptVerbatim(c, contentKey(content)) {
		return nil, 0, false // agent expanded this; replaying the collapse would loop
	}
	repl, ok := c.Store.Get(frozenKey(c.Session, comp, contentKey(content)))
	if !ok {
		frozenMisses.Add(1)
		return nil, 0, false
	}
	frozenHits.Add(1)
	rs := string(repl)
	saved := schema.TextTokens(content) - schema.TextTokens(rs)
	if saved <= 0 {
		return nil, 0, false
	}
	keys := expand.ParseMarkers(rs)
	for _, k := range keys {
		c.Store.Put(k, []byte(content)) // refresh the stashed original for expand
	}
	schema.SetMessageText(m, rs)
	return keys, saved, true
}

// repairLostFreeze reports whether an offloader may compact this message even though the
// cache-tail gate would forbid it, because a freeze for it was established and then lost.
// Re-deriving reproduces the bytes the provider already cached; NOT re-deriving is what
// flips the representation and re-writes the suffix. The caller's own never-worse and
// skipReduce guards still apply, so this only ever LIFTS the depth restriction.
//
// ONLY safe for offloaders whose replacement is a pure function of (content, config):
// mask and failed_run build `prefix + headPeek(content) + Marker(sha256(content))`, which
// is position-independent — their windows (keep_recent, runs[:len-1]) gate WHETHER the
// component considers a message, never WHAT bytes it emits, and config cannot drift
// mid-session (no hot reload; a restart wipes the store with it).
//
// It is deliberately NOT offered to extract_llm. That replacement is a SAMPLED model
// output (cheapmodel sends no temperature/seed), so re-deriving may emit different bytes
// at depth — and the trade does not work even ignoring that: if the bytes differ, the
// suffix is cache-written either way, so the repair branch pays a model call for nothing.
// A lost extract_llm decision therefore just declines, like any other tail-gated miss.
func repairLostFreeze(c *components.Ctx, comp, content string) bool {
	return frozenLost(c, frozenKey(c.Session, comp, contentKey(content)))
}

// Freeze-replay counters: how often a replay landed vs found nothing. Cache-write is the
// largest cost line on long-horizon traffic and a lost freeze is the mechanism that
// produces it, so the store counts the drops and the repairs (a re-Put of a dropped
// frozen key) itself — exactly once each, regardless of how many turns observe them —
// while these two count the replay path. /stats reports all of them together.
var (
	frozenHits   atomic.Int64
	frozenMisses atomic.Int64
)

// FrozenStats returns the cumulative freeze-replay hits and misses since process start.
// Exported for the host's /stats, which pairs them with the store's drop/repair counts.
func FrozenStats() (hits, misses int64) { return frozenHits.Load(), frozenMisses.Load() }

// contentKey is a marker/whitespace-insensitive content hash (shared with extract's
// result cache), so the same output re-sent across turns maps to one frozen decision.
func contentKey(s string) string { return extract.ContentKey(s) }

// --- Kept-verbatim (expanded content) -------------------------------------
//
// When the agent expands an offloaded output, re-compacting it on the next turn would
// just make the agent expand it again — an expand loop (wasted round-trips + cache
// churn). The expand handler marks the restored content's key kept-verbatim so the
// offloaders leave it alone thereafter.

func keptKey(ck string) string { return "cg:keep:" + ck }

// MarkKeptVerbatim records that this original content was expanded and must not be
// re-compacted (keyed by content hash, session-independent). Exported for the proxy's
// expand loop, which has the restored original but not the offload Ctx.
func MarkKeptVerbatim(st store.Store, original string) {
	st.Put(keptKey(contentKey(original)), []byte{1})
}

func isKeptVerbatim(c *components.Ctx, ck string) bool {
	_, ok := c.Store.Get(keptKey(ck))
	return ok
}

// skipReduce reports whether an offloader must leave this content untouched: it
// already carries an offload marker (reducing again would double-compact and can
// orphan the earlier stash), or the agent expanded it and re-compacting would just
// trigger another expand — a per-turn bounce loop. Every offloader consults this on
// each candidate so the kept-verbatim / never-double-reduce guarantees hold uniformly.
func skipReduce(c *components.Ctx, content string) bool {
	return expand.HasPlaceholder(content) || isKeptVerbatim(c, contentKey(content))
}

// --- Stash ownership (scoping GET /expand by session) ----------------------
//
// A rewind stash is keyed by a content HASH, which is global (the same content in two
// sessions hashes the same). The model-driven expand loop is inherently same-session (a
// request only ever contains markers minted from its own content), but the management
// GET /expand endpoint takes an arbitrary id — so without a scope check any client that
// reaches the proxy could fetch another session's stashed original. We record, per
// (session, key), that this session stashed that key, and GET /expand only resolves a
// key the caller's session actually owns.

func ownerKey(session, key string) string { return "cg:own:" + session + ":" + key }

// recordOwner marks that this session stashed key (no-op for empty key / summary-off).
func recordOwner(c *components.Ctx, key string) {
	if key != "" {
		c.Store.Put(ownerKey(c.Session, key), []byte{1})
	}
}

// OwnsKey reports whether session stashed key. Exported for the proxy's GET /expand
// handler to scope retrieval to the caller's session (prevents cross-session/tenant
// disclosure of offloaded originals). Returns false when either is empty.
func OwnsKey(st store.Store, session, key string) bool {
	if session == "" || key == "" {
		return false
	}
	_, ok := st.Get(ownerKey(session, key))
	return ok
}

// sumCheckpoint is the per-session summarize state: the exact summary message
// text produced last time (re-emitted verbatim so the prefix stays byte-stable),
// how many leading span messages it subsumed, a hash of that span to prove the
// prefix is unchanged before reusing, and the stash key of the summarized span
// (its original is refreshed in the Store on reuse so expand keeps working).
type sumCheckpoint struct {
	SummaryMsg   string `json:"m"`
	CoveredCount int    `json:"c"`
	CoveredHash  string `json:"h"`
	Key          string `json:"k"`
}

func sumKey(session string) string { return "cg:sum:" + session }

func loadCheckpoint(c *components.Ctx) (sumCheckpoint, bool) {
	b, ok := c.Store.Get(sumKey(c.Session))
	if !ok {
		return sumCheckpoint{}, false
	}
	var cp sumCheckpoint
	if json.Unmarshal(b, &cp) != nil {
		return sumCheckpoint{}, false
	}
	return cp, true
}

func saveCheckpoint(c *components.Ctx, cp sumCheckpoint) {
	if b, err := json.Marshal(cp); err == nil {
		c.Store.Put(sumKey(c.Session), b)
	}
}

// spanHash is a stable content hash of a message span, used to confirm the
// covered prefix is unchanged on a later turn before reusing the summary.
func spanHash(span []bschemas.ChatMessage) string {
	h := sha256.New()
	for i := range span {
		b, _ := json.Marshal(span[i])
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
