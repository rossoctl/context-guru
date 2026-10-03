package record

// Emitting what the proxy RECONSTRUCTS, as distinct from what it OBSERVES.
//
// Two things in this file, both of them the "with further work" column of the comparison we
// showed the eval team, now implemented:
//
//  1. DerivedTurnSpan -- a span per recovered human turn, so a consumer that groups by turn
//     has something to group by. It is NOT named claude_code.interaction. That name means
//     "measured on the developer's machine", and a reconstruction wearing it would let a
//     derived sequence number be read as an observed one. It is cg.turn.derived, it carries
//     cg.turn.derived=true, and it omits any duration field rather than inventing one.
//
//  2. ToolCallAttrs -- lifting tool name and arguments out of the message body into span
//     attributes, so queries written against claude-record's tool_name / full_command keep
//     working. The information was always present in the body; this only changes where it
//     sits.
//
// What is deliberately NOT here: a success boolean, and an execution duration. Failure appears
// in a tool result as prose, not as a flag, so inferring it would produce a column that looks
// like claude-record's `success` and is not. Execution duration happens on a machine the proxy
// is not on. Both stay absent; the comparison marks them so.

import (
	"strings"
	"time"
)

// Derived-data attribute keys. All cg.* -- additive, and namespaced so nothing mistakes them
// for either standard gen_ai semconv or claude-record's observed claude_code.* keys.
const (
	// SpanDerivedTurn is the span name for a reconstructed human turn.
	SpanDerivedTurn = "cg.turn.derived"

	AttrTurnDerived   = "cg.turn.derived"  // always true on these spans
	AttrTurnSequence  = "cg.turn.sequence" // DERIVED ordering, 1-based within a root
	AttrTurnPrompt    = "cg.turn.prompt"   // the recovered human text
	AttrTurnPromptLen = "cg.turn.prompt_length"
	AttrTurnRoot      = "cg.turn.conversation_root"
	AttrTurnMethod    = "cg.turn.method" // how it was recovered, for auditability

	// AttrTurnPrior is true on a turn that was already history in the first call observed for
	// its conversation: a resumed session's earlier turns, or turns from before recording began.
	// Count a session's own turns as those where this is false. See ReconTurn.Prior.
	AttrTurnPrior = "cg.turn.prior_to_observation"

	// TurnMethodBodyRecon names the only method there is today. Spelled out in the data so a
	// consumer can tell a reconstruction from an observation without reading this file.
	TurnMethodBodyRecon = "request_body_reconstruction"

	// Tool-call attributes lifted from the body. Same spelling as claude-record's, because
	// the point is that existing queries keep working.
	AttrToolName   = "tool_name"
	AttrToolArgs   = "full_command"
	AttrToolCallID = "gen_ai.tool.call.id"
	AttrToolLifted = "cg.tool.from_body" // true: read out of the message body, not observed
)

// DerivedTurnSpan renders one reconstructed turn as a span.
//
// start is the start time of the model call whose body first carried the turn -- the earliest
// moment the proxy can prove the turn existed by. It is NOT when the human typed, and the span
// carries no end time for exactly that reason: a zero-length span is honest about knowing one
// instant, where a fabricated duration would not be.
func DerivedTurnSpan(sessionID, spanID, parentID string, t ReconTurn, start time.Time) Span {
	at := map[string]any{
		AttrTurnDerived:   true,
		AttrTurnSequence:  t.Sequence,
		AttrTurnPrompt:    t.Text,
		AttrTurnPromptLen: len(t.Text),
		AttrTurnMethod:    TurnMethodBodyRecon,
		AttrTurnPrior:     t.Prior,
		AttrRecordedBy:    CollectorVersion,
	}
	if t.Root != "" {
		at[AttrTurnRoot] = t.Root
	}
	ts := FormatTime(start)
	return Span{
		TraceID:      sessionID,
		SpanID:       spanID,
		ParentSpanID: parentID,
		Name:         SpanDerivedTurn,
		Kind:         SpanKindClient,
		StartTime:    ts,
		EndTime:      ts, // no duration is knowable from this side; see the comment above
		Attributes:   at,
		ResourceAttr: map[string]any{"service.name": "claude-code"},
		Status:       Status{Code: StatusOK},
	}
}

// ToolUse is one tool call lifted out of an assistant message body.
type ToolUse struct {
	CallID    string
	Name      string
	Arguments string // JSON, as a string -- same shape as the body carries it
}

// ToolCalls extracts every tool call from a translated output-message list.
//
// The join key (gen_ai.tool.call.id, the toolu_bdrk_... value) is what makes this useful: the
// same id appears on claude-record's tool spans and in every later request body, so a
// reconstruction and an observation can be matched call-for-call.
func ToolCalls(msgs []Message) []ToolUse {
	var out []ToolUse
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != PartToolCall {
				continue
			}
			out = append(out, ToolUse{CallID: p.ID, Name: p.Name, Arguments: p.Arguments})
		}
	}
	return out
}

// ToolResults maps call id -> result text, read from role:tool messages.
func ToolResults(msgs []Message) map[string]string {
	out := make(map[string]string)
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type == PartToolResponse && p.ID != "" {
				out[p.ID] = p.Response
			}
		}
	}
	return out
}

// ToolCallAttrs renders one lifted tool call as span attributes, using claude-record's own key
// names so existing queries resolve.
//
// cg.tool.from_body marks the provenance: these values were read out of a message body rather
// than observed at execution time. Present so that a mixed corpus stays interpretable.
func ToolCallAttrs(u ToolUse) map[string]any {
	at := map[string]any{
		AttrToolLifted: true,
		AttrRecordedBy: CollectorVersion,
	}
	if u.Name != "" {
		at[AttrToolName] = u.Name
	}
	if u.CallID != "" {
		at[AttrToolCallID] = u.CallID
	}
	if a := strings.TrimSpace(u.Arguments); a != "" && a != "{}" && a != "null" {
		at[AttrToolArgs] = a
	}
	return at
}
