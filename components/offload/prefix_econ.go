package offload

import (
	"math"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
)

// Economics of deliberately mutating the provider's CACHED PREFIX.
//
// Every other offloader refuses to touch the prefix (Ctx.TailOnly) because breaking the
// prefix hash at index i forces the provider to cache-WRITE everything from i onward. coref chooses to
// pay that on purpose, so the price lives here rather than being reimplemented in it.
//
//	cost    = W x (2.50 - 0.20) = 11.5 x W   (in cache-read-equivalents)
//	benefit = S x T x 0.20      =  S x T
//	worth it when  S x T > 11.5 x W
//
// S is the mass removed; W is the suffix the mutation forces the provider to re-write —
// counted from the shallowest mutated index to the CACHED boundary, because content past
// that boundary was never cached and would be written this turn regardless; T is how many
// turns remain to collect the saving on, which nobody has, so it is estimated from how
// fast the BILLED prompt has been growing on this thread (cacheRewriteHorizon).
//
// WHAT WAS HERE, AND WENT (#404 PR C). The sweep's own pricing (prefixRewriteNet, and the ask cost,
// approval, reward premium and horizon-credit terms of prefixRewritePaysWith) priced a rewrite the
// sweep chose to make on a LIVE cache, and a model ask charged against it. The sweep no longer does
// either: it asks at the keep-alive ping, where the read is already paid, and applies only on a cold
// request, where the write happens anyway. Nothing called that code but the sweep, so it went with the
// trigger it served. What is left is exactly what coref reaches.

// cacheWriteX is one cache-write in cache-read-equivalents: ($2.50 - $0.20) / $0.20 on
// Anthropic's published per-MTok prices. Shared with deploy/harbor/coref.py.
const cacheWriteX = 11.5

// Co-reference classifier defaults, shared by coref and by extract_llm's prefix
// pre-filter. One definition on purpose: if the two components classified the same output
// differently, the "free deterministic pre-filter" would be answering a different question
// from the component whose measurements calibrated it. Mirrors deploy/harbor/coref.py.
const (
	corefClosedDistDefault = 12
	corefOpenRepsDefault   = 3
	// corefMinLaterDefault is the opportunity floor: an output with fewer model turns after
	// it has not yet HAD a chance to be referenced, so "unreferenced" says nothing about it.
	corefMinLaterDefault = 8
)

// prefixRewriteWindow reports the last message index the provider is believed to already
// hold. An unknown boundary assumes the whole transcript is cached, which over-states the
// rewrite cost rather than under-stating it.
func prefixRewriteWindow(req *bschemas.BifrostChatRequest, c *components.Ctx) int {
	end := len(req.Input) - 1
	if c != nil && c.CacheAware && c.MaxCachedIdx >= 0 && c.MaxCachedIdx < end {
		end = c.MaxCachedIdx
	}
	return end
}

// prefixRewritePays applies S*T > 11.5*W for a mutation of `saved` tokens whose shallowest
// touched index is `shallowest`. Returns (needed T, estimated T, whether it clears).
//
// It uses the fallback horizon only (no billed growth). Callers that track growth use
// prefixRewritePaysWith.
func prefixRewritePays(req *bschemas.BifrostChatRequest, saved, shallowest int, c *components.Ctx) (need, have int, ok bool) {
	need, have, ok, _ = prefixRewritePaysWith(req, saved, shallowest, c, billedGrowth{})
	return need, have, ok
}

