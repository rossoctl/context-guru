package offload

import (
	"log/slog"
	"sort"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
)

// SHADOW MODE (#404, point 7).
//
// With `shadow: true` the sweep asks at the keep-alive ping, holds the answer, and then does what the
// cold apply will do EXCEPT change the request: on the first cold request after the question it
// rechecks the held answer and reports what it would have dropped. Osher runs it in production to
// measure, before anything is applied:
//
//   - how many candidates were asked about and how many drops the model proposed,
//   - how many of those drops were still valid at the next cold request,
//   - why the others were not (the recheck's reasons).
//
// Two moments are reported, because they happen on two different requests:
//
//	the FIRST request after the ping carries the ask (warm or cold)         -> sweep_shadow_asked, ...
//	the first COLD request after the ping carries the recheck               -> sweep_shadow_valid, ...
//
// Everything goes out three ways: Report events (they land in request_components.events, which is the
// dashboard DB), a cg.sweep.shadow log line, and the held record itself. Nothing here writes to req.
//
// EVENT NAMES (all counts; the *_tokens ones count tokens, not outputs):
//
//	sweep_shadow_asked              candidates the question carried
//	sweep_shadow_proposed           drops the model voted for
//	sweep_shadow_proposed_tokens    tokens those drops would remove
//	sweep_shadow_wait_<cause>       one per ask: what the thread was waiting on (#453), e.g.
//	                                sweep_shadow_wait_human. Recorded, never used to decide.
//	sweep_shadow_valid              proposed drops still valid at the cold request
//	sweep_shadow_valid_tokens       tokens those would remove
//	sweep_shadow_dropped_<reason>   proposed drops the recheck turned down, by reason
//	                                (conversation_shorter, output_gone, content_changed,
//	                                 referenced_later, expanded)
const (
	shadowAsked          = "sweep_shadow_asked"
	shadowProposed       = "sweep_shadow_proposed"
	shadowProposedTokens = "sweep_shadow_proposed_tokens"
	shadowWaitPrefix     = "sweep_shadow_wait_"
	shadowValid          = "sweep_shadow_valid"
	shadowValidTokens    = "sweep_shadow_valid_tokens"
	shadowDroppedPrefix  = "sweep_shadow_dropped_"
)

// shadowStep runs at the top of every request on this component. It is a no-op unless shadow mode is
// on and the thread has a held answer.
//
// A WARM request reports the ask (once) and leaves the answer held. A COLD request reports the ask if
// it has not been reported, runs the recheck, reports it, and consumes the answer.
func (e *ExtractSweep) shadowStep(c *components.Ctx, req *bschemas.BifrostChatRequest, rep *components.Report) {
	if !e.shadow || c == nil || c.Store == nil || c.Session == "" {
		return
	}
	key := thread.Key(c.Session, c.Thread)
	h, ok := getHeld(c.Store, key)
	if !ok {
		return
	}
	// Reported ONCE per held answer even when two requests of the thread arrive together: the claim is
	// atomic, so only the caller that wins it emits the ask's events and log line.
	if claimed, won := claimHeldReport(c.Store, key); won {
		h = claimed
		n, tokens := h.proposed()
		rep.EventN(shadowAsked, len(h.Outputs))
		rep.EventN(shadowProposed, n)
		rep.EventN(shadowProposedTokens, tokens)
		if h.WaitingFor != "" {
			rep.Event(shadowWaitPrefix + trimReason(h.WaitingFor))
		}
		slog.Info("cg.sweep.shadow", "phase", "ask", "session", c.Session, "thread", c.Thread,
			"asked", len(h.Outputs), "proposed", n, "proposed_tokens", tokens,
			"waiting_for", h.WaitingFor, "wait_tool", h.WaitTool, "cost_usd", h.CostUSD,
			"model", h.Model, "judged_msgs", h.Judged)
	}
	if !sweepColdRequest(c) {
		return
	}
	// The recheck is reported once per held answer: the request that wins the claim consumes it, and a
	// second cold request of the thread finds nothing left to report.
	h, won := claimHeldSpend(c.Store, key)
	if !won {
		return
	}
	res := recheckHeld(c, req, h, e.minTokens)
	rep.EventN(shadowValid, len(res.Valid))
	rep.EventN(shadowValidTokens, res.validTokens())
	reasons := res.reasonCounts()
	names := make([]string, 0, len(reasons))
	for r := range reasons {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		rep.EventN(shadowDroppedPrefix+r, reasons[r])
	}
	n, tokens := h.proposed()
	slog.Info("cg.sweep.shadow", "phase", "cold_recheck", "session", c.Session, "thread", c.Thread,
		"proposed", n, "proposed_tokens", tokens,
		"valid", len(res.Valid), "valid_tokens", res.validTokens(), "dropped_reasons", reasons,
		"waiting_for", h.WaitingFor, "wait_tool", h.WaitTool)
	// Consumed above, by the claim: a cold request is what the answer was held for. The next ping asks
	// afresh.
}
