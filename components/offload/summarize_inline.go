package offload

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// summarizeInline is the ORIGINAL synchronous path, kept for requests with no session id.
//
// Verbatim apart from its signature: the same call, the same reserve handling, the same checkpoint
// and the same splice. It is a separate method rather than a flag through Offload because the two
// paths differ in what they can promise — this one returns a summary to THIS turn, the async one
// promises a summary to a LATER turn of the same session — and a single function trying to be both
// is how a caller ends up believing the wrong one happened.
//
// The 300-second budget stays here, and it is defensible here in a way it was not on the proxy
// path: a sessionless caller is the library API or /compact, neither of which is holding a prompt
// cache entry that the wait could kill. See summarize_async.go for why the same budget was
// indefensible for proxy traffic.
func (s *Summarize) summarizeInline(c *components.Ctx, rep *components.Report,
	req *bschemas.BifrostChatRequest, msgs []bschemas.ChatMessage, model components.Model,
	span []bschemas.ChatMessage, headCount, start, end int, stale bool) ([]string, error) {
	mode := effectiveMode(c, s.mode)
	var spanJSON []byte
	var key string
	if mode == markerFull {
		var err error
		if spanJSON, err = json.Marshal(span); err != nil {
			return nil, err
		}
		key = hashKey(string(spanJSON))
		if !store.StashRoom(c.Store, len(spanJSON)) {
			return s.refuse(c, rep, req, msgs, headCount, start, stale)
		}
	}
	ctx, cancel := context.WithTimeout(c.Ctx, summarizeCallTimeout)
	defer cancel()
	summary, err := s.summarize(ctx, model, span, conversationGoal(req))
	if err != nil {
		// Classify before returning. Our own ctx is the reliable signal: the parent
		// request may still be healthy while THIS component's budget expired, and the
		// http client wraps the cause, so errors.Is walks to it.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&summarizeTimeouts, 1)
		} else {
			atomic.AddInt64(&summarizeErrors, 1)
		}
		return nil, err // fail-open: the pipeline reverts this component
	}
	if strings.TrimSpace(summary) == "" {
		rep.Skipped = true
		return nil, nil
	}

	// Stash the replaced span so expand can restore it — full mode only. In
	// summary/off there is no restoration; flag the deliberate lossy drop so the
	// pipeline's dropped-without-stash guard permits it.
	// full is reversible only if the store persists the stash; otherwise degrade
	// to an irreversible off-style drop (no unresolvable marker).
	if mode == markerFull {
		// A summary REPLACES the span it covers, so the marker in the summary text is the
		// only route back to it. If the store's rewind reserve cannot hold the span, this
		// component must not summarize at all: unlike the per-message offloaders it cannot
		// leave "this message" verbatim, so refusing means skipping the whole checkpoint.
		//
		// Reachable despite the StashRoom probe above (the probe claims nothing and another
		// session can take the slot in between), which is why the refusal path still exists
		// here — the probe removes the steady-state waste, not the race.
		if !store.PutStash(c.Store, key, spanJSON) {
			return s.refuse(c, rep, req, msgs, headCount, start, stale)
		}
	} else {
		rep.Irreversible = true
	}

	summaryText := summaryWrapper(summary, key, mode)
	// USER, not system. The summary is injected context, and Anthropic will not accept a
	// system-role message in the middle of `messages`: system content belongs in the
	// top-level `system` field, and a system role inside the array must precede an
	// assistant message or end it. This component emits [msgs[0], summary, tail...], so
	// when msgs[0] is itself the system prompt — the normal case — a system-role summary
	// lands at index 1 and the provider rejects the whole request:
	//
	//	400 messages.1: role 'system' must precede an 'assistant' message or end the array
	//
	// Measured on live LOCA-bench traffic: every task that triggered a summarization failed
	// this way, including in an arm with NO other component enabled, so it is this
	// component's own output and not a pipeline interaction. It went unnoticed because
	// every prior measurement replayed through /compact, which never forwards upstream and
	// therefore never has the body validated by a provider.
	//
	// A user-role message carrying the summary is both valid and conventional — it is what
	// Claude Code's own compaction does.
	summaryMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&summaryMsg, summaryText)

	// Checkpoint: this summary subsumes the leading span (len(span) messages from
	// index 1). A later turn appends messages, so this same prefix stays stable.
	saveCheckpoint(c, sumCheckpoint{
		SummaryMsg: summaryText, CoveredCount: end - start,
		CoveredHash: spanHash(span), Key: key,
	})

	// The fresh path's own event, filed HERE rather than at the top of the path: everything
	// above this line can still decline (a blank summary, a refused stash, an empty span after
	// the expand trim), and an event filed before those would name a summary that was never
	// emitted. This is the first point at which a new summary is committed.
	//
	// An Event, not a Replay: this turn spent money. Report.Replay is for the free paths, and
	// keeping the two apart is what lets the episode walk below tell a paid t0 from the
	// amortization that follows it.
	rep.Event(EventFreshSummary)

	// [msg0, summary, last-K] — reassign; apply.Body rebuilds losslessly.
	out := make([]bschemas.ChatMessage, 0, 2+s.keepLast)
	out = append(out, msgs[:headCount]...)
	out = append(out, summaryMsg)
	out = append(out, msgs[end:]...)
	// Removing a span can orphan the tail's leading tool_result blocks; a provider rejects
	// the whole request if it does. See dropOrphanedToolResults.
	if repaired, n := dropOrphanedToolResults(out); n > 0 {
		out = repaired
	}
	req.Input = out
	if key != "" {
		return []string{key}, nil
	}
	return nil, nil
}
