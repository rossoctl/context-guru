package tokens

import "strings"

// BilledDeltaFactor is the measured ratio of the PROVIDER's token count to this package's
// count, over content a compaction component REMOVED. It is the correction `saved_usd`
// needs, and issue #240 is the report that it was missing.
//
// # Why a factor on a difference, and not on a level
//
// Count is o200k_base — an OpenAI BPE — applied to every model (see tokens.go). On Claude
// traffic it under-counts. But the obvious instrument for "how much", the dashboard's own
// EstimatorDivergence, is a ratio of LEVELS (billed prompt ÷ tokens_before) and is 3-4x,
// because a level carries the system prompt, the tool declarations and the JSON envelope
// that MessagesTokens never counts. None of that is what a component removed. In a
// DIFFERENCE of two counts of the same request, all of it cancels — which is why this is a
// factor on `tokens_before − tokens_after` and the 3.38x divergence is not a substitute.
//
// # How these numbers were measured
//
// apply.TestBilledPairColdReplay, 2026-09-15, against the IBM LiteLLM gateway. For each
// real captured Claude Code turn: run the production pipeline, then send the before-body
// and the after-body as UNCACHED requests and read the provider's own `usage` off each
// reply. Every cache_control mark is stripped so both arms bill their whole prompt as fresh
// input, making each reply a statement of the provider's count of that exact body.
//
//	f = (billed_before − billed_after) / (ours_before − ours_after)
//
//	model              n    pooled f   median   range
//	claude-haiku-4-5   11   1.3686     1.5030   1.168 - 1.517
//	claude-sonnet-5    11   1.6857     1.8197   1.519 - 1.897
//	claude-opus-5      11   1.6857     1.8197   1.519 - 1.897
//
// Sonnet 5 and Opus 5 returned BYTE-IDENTICAL billed deltas on every pair, so they share a
// tokenizer and haiku-4-5 does not. That is why this is keyed per family and not set from
// one global constant — the same lesson the cache-write premium taught in modelinfo.
//
// # What NOT to claim from these numbers
//
// The 11 pairs are successive turns of TWO session lineages, not 11 independent removals:
// pairs 5-8 have identical deltas. So the effective n is a handful, the spread within a
// lineage is small and the spread BETWEEN lineages is most of the range. Treat these as
// "the direction is established and the magnitude is roughly this", not as a tight estimate.
// The direction is not in doubt — every one of 33 measured pairs across three models came
// back above 1.0, and the kill criterion (a 90% interval spanning 1.0) is not close.
//
// Issue #240 asserted 1.1416 from a single turn, and a cross-turn one at that. Every model
// measured here is ABOVE that, so the issue understated its own finding.
//
// # Why it is applied to dollars and not to the token counts
//
// Only Event.Price uses this. tokens_before / tokens_after / saved_gross / saved_unique stay
// exactly as they were, because component gates and triggers are evaluated against them: a
// tokenizer change would alter WHICH content gets compacted, which is a behaviour change
// wearing an accounting change's clothes. The published token figures remain this repo's own
// estimate, as they always were, and only the counterfactual DOLLARS are corrected.
//
// measured reports whether this model's family was actually measured. false returns 1.0 —
// an unmeasured family gets no correction rather than another family's constant.
func BilledDeltaFactor(model string) (factor float64, measured bool) {
	m := strings.ToLower(model)
	switch {
	// Order matters: haiku before the generic claude fallthrough.
	case strings.Contains(m, "haiku"):
		return 1.3686, true
	case strings.Contains(m, "sonnet"), strings.Contains(m, "opus"):
		return 1.6857, true
	}
	return 1.0, false
}