// prefixRewritePaysWith is prefixRewritePays with the thread's billed growth. It also returns the
// horizon, so the caller can count a fallback.
//
// Always clears when the context window is unknown — the same convention as every
// fraction-based threshold in this package: an unresolvable threshold imposes no
// constraint rather than silently disabling the pass.
func prefixRewritePaysWith(req *bschemas.BifrostChatRequest, saved, shallowest int, c *components.Ctx, g billedGrowth) (need, have int, ok bool, h rewriteHorizon) {
	if c == nil || c.CtxWindow <= 0 {
		return 0, 0, true, rewriteHorizon{}
	}
	if saved <= 0 {
		return 0, 0, false, rewriteHorizon{}
	}
	end := prefixRewriteWindow(req, c)
	rewritten := 0
	for j := shallowest; j <= end && j < len(req.Input); j++ {
		rewritten += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	rewritten -= saved // the removed mass is not part of what gets written back
	h = cacheRewriteHorizon(req, c, g, saved)
	have = h.turns()
	if rewritten <= 0 {
		return 0, have, true, h
	}
	need = int(math.Ceil(cacheWriteX * float64(rewritten) / float64(saved)))
	return need, have, need <= have, h
}

// estimateTurnsRemaining is the FALLBACK horizon: how many more turns fit before the request reaches
// the model's window, if the transcript keeps growing at the average rate it has so far, counted in
// LOCAL tokens per model turn.
//
// It is NOT right to an order of magnitude. A few large tool outputs move the local tokens per turn by
// 10x to 100x, and the horizon moves with them (#232). cacheRewriteHorizon is the horizon to use. It
// prices the turns left from the billed growth the provider reported on this thread. This function
// runs only when that horizon cannot be computed, and the caller counts each such use with a reason.
func estimateTurnsRemaining(reqTokens, turns, window int) int {
	if window <= 0 || turns <= 0 || reqTokens <= 0 || reqTokens >= window {
		return 0
	}
	perTurn := reqTokens / turns
	if perTurn <= 0 {
		return 0
	}
	return (window - reqTokens) / perTurn
}

// modelTurns counts assistant messages — the closest thing in a request to "steps taken",
// which is the unit the growth rate is per.
func modelTurns(req *bschemas.BifrostChatRequest) int {
	n := 0
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleAssistant {
			n++
		}
	}
	return n
}

// Horizon models and fallback reasons (rewriteHorizon.Model, rewriteHorizon.Why).
const (
	horizonModelBilled   = "billed_growth"
	horizonModelFallback = "local_estimate"

	horizonWhyNoBilled         = "no_billed_size"
	horizonWhyWindowNotExact   = "window_not_exact"
	horizonWhyGrowthUnknown    = "growth_unknown"
	horizonMaxRatio            = 8.0
	horizonMaxThreads          = 256
	horizonGrowthMaxAge        = time.Hour
	horizonTurnsCap            = math.MaxInt32
	horizonFallbackEventPrefix = "horizon_fallback_"
)

// rewriteHorizon is N, the number of turns a cache rewrite has to collect its saving on.
type rewriteHorizon struct {
	// N is the turns left. Raw before the int cast is not kept: N is floored.
	N float64
	// Model is how N was made: horizonModelBilled, or horizonModelFallback.
	Model string
	// Why is the reason for the fallback; empty for the billed model.
	Why string
}

func (h rewriteHorizon) turns() int {
	if h.N <= 0 {
		return 0
	}
	if h.N >= horizonTurnsCap {
		return horizonTurnsCap
	}
	return int(h.N)
}

// billedGrowth is one thread's growth of the billed prompt: three numbers, no conversation. from is the
// billed size at the start of the current run, turns the number of turns since, last the latest size.
type billedGrowth struct {
	from, turns, last int
}

// perTurn is the average billed growth per turn over the run, or 0 when it is unknown.
func (g billedGrowth) perTurn() float64 {
	if g.turns <= 0 || g.last <= g.from {
		return 0
	}
	return float64(g.last-g.from) / float64(g.turns)
}

// note folds the billed prompt size of the previous turn into the run. A size that did not grow
// (a client compaction, our own cut, a rewind) restarts the run from there.
func (g *billedGrowth) note(billed int) {
	if billed <= 0 {
		return
	}
	switch {
	case g.from == 0 || billed < g.last:
		g.from, g.turns = billed, 0
	case billed == g.last:
		// The same previous turn read twice (a retry, a parallel request): not a new turn.
	default:
		g.turns++
	}
	g.last = billed
}

type growthEntry struct {
	g       billedGrowth
	touched time.Time
}

var (
	growthMu      sync.Mutex
	growthThreads = map[string]*growthEntry{}
	growthNow     = time.Now // a test replaces it
)

