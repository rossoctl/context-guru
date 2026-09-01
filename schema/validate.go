package schema

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// ShapeViolation is one broken invariant, phrased to point at the message that broke it.
type ShapeViolation struct {
	Index int    // message index, or -1 when the violation is about the list as a whole
	Rule  string // short, stable identifier for the invariant
	Msg   string // human-readable, mirroring the provider's own wording where possible
}

func (v ShapeViolation) String() string {
	if v.Index < 0 {
		return fmt.Sprintf("[%s] %s", v.Rule, v.Msg)
	}
	return fmt.Sprintf("messages.%d [%s] %s", v.Index, v.Rule, v.Msg)
}

// Shape rule identifiers, so a test can assert on the specific invariant it exists for
// rather than on wording.
const (
	RuleSystemPosition   = "system-position"
	RuleAnsweredToolUse  = "answered-tool-use"
	RulePairedToolResult = "paired-tool-result"
	RuleNonEmptyContent  = "non-empty-content"
)

// ValidateShape reports every message-shape invariant the list breaks, judged as an
// ANTHROPIC request. It is the Anthropic-dialect shorthand for ValidateShapeFor; see there
// for the invariants and for why the dialect matters.
func ValidateShape(msgs []schemas.ChatMessage) []ShapeViolation {
	return ValidateShapeFor(schemas.Anthropic, msgs)
}

// ValidateShapeFor reports every message-shape invariant the list breaks when sent to
// `provider`. An empty result means the list is well-formed in the ways that provider
// enforces structurally.
//
// THE PROVIDER ARGUMENT IS NOT DECORATION. Two of the four invariants are properties of the
// Anthropic-style tool protocol and hold for every provider that speaks it; the other two are
// Anthropic's own rules and are simply FALSE elsewhere. OpenAI imposes no positional
// constraint on system/developer messages, so checking system-position against an OpenAI
// transcript reports a violation on every turn where the client re-injects a system message —
// ordinary traffic, and `/compact` defaults to OpenAI (proxy/proxy.go:566). An ungated rule
// would therefore be a savings regression the moment this is used on the request path, which
// is why the gate is a prerequisite for that wiring rather than a nicety.
//
// The invariants, and why each one exists:
//
//  1. system-position — ANTHROPIC ONLY. A system-role message away from index 0 must be
//     immediately followed by an assistant message, or end the array. This is the provider's
//     own wording, and it is the defect that made `summarize` unusable on every call: it
//     emitted [msgs[0], summary(system), tail...], so with the usual system prompt at
//     index 0 a second system role landed at index 1 in front of the kept tail.
//  2. answered-tool-use — ALL PROVIDERS. Every `tool_use` must be answered by a
//     `tool_result` in the messages immediately following it. Removing a span can delete the
//     answer while keeping the call: preserving msgs[0] when it is an assistant tool-call
//     message does exactly that.
//  3. paired-tool-result — ALL PROVIDERS. Every `tool_result` must answer a `tool_use` that
//     appeared earlier. Removing a span can delete the call while keeping the answer. The
//     mirror of (2), and the reason both are checked: e7d1aa8 showed they are one mistake
//     seen from either side, and fixing one alone leaves the other live — which is what
//     happened.
//  4. non-empty-content — ANTHROPIC ONLY, on the evidence available. A message that CARRIES
//     content whose text is blank is a hard 400 ("text content blocks must be non-empty"),
//     and this pipeline has ~20 places that can produce one: every component that rewrites a
//     message does it through SetMessageText, and any of them reducing a message to the empty
//     string writes `""` straight onto the wire. `summarize` itself is NOT one of them — it
//     refuses a blank summary (components/offload/summarize.go:201), confirmed by mutation:
//     making its model return "" makes it decline rather than emit empty content. So this rule
//     guards the OTHER rewriters (cmdfilter, dedup, skeleton, textclean and the rest), which
//     have no such guard. Gated to Anthropic because an Anthropic rejection is what is on
//     record; widen it when another provider's 400 is.
//
// The rule is deliberately narrow in three ways, to preserve the property that makes this
// validator usable at all — content-shape false positives being impossible rather than
// merely absent. It fires only when Content is non-nil (a pure tool-call assistant message
// has nil content and is legal), only when the message carries no non-text block (Rewritable,
// so an image-only, thinking-only or tool_result-payload message is never judged by its text),
// and never on a role=tool message or an assistant message carrying ToolCalls, whose meaning
// lives in fields other than their text.
func ValidateShapeFor(provider schemas.ModelProvider, msgs []schemas.ChatMessage) []ShapeViolation {
	var out []ShapeViolation
	seenCall := map[string]bool{}

	for i := range msgs {
		m := msgs[i]

		if provider == schemas.Anthropic &&
			m.Role == schemas.ChatMessageRoleSystem && i != 0 && !systemPositionOK(msgs, i) {
			out = append(out, ShapeViolation{Index: i, Rule: RuleSystemPosition,
				Msg: "role 'system' must precede an 'assistant' message or end the array"})
		}

		if provider == schemas.Anthropic && contentPresentButBlank(m) {
			out = append(out, ShapeViolation{Index: i, Rule: RuleNonEmptyContent,
				Msg: "text content blocks must be non-empty"})
		}

		// A result must answer a call seen earlier.
		if m.Role == schemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil && *m.ChatToolMessage.ToolCallID != "" {
			if id := *m.ChatToolMessage.ToolCallID; !seenCall[id] {
				out = append(out, ShapeViolation{Index: i, Rule: RulePairedToolResult,
					Msg: fmt.Sprintf("unexpected `tool_use_id` found in `tool_result` blocks: %s", id)})
			}
		}

		if m.ChatAssistantMessage == nil || len(m.ChatAssistantMessage.ToolCalls) == 0 {
			continue
		}
		var ids []string
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			if tc.ID != nil && *tc.ID != "" {
				seenCall[*tc.ID] = true
				ids = append(ids, *tc.ID)
			}
		}
		answered := answeredInRun(msgs, i+1)
		for _, id := range ids {
			if !answered[id] {
				out = append(out, ShapeViolation{Index: i, Rule: RuleAnsweredToolUse,
					Msg: fmt.Sprintf("`tool_use` ids were found without `tool_result` blocks "+
						"immediately after: %s", id)})
			}
		}
	}
	return out
}

