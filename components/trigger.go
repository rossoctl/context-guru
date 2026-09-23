package components

import (
	"fmt"
	"math"
	"slices"
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

	// Context-window fractions (0 = unset) make triggers general across models: each is
	// resolved against Ctx.CtxWindow (the model's max input tokens, obtained dynamically).
	// When the window is unknown (0) fractions are ignored and only the absolute thresholds
	// apply — fully backward compatible.
	//
	// MinRequestFrac is compared against Ctx.PrevBilledInput, NOT against
	// schema.MessagesTokens: a window is stated in the provider's billed units and
	// MessagesTokens counts message text only, a median 3.38x smaller. See Fires. The per-item
	// fractions below are floors on a single tool output, where our own tokenizer is both sides
	// of the comparison and no conversion arises.
	MinRequestFrac float64 `yaml:"min_request_frac"` // fire when request >= frac*window (e.g. 0.6)
	MinOutputFrac  float64 `yaml:"min_output_frac"`  // per-item: only offload an output >= frac*window
	HugeOutputFrac float64 `yaml:"huge_output_frac"` // HARD per-item trigger: a single output >= frac*window

	// CacheState restricts firing by where the request sits in its prompt cache's life:
	// "" / "any" (no constraint) or "pre_expiry". See CacheAllows, and CachePhase for what the
	// phases mean. "cold" and "pre_expiry_or_cold" were withdrawn — see removedCacheStates.
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
//
// # THERE ARE ONLY TWO, AND THAT IS THE RESULT OF A RETRACTION
//
// This key shipped with four values — `cold` and `pre_expiry_or_cold` alongside these two — on the
// argument that compaction should wait until invalidating the cached prefix was free. Three things
// retired that argument, and they are recorded on summarizeDefaultCacheState because that is where
// the default lived. In short: the downside it avoided was measured at **−$0.84 across the whole
// corpus**, the payback for firing on a WARM cache is 2-3 turns, and the two-turn commission shape
// means a cold-gated component pays the first full rewrite anyway.
//
// So there is exactly one question left worth asking about the cache, and it is not "is
// invalidation free" but "is there a live prefix for the summarizing CALL to hit". Only a component
// whose model call reuses the conversation's own prefix has that question, which is why
// `pre_expiry` survives while the two cold states do not.
const (
	CacheStateAny       = "any"
	CacheStatePreExpiry = "pre_expiry"

	// Removed. Kept as identifiers ONLY so Validate can name them in a targeted error and so this
	// comment has something to hang on: a config carrying either is refused at construction
	// rather than aliased, because every alias would change behaviour silently. See
	// removedCacheStates.
	cacheStateCold            = "cold"
	cacheStatePreExpiryOrCold = "pre_expiry_or_cold"
)

// CacheStates is the permitted set, in display order — the Options for the settings form and
// the set a constructor validates against.
var CacheStates = []string{CacheStateAny, CacheStatePreExpiry}

// removedCacheStates maps a withdrawn value to what to write instead.
//
// REFUSED, NOT ALIASED, and the distinction is the point. Either value could be mapped to a
// surviving one without a config error, but both would change when the component fires — and a
// gate that silently starts firing at a different moment is the failure mode this key's own
// history is made of. An operator who wrote `pre_expiry_or_cold` chose a behaviour that no longer
// exists; they are entitled to be told so rather than migrated.
var removedCacheStates = map[string]string{
	cacheStateCold: "cache_state: cold was withdrawn — it fired only once the prefix was already " +
		"dead, which is strictly later than any surviving value for no measured gain. Write " +
		"`any` to compact on the size gates alone (what almost every deployment wants), or " +
		"`pre_expiry` if this component's model call needs a live prefix to reuse",
	cacheStatePreExpiryOrCold: "cache_state: pre_expiry_or_cold was withdrawn — waiting for the " +
		"cache to be free to invalidate costs more than it saves (payback for firing warm is 2-3 " +
		"turns, and the corpus cost of firing warm and being wrong was −$0.84 in total). Write " +
		"`any` to compact on the size gates alone, or `pre_expiry` if this component's model call " +
		"needs a live prefix to reuse",
}

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
func (t Trigger) CacheAllows(c *Ctx, p CachePhase) bool {
	// UNKNOWN CANNOT ESTABLISH THAT A PREFIX IS ALIVE, which is the whole reason this guard exists —
	// and it is NOT the reason it was written. The original argument was that compacting over a live
	// prefix is expensive, so a phase that hides one must decline. That argument is withdrawn: the
	// shipped default now compacts over live prefixes deliberately, because it pays back in 2-3
	// turns (see summarizeDefaultCacheState). Leaving the old prose here would put two opposite
	// valuations of one event in a single package, which is the defect this key's history is made of.
	//
	// THE GUARD SURVIVES BECAUSE ITS ONLY REMAINING CALLER ASKS A DIFFERENT QUESTION. With `""` and
	// `any` exempt below, the sole state that reaches this condition is `pre_expiry`, and
	// `pre_expiry` does not mean "invalidation is cheap here" — it means "my model call reuses this
	// prefix, so it has to be provably still alive". A request carrying MaxCachedIdx >= 0 with no
	// readable TTL says there IS a cached prefix and says nothing about its remaining life, so it
	// cannot establish that. Declining is then the honest answer rather than a cautious one: firing
	// would pay fresh prefill for the whole conversation with nothing guaranteed to hit.
	//
	// WHAT THAT MEANS FOR THE DEFAULT IS A DECISION, not a leftover scoping clause. Two routes reach
	// this state — apply's legacy no-Tracker path, which every library consumer of
	// BodyFull/BodyOpts takes, and `cache_mode: on` against a provider whose TTL this repo does not
	// derive; both leave MaxCachedIdx set and CacheTTLMs at zero. On those deployments the default
	// now compacts across a prefix whose remaining life is unknown, and that is intended: the
	// payback argument does not depend on knowing the lifetime, only on the session continuing. A
	// third route — two turns in the same millisecond reading as zero-idle-therefore-unknown, which
	// a review observed opening the old gate over a live 8-message prefix on 13 of 26 turns — was
	// fixed at its root in apply and is no longer reachable.
	//
	// ⚠️ DO NOT DELETE THE `any` CLAUSE BELOW ON THE STRENGTH OF THAT RETRACTION. It used to exempt
	// an opt-out; it now exempts the DEFAULT, so removing it stops summarize firing at all on both
	// routes above — a much larger consequence than when it was written, and one no reading of the
	// paragraphs above would predict. TestTheDefaultStillFiresWhenAnUnknownPhaseHidesALivePrefix is
	// the guard on the guard.
	if p == CachePhaseUnknown && c != nil && c.CacheAware && c.MaxCachedIdx >= 0 &&
		t.CacheState != "" && t.CacheState != CacheStateAny {
		return false
	}
	switch t.CacheState {
	case CacheStatePreExpiry:
		return p == CachePhasePreExpiry || p == CachePhaseUnknown
	default: // "" and "any"
		// A WITHDRAWN VALUE LANDS HERE, and it is the right place for it to land. Validate refuses
		// `cold` and `pre_expiry_or_cold` at construction, so the only way either reaches this
		// switch is a Trigger built as a struct literal in code that skipped the constructor —
		// tests, and any future in-repo caller. Permitting is then the fail-open answer and it
		// matches the guidance Validate prints for both.
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
	// Three facts, all required, and the third is the one this function was missing when it
	// shipped. A fraction of the window has to be compared against a figure measured the way
	// the window is measured — the provider's own billed input — and Ctx.PrevBilledInput is
	// the only such figure available. 0 means this session has no earlier response to read
	// from, so the fill is unknown and there is nothing honest to compare.
	return c != nil && c.CtxWindowExact && c.CtxWindow > 0 && c.PrevBilledInput > 0
}

// frac converts a fraction of the window to an absolute token count (0 if either is unset).
func frac(f float64, window int) int {
	if f <= 0 || window <= 0 {
		return 0
	}
	return int(math.Ceil(f * float64(window)))
}

// Fires reports whether the request-level thresholds are met. Thresholds are ANDed; a zero
// threshold imposes no constraint. Does not consider the per-item floors (OutputFloor/IsHuge).
//
// # The two size thresholds are measured on DIFFERENT RULERS, and are therefore separate
//
// This function used to take `reqFloor = max(MinRequestTokens, frac(MinRequestFrac, window))`
// and compare the result against schema.MessagesTokens. That is a units error, and it made the
// fraction unreachable rather than merely inaccurate:
//
//   - MessagesTokens counts message TEXT ONLY — no system prompt, no tool declarations, no JSON
//     envelope. MinRequestTokens has always been stated in those units and still is.
//   - A context window is stated in the units the PROVIDER bills, which include all of the
//     above. Measured on uncompacted production traffic the provider's count runs a median
//     3.38x higher than MessagesTokens (p25 2.43, p90 6.80 — dash/overview.go's
//     EstimatorDivergence).
//
// So `MessagesTokens >= 0.9 * 1_000_000` really asks for about 3M provider tokens on a 1M model:
// the request is rejected upstream, or the client compacts, long before it can be true. A gate
// that never fires looks exactly like a gate that is working, which is why this is worth the
// paragraph.
//
// max() cannot be salvaged either — taking the larger of two numbers on two different rulers is
// meaningless. They are ANDed as separate conjuncts, each against the figure measured its own
// way. For a config that sets only one (the common case, and every shipped default) this changes
// nothing; for one that sets both it is stricter, and honestly so.
//
// The fraction is skipped when PrevBilledInput is 0 rather than treated as a zero fill — see
// FracResolvable, which is the conjunct a caller uses to tell "not full enough" from "cannot
// tell", and which summarize counts separately.
//
// # AND THE DENOMINATOR IS C, NOT THE MODEL WINDOW
//
// "90% full" has to mean 90% of the way to the point where the conversation's own compaction
// mechanism acts — Ctx.CompactionPoint — and not 90% of the model's window. That IS the argument for
// this component: compacting just before the client would have acted captures the saving of a large
// prefix going cold and costs no accuracy that was not already going to be lost, because a compaction
// was going to happen there anyway. Against the window it is only correct when C equals the window.
//
// The direction of the error is what makes it worth fixing rather than noting. A client that compacts
// EARLY — Claude Code with a lowered auto-compact threshold, or any agent that caps its own context —
// resets the transcript before billed input ever reaches `frac x window`, so the component NEVER
// FIRES on that deployment, silently, and looks exactly like a gate that is working. Measured on
// haiku behind the current client C is 0.996 of the window, so the two denominators are nearly the
// same there and this branch's own acceptance run was not affected; that is luck, not design.
//
// See Ctx.FillDenominator for the fallback, and internal/compactionpoint for the definition.
func (t Trigger) Fires(req *schemas.BifrostChatRequest, c *Ctx) bool {
	if t.MinMessages > 0 && len(req.Input) < t.MinMessages {
		return false
	}
	if t.MinRequestTokens > 0 && schema.MessagesTokens(req) < t.MinRequestTokens {
		return false
	}
	denom, billed := 0, 0
	if c != nil {
		denom, billed = c.FillDenominator(), c.PrevBilledInput
	}
	if f := frac(t.MinRequestFrac, denom); f > 0 && billed > 0 && billed < f {
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
// TriggerFields declares every key a Trigger accepts, for every component that embeds one.
//
// It is deliberately NOT split per component, and an attempt to split it is what established that.
// `extract` and `extract_llm` consult neither CacheAllows nor CachePhase, so their `cache_state`
// control did nothing — and the obvious fix, hiding the key from their form, breaks this repo's own
// contract: TestEveryComponentDeclaresExactlyItsConfigurableKeys requires that a key the config
// STRUCT accepts is declared, because a key that is accepted but undeclarable is settable in YAML and
// invisible in the UI. Hiding the control would have made the inert key harder to see, not gone.
//
// So the inert key is refused instead — see Trigger.Validate's consultsCache argument — which means
// it can never be silently ignored, and an operator who sets it is told which component does honour
// it. The form still offers the control; its default (`any`) is the no-op, so only a deliberate
// restriction reaches the error.
func TriggerFields(prefix string) []Field {
	p := prefix + "."
	f := []Field{
		{Key: p + "min_request_tokens", Type: FieldInt, Hint: "Fire only when the whole request carries at least this many tokens (0 = no constraint)."},
		{Key: p + "min_messages", Type: FieldInt, Hint: "…and at least this many messages, which is roughly agent steps (0 = no constraint)."},
		{Key: p + "min_output_tokens", Type: FieldInt, Hint: "Per-item floor: only act on a tool output at least this big (0 = use the component's own min_tokens)."},
		{Key: p + "min_request_frac", Type: FieldFloat, Hint: "The request threshold as a fraction of the model's context window, e.g. 0.6. Raises the absolute floor, never lowers it; ignored when the window is unknown."},
		{Key: p + "min_output_frac", Type: FieldFloat, Hint: "The per-item floor as a fraction of the window. Also only ever raises the absolute one."},
		{Key: p + "huge_output_frac", Type: FieldFloat, Hint: "Hard per-item trigger: a single output at least this fraction of the window is acted on regardless of the request-level gate."},
	}
	return append(f,
		Field{Key: p + "cache_state", Type: FieldEnum, Default: CacheStateAny, Options: CacheStates,
			Withdrawn: removedCacheStates,
			Hint:      "Restrict firing by the prompt cache's state: any (no constraint) or pre_expiry (the entry still exists but is about to expire). Only useful to a component whose MODEL CALL reuses the cached prefix and therefore needs it alive — a compactor that builds its own prompt gains nothing from waiting. A component whose default is not `any` documents its own."},
		Field{Key: p + "pre_expiry_seconds", Type: FieldInt, Default: int(DefaultPreExpiry / time.Second),
			Min:  0,
			Hint: "How wide the pre-expiry window is, in seconds. Wider fires more often and invalidates more remaining cache lifetime; narrower fires rarely. Must stay below the shortest prompt-cache lifetime (300s), or every warm request counts as pre-expiry. Unmeasured either way."},
	)
}

// maxPreExpirySeconds is one past the widest pre-expiry window that can mean anything: the SHORTEST
// prompt-cache lifetime this repo derives, which is the 5 minutes a bare `ephemeral` mark buys and
// the tier every captured Claude Code breakpoint uses.
//
// A WINDOW AT OR ABOVE THE TTL SWALLOWS THE WHOLE LIFETIME. `remaining <= preExpiry` is then true for
// every request that has a cache entry at all, so CachePhase returns PreExpiry always, Warm never,
// and a component gated on pre_expiry compacts on EVERY turn — rewriting a live prefix each time,
// which is the most expensive thing this pipeline can do. The form accepted 600 silently. A review
// found it.
const maxPreExpirySeconds = 300

// Validate checks the Trigger's own keys and is called by every component constructor that embeds
// one, so a bad value is refused at config time rather than becoming a silent behaviour change.
//
// IT IS SHARED BECAUSE IT DRIFTED. CacheAllows' docstring promised that "constructors validate the
// string and refuse a bad one at config time", and exactly one of the three constructors that accept
// the key did. `extract` and `extract_llm` took `cache_state: pre_expiryy` without complaint — which
// mattered less than it sounds, because neither consults the key either, but the promise in the
// docstring was load-bearing for the component that DOES.
//
// `component` names the caller so the error says which config block is wrong.
//
// WHAT THIS DELIBERATELY DOES NOT DO: refuse a cache_state on a component that ignores it.
// `extract` and `extract_llm` embed a Trigger and call neither CacheAllows nor CachePhase anywhere,
// so `cache_state: pre_expiry` on either of them is silently inert while the settings form describes it as
// "Restrict firing by the prompt cache's state". A review found that, and both obvious fixes were
// tried here and both break a contract this repo already keeps:
//
//   - Hiding the keys from those components' form fields fails
//     TestEveryComponentDeclaresExactlyItsConfigurableKeys — a key the config struct accepts must be
//     declared, or it is settable in YAML and invisible in the UI.
//   - Refusing the value in the constructor fails
//     TestEveryDeclaredFieldReachesTheDocumentAndNothingElseMoves — a declared field must accept every
//     value it declares.
//
// Both contracts are right, and together they say the real defect is upstream of validation: the key
// should not be in those components' config at all. That wants Trigger split into a size-only
// embedded struct plus the cache keys, which is a config-shape change with its own compatibility
// surface — its own issue, not a rider here. Honouring the key on those components is NOT the answer
// either: they only ever rewrite the uncached tail (Ctx.TailOnly), so they never invalidate the
// cached prefix, which is exactly why they have no economic reason to wait for a cache state.
//
// So this validates what is true for every component that embeds a Trigger: that the enum value
// exists, and that the window is in range.
func (t Trigger) Validate(component string) error {
	// Refused rather than silently read as "any": a typo in the one key that decides WHEN a
	// component fires would otherwise turn the cache gate off and look like it was on.
	//
	// A WITHDRAWN VALUE GETS ITS OWN MESSAGE, because "is not one of [any pre_expiry]" is the least
	// useful thing to tell someone whose config worked yesterday. They did not make a typo — they
	// wrote a value that was documented, defaulted to, and then retired on evidence, so the error
	// says which replacement matches what they were trying to buy.
	if why, withdrawn := removedCacheStates[t.CacheState]; withdrawn {
		return fmt.Errorf("%s: %s", component, why)
	}
	if t.CacheState != "" && !slices.Contains(CacheStates, t.CacheState) {
		return fmt.Errorf("%s: trigger.cache_state %q is not one of %v",
			component, t.CacheState, CacheStates)
	}
	if t.PreExpirySeconds >= maxPreExpirySeconds {
		return fmt.Errorf("%s: trigger.pre_expiry_seconds is %d, which is at or above the shortest "+
			"prompt-cache lifetime (%ds); every warm request would then count as pre-expiry and the "+
			"component would act on every turn",
			component, t.PreExpirySeconds, maxPreExpirySeconds)
	}
	if t.PreExpirySeconds < 0 {
		return fmt.Errorf("%s: trigger.pre_expiry_seconds is %d, which cannot be negative",
			component, t.PreExpirySeconds)
	}
	return nil
}