// noteThreadGrowth records the billed size of the request's thread and returns the thread's growth.
// The map is bounded (horizonMaxThreads, evicting the least recently touched) and an entry older than
// horizonGrowthMaxAge restarts. A lost entry reads as unknown growth, which falls back.
func noteThreadGrowth(c *components.Ctx) billedGrowth {
	if c == nil {
		return billedGrowth{}
	}
	key := thread.Key(c.Session, c.Thread)
	now := growthNow()
	growthMu.Lock()
	defer growthMu.Unlock()
	e, ok := growthThreads[key]
	if ok && now.Sub(e.touched) > horizonGrowthMaxAge {
		delete(growthThreads, key)
		ok = false
	}
	if !ok {
		if len(growthThreads) >= horizonMaxThreads {
			oldestKey, oldest := "", time.Time{}
			for k, v := range growthThreads {
				if oldestKey == "" || v.touched.Before(oldest) {
					oldestKey, oldest = k, v.touched
				}
			}
			delete(growthThreads, oldestKey)
		}
		e = &growthEntry{}
		growthThreads[key] = e
	}
	e.touched = now
	e.g.note(c.PrevBilledInputThread)
	return e.g
}

// cacheRewriteHorizon is the ONE horizon for every gate that rewrites the cached prefix on purpose.
//
//		N = (cut - billed after the removal) / (billed growth per turn on this thread)
//
//	  - cut is the summarizer's trigger fraction of the window when one is in the pipeline
//	    (Ctx.CutFrac) times Ctx.FillDenominator(): the CompactionPoint when it is known, else the window.
//	  - billed is Ctx.PrevBilledInputThread (this thread only, not the session), less the removal. The removal is `removedLocal` local tokens,
//	    scaled to billed units by billed/local of this request (kept within [1, horizonMaxRatio]).
//	    So N is the room left AFTER the mutation (#232 defect 1).
//	  - growth is billed growth per turn, measured on this thread (billedGrowth).
//
// It falls back to estimateTurnsRemaining only when the billed size, the growth, or an exact window
// is unknown. The reason is in Why. A size already at the cut gives N = 0 and is not a fallback.
func cacheRewriteHorizon(req *bschemas.BifrostChatRequest, c *components.Ctx, g billedGrowth, removedLocal int) rewriteHorizon {
	local := schema.MessagesTokens(req)
	fallback := func(why string) rewriteHorizon {
		var window int
		if c != nil {
			window = c.CtxWindow
		}
		// Today's estimate, unchanged: coref's fallback does not credit the removal.
		n := estimateTurnsRemaining(local, modelTurns(req), window)
		return rewriteHorizon{N: float64(n), Model: horizonModelFallback, Why: why}
	}
	if c == nil || c.PrevBilledInputThread <= 0 {
		return fallback(horizonWhyNoBilled)
	}
	if !c.CtxWindowExact || c.CtxWindow <= 0 {
		return fallback(horizonWhyWindowNotExact)
	}
	growth := g.perTurn()
	if growth <= 0 {
		return fallback(horizonWhyGrowthUnknown)
	}
	billedAfter := float64(c.PrevBilledInputThread)
	if removedLocal > 0 && local > 0 {
		ratio := float64(c.PrevBilledInputThread) / float64(local)
		ratio = math.Min(math.Max(ratio, 1), horizonMaxRatio)
		billedAfter -= ratio * float64(removedLocal)
	}
	cut := float64(c.FillDenominator())
	if c.CutFrac > 0 {
		cut = c.CutFrac * cut
	}
	return rewriteHorizon{N: turnsUntilCut(cut, billedAfter, growth), Model: horizonModelBilled}
}

// turnsUntilCut is THE definition of the turns left: (cut - the billed prompt AFTER the removal) / (billed
// growth per turn), never below 0. cacheRewriteHorizon (coref) and the warm gate (sweep v3) both use it, so
// "a removal postpones the cut" is counted one way.
func turnsUntilCut(cut, billedAfter, growth float64) float64 {
	n := (cut - billedAfter) / growth
	if n < 0 {
		return 0
	}
	return n
}
