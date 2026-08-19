package components

import (
	"math"

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
	}
}
