package components

import (
	"math"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// Trigger is the shared, configurable gate that decides whether an expensive
// (LLM-based) component should ACT on a request — so summarize/extract run only
// when it's worth an LLM call, not on every turn. It is embedded in a
// component's config as `trigger:` and works for any agent/benchmark/use-case
// because the thresholds are pure request shape (tokens and message count), not
// task-specific.
//
// A zero field is "no constraint", so the zero Trigger fires always (backward
// compatible with configs that don't set it). Request-level thresholds
// (MinRequestTokens, MinMessages) are checked by Fires; the per-item
// MinOutputTokens floor is checked by the component against each candidate
// (e.g. extract, per tool output).
type Trigger struct {
	MinRequestTokens int `yaml:"min_request_tokens"` // whole request must be at least this many tokens
	MinMessages      int `yaml:"min_messages"`       // …and carry at least this many messages (≈ steps)
	MinOutputTokens  int `yaml:"min_output_tokens"`  // per-item floor: only offload an output at least this big

	// Context-window fractions (0 = unset) make triggers general across models:
	// each is resolved against Ctx.CtxWindow (the model's max input tokens, obtained
	// dynamically). When the window is unknown (0) fractions are ignored and only the
	// absolute thresholds apply — fully backward compatible.
	MinRequestFrac float64 `yaml:"min_request_frac"` // fire when request >= frac*window (e.g. 0.6)
	MinOutputFrac  float64 `yaml:"min_output_frac"`  // per-item: only offload an output >= frac*window
	HugeOutputFrac float64 `yaml:"huge_output_frac"` // HARD per-item trigger: a single output >= frac*window

	// CacheState restricts firing by where the request sits in its prompt cache's life:
	// "" / "any" (no constraint), "pre_expiry", "cold", "pre_expiry_or_cold". See
	// CacheAllows, and CachePhase for what the phases mean.
	CacheState string `yaml:"cache_state"`
	// PreExpirySeconds is how wide the pre-expiry window is (0 = DefaultPreExpiry). Wider
	// fires more often and invalidates more remaining lifetime; narrower fires rarely.
	// Nothing measures either side — it is the same unmeasured number extract_llm_sweep
	// carries, and for the same reason.
	PreExpirySeconds int `yaml:"pre_expiry_seconds"`
}

// CacheState values. "any" is spelled explicitly rather than left as "" because it has to be
// SAYABLE: a component whose default is a restrictive state needs an opt-out an operator can
// write down, and "" is what the settings form posts for "unset" (see config/form.go normalize).
const (
	CacheStateAny             = "any"
	CacheStatePreExpiry       = "pre_expiry"
	CacheStateCold            = "cold"
	CacheStatePreExpiryOrCold = "pre_expiry_or_cold"
)

// CacheStates is the permitted set, in display order — the Options for the settings form and
// the set a constructor validates against.
var CacheStates = []string{CacheStateAny, CacheStatePreExpiry, CacheStateCold, CacheStatePreExpiryOrCold}

// DefaultPreExpiry is the pre-expiry window's width when none is configured. One minute,
// which is this codebase's own clock-uncertainty margin for cache expiry (see apply.cacheIsCold)
// — chosen because it is the margin already trusted elsewhere, not because it was measured.
const DefaultPreExpiry = time.Minute

// PreExpiry is the configured window width, or DefaultPreExpiry.
func (t Trigger) PreExpiry() time.Duration {
	if t.PreExpirySeconds > 0 {
		return time.Duration(t.PreExpirySeconds) * time.Second
	}
	return DefaultPreExpiry
}

// CacheAllows reports whether the cache phase permits firing.
//
// UNKNOWN IS PERMITTED, AND THAT IS THE OPPOSITE OF WHAT extract_llm_sweep DOES WITH IT. The
// asymmetry is deliberate and load-bearing both ways.
//
// The sweep must not fire on Unknown because its ASK reads a cache entry that has to exist; a
// window computed from a guessed TTL would invalidate live prefixes on exactly the deployments
// whose TTL could not be read.
//
// A size-gated compactor must fire on Unknown, because Unknown is not rare or exotic: CacheTTLMs
// and IdleMs are both zero whenever the cache-aware path did not run at all — a
// non-Anthropic-family provider with no cache_control breakpoint, `cache_mode: off`, a bypassed
// turn, a session's first turn. Declining there would make the component DEAD on those
// deployments while protecting nothing: with cacheAware false, MaxCachedIdx stays -1, so
// Ctx.TailOnly already permits every offloader to rewrite deep history there. There is no live
// prefix whose invalidation we would be avoiding.
//
// An unrecognised value permits, rather than silently disabling the component. Constructors
// validate the string and refuse a bad one at config time, which is where a typo belongs.
func (t Trigger) CacheAllows(p CachePhase) bool {
	switch t.CacheState {
	case CacheStatePreExpiry:
		return p == CachePhasePreExpiry || p == CachePhaseUnknown
	case CacheStateCold:
		return p == CachePhaseCold || p == CachePhaseUnknown
	case CacheStatePreExpiryOrCold:
		return p == CachePhasePreExpiry || p == CachePhaseCold || p == CachePhaseUnknown
	default: // "" and "any"
		return true
	}
}

// FracResolvable reports whether the configured fractions can be resolved against a window worth
// trusting. False only when a fraction is actually configured AND the window is a guess or
// unknown — so a Trigger carrying no fractions is unaffected.
//
// WHY THIS IS NOT JUST `CtxWindow > 0`: the resolver's ok=true is not the same as right. Its last
// resort is a substring table whose Claude entries are `claude-sonnet-5` and a `claude` catch-all
// at 200,000, so every Opus and every Fable resolves to 200,000 against a real window of
// 1,000,000. A 0.9 fraction against that fires at 180k — five times too early, on the exact
// sessions the fraction exists to protect. Frac(0.9, 0) already evaluates to no constraint, so
// without this check an unknown window silently DROPS the size gate instead of declining.
//
// A per-item floor (OutputFloor, IsHuge) does not consult this on purpose: too small a window
// there merely raises a floor, which is the safe direction.
func (t Trigger) FracResolvable(c *Ctx) bool {
	if t.MinRequestFrac <= 0 {
		return true
	}
	return c != nil && c.CtxWindowExact && c.CtxWindow > 0
}

// frac converts a fraction of the window to an absolute token count (0 if either is unset).
func frac(f float64, window int) int {
	if f <= 0 || window <= 0 {
		return 0
	}
	return int(math.Ceil(f * float64(window)))
}

// Fires reports whether the request-level thresholds are met, given the resolved
// model context window (0 = unknown). The effective request-token threshold is the
// MAX of the absolute MinRequestTokens and the fraction MinRequestFrac*window; the
// message-count threshold is unchanged. Thresholds are ANDed; a zero threshold
// imposes no constraint. Does not consider the per-item floors (OutputFloor/IsHuge).
func (t Trigger) Fires(req *schemas.BifrostChatRequest, window int) bool {
	if t.MinMessages > 0 && len(req.Input) < t.MinMessages {
		return false
	}
	reqFloor := t.MinRequestTokens
	if f := frac(t.MinRequestFrac, window); f > reqFloor {
		reqFloor = f
	}
	if reqFloor > 0 && schema.MessagesTokens(req) < reqFloor {
		return false
	}
	return true
}

// OutputFloor is the per-item minimum size an Offload should act on: the absolute
// MinOutputTokens if set, else MinOutputFrac*window, else legacyDefault (the
// component's pre-trigger min_tokens default). Lets a component keep firing sensibly
// whether configured absolutely, as a fraction, or not at all.
func (t Trigger) OutputFloor(window, legacyDefault int) int {
	// The absolute floor is the base; the window fraction only RAISES it (never replaces
	// it). Returning the fraction outright was a footgun: on a large-window model
	// (e.g. 1M) a small frac like 0.0075 resolved to 7500, silently overriding a 1500
	// absolute and suppressing nearly all compaction. max() keeps both meaningful.
	base := legacyDefault
	if t.MinOutputTokens > 0 {
		base = t.MinOutputTokens
	}
	if f := frac(t.MinOutputFrac, window); f > base {
		return f
	}
	return base
}

// IsHuge reports whether a single tool output is large enough (>= HugeOutputFrac*window)
// to be a "huge tool call" hard trigger — worth acting on regardless of the request-level
// Fires gate. Returns false when the window is unknown or HugeOutputFrac is unset.
func (t Trigger) IsHuge(outputTokens, window int) bool {
	h := frac(t.HugeOutputFrac, window)
	return h > 0 && outputTokens >= h
}

// TriggerFields declares Trigger's keys for the settings form, under prefix (normally
// "trigger"). It lives beside the struct so a new threshold cannot be added without the
// form learning about it — the fields parity test compares these keys against the struct's
// yaml tags.
func TriggerFields(prefix string) []Field {
	p := prefix + "."
	return []Field{
		{Key: p + "min_request_tokens", Type: FieldInt, Hint: "Fire only when the whole request carries at least this many tokens (0 = no constraint)."},
		{Key: p + "min_messages", Type: FieldInt, Hint: "…and at least this many messages, which is roughly agent steps (0 = no constraint)."},
		{Key: p + "min_output_tokens", Type: FieldInt, Hint: "Per-item floor: only act on a tool output at least this big (0 = use the component's own min_tokens)."},
		{Key: p + "min_request_frac", Type: FieldFloat, Hint: "The request threshold as a fraction of the model's context window, e.g. 0.6. Raises the absolute floor, never lowers it; ignored when the window is unknown."},
		{Key: p + "min_output_frac", Type: FieldFloat, Hint: "The per-item floor as a fraction of the window. Also only ever raises the absolute one."},
		{Key: p + "huge_output_frac", Type: FieldFloat, Hint: "Hard per-item trigger: a single output at least this fraction of the window is acted on regardless of the request-level gate."},
		{Key: p + "cache_state", Type: FieldEnum, Default: CacheStateAny, Options: CacheStates,
			Hint: "Restrict firing by the prompt cache's state: any (no constraint), pre_expiry (the entry still exists but is about to expire — acting then invalidates almost nothing), cold (the entry is gone), or either. A component whose default is not `any` documents its own."},
		{Key: p + "pre_expiry_seconds", Type: FieldInt, Default: int(DefaultPreExpiry / time.Second),
			Hint: "How wide the pre-expiry window is, in seconds. Wider fires more often and invalidates more remaining cache lifetime; narrower fires rarely. Unmeasured either way."},
	}
}
