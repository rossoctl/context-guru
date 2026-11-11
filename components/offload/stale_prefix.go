package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
)

// rawSpanForHash returns the first n messages in the SAME coordinates the host's prefix stash
// was built from (components.Ctx.RawInput), for a stale-prefix guard that hashes a span and
// compares it against proxy's sentStash (components.PrefixCoverage.CoversSpan). It is the one
// place both extract_llm_sweep and cache_aware_summarizer compute that span, so the two guards
// cannot drift the way they did in PR #431 round 1 — the sweep hashed req.Input at its OWN
// pipeline position while CoversSpan compared against the stashed PRE-pipeline body, so any
// component running before it (textclean, format, …) made the two hashes disagree on every turn
// it actually rewrote something, even though the stash held exactly the right bytes.
//
// RawInput nil (a Ctx built directly, as most component unit tests still do, rather than through
// apply.BodyOpts) falls back to req.Input, which is the pre-fix behavior and is correct for every
// caller that never puts a component ahead of the guarded one — i.e. every existing test. A
// present RawInput whose length disagrees with req.Input's at the guarded component's own
// position means an EARLIER component changed the message count (a count-changing summarize or
// extract path): the two slices' indices then name different messages, so no span taken from
// either one is safe to compare against the raw stash, and ok reports false rather than hand back
// a wrong one.
func rawSpanForHash(c *components.Ctx, req *bschemas.BifrostChatRequest, n int) ([]bschemas.ChatMessage, bool) {
	if n < 0 {
		return nil, false
	}
	raw := req.Input
	if c.RawInput != nil {
		if len(req.Input) != len(c.RawInput) {
			return nil, false
		}
		raw = c.RawInput
	}
	if n > len(raw) {
		return nil, false
	}
	return raw[:n], true
}