// contentPresentButBlank reports whether m has content that a provider will read as empty.
//
// The guards are the whole point (see ValidateShapeFor rule 4): a message with a non-text
// block is never judged by MessageText, because MessageText cannot see an image, a thinking
// block or an Anthropic tool_result payload and would call every one of them blank. Restricting
// this to Rewritable messages keeps the "content-shape false positives are impossible" property
// intact — this rule cannot fire on any shape it does not fully understand.
func contentPresentButBlank(m schemas.ChatMessage) bool {
	if m.Content == nil || m.Role == schemas.ChatMessageRoleTool {
		return false
	}
	if a := m.ChatAssistantMessage; a != nil && len(a.ToolCalls) > 0 {
		return false
	}
	return Rewritable(m) && strings.TrimSpace(MessageText(m)) == ""
}

// systemPositionOK applies the provider's rule for a system role inside `messages`, which is
// narrower than "index 0 only" in one direction and wider in another.
//
// "Index 0 only" is what the first version of this check said, and it is WRONG for this
// codebase: the Claude Agent SDK appends a fresh system-role message inside `messages` on
// every turn (its `<system-reminder><total_tokens>N tokens left</total_tokens>` budget
// reminder), and that traffic is ACCEPTED by the provider. apply's captured Agent-SDK
// fixture carries system roles at indices 1, 4 and 7 of a five- and eight-message
// transcript — see schema.SessionHead, which exists because of those same messages. A
// validator that rejected them would fire on ordinary live traffic and be worthless
// precisely where it is needed, the same trap as checking only msgs[i+1] for a parallel
// call's results.
//
// So the rule is the provider's literal one: such a message must be followed by an assistant
// turn (the SDK's reminder always is, or it ends the array). The summarize defect fails it
// because the summary was spliced in FRONT of the kept tail, which begins with whatever the
// conversation was doing — a user turn or a tool exchange, not an assistant reply.
func systemPositionOK(msgs []schemas.ChatMessage, i int) bool {
	if i == len(msgs)-1 {
		return true // ends the array
	}
	return msgs[i+1].Role == schemas.ChatMessageRoleAssistant
}

// answeredInRun collects the tool_result ids in the contiguous run of tool messages starting
// at `from`. Any other role ends the run.
//
// PARALLEL CALLS are why this scans a run rather than one message. One assistant message may
// carry several tool_use blocks, and Anthropic requires every result in the SINGLE user
// message that follows. bifrost's schema represents each result as its OWN role=tool message,
// so the wire's one user message maps to a RUN of consecutive tool messages here (see
// apply.normalize). Inspecting only msgs[i+1] therefore reported a violation on every
// ordinary parallel exchange — the second call always looked unanswered:
//
//	messages.1 [answered-tool-use] ... without `tool_result` blocks immediately after: call_b
//
// which is indistinguishable from the real defect being hunted. Scanning the whole run is the
// representation-correct reading of the same invariant: the run must still be CONTIGUOUS, so
// anything else in between (a summary, a user turn) still fails.
func answeredInRun(msgs []schemas.ChatMessage, from int) map[string]bool {
	answered := map[string]bool{}
	for j := from; j < len(msgs); j++ {
		if msgs[j].Role != schemas.ChatMessageRoleTool {
			break
		}
		if t := msgs[j].ChatToolMessage; t != nil && t.ToolCallID != nil {
			answered[*t.ToolCallID] = true
		}
	}
	return answered
}

// FormatShapeViolations renders violations one per line for a test failure message, with the
// list's roles appended — the roles are what makes a shape failure diagnosable.
func FormatShapeViolations(vs []ShapeViolation, msgs []schemas.ChatMessage) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.String())
		b.WriteString("\n")
	}
	b.WriteString("roles: ")
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d:%s", i, m.Role)
	}
	return b.String()
}
