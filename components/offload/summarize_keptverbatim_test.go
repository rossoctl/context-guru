package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// asstWithCall is an assistant turn that calls a tool, so a span boundary landing after it would
// leave the call unanswered — one of the two provider rejections summarizeSpan exists to prevent.
func asstWithCall(id string) bschemas.ChatMessage {
	return bschemas.ChatMessage{
		Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{
			ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id}},
		},
	}
}

// SUMMARIZE MUST NOT SUMMARIZE AWAY CONTENT THE AGENT EXPANDED.
//
// It is the one offloader that consults neither skipReduce nor isKeptVerbatim, and it replaces
// msgs[start:end] wholesale rather than editing in place. Two consequences, one pre-existing and one
// introduced by the repair now writing a pointer instead of a second copy:
//
//   - the bounce loop cg:keep: exists to prevent: the agent asks for content back, gets it, and next
//     turn it is gone again;
//   - expand.RestoredInPlace's pointer says the content is in the transcript, on the strength of the
//     body BEFORE the pipeline runs. If summarize removes it, the model is left with a pointer to
//     nothing where it previously got the content.
func TestSummarizeLeavesExpandedContentInTheTranscript(t *testing.T) {
	expanded := strings.Repeat("the content the agent asked to have back\n", 40)
	filler := strings.Repeat("older chatter that is fine to summarize\n", 40)

	st := store.NewMemory(store.Options{})
	MarkKeptVerbatim(st, expanded)

	// msg0, then a summarizable stretch, then the expanded output inside its own exchange, then a
	// tail. keep_last is 1 so `end` starts well past the expanded message.
	msgs := []bschemas.ChatMessage{
		userMsg("system-ish opener"),
		userMsg(filler),
		asstWithCall("call_1"),
		tool(expanded),
		userMsg("what next?"),
	}

	_, start, end := summarizeSpan(msgs, 1)
	trimmed := trimSpanForKeptVerbatim(msgs, start, end,
		func(text string) bool { return text == expanded })

	if trimmed >= 3 {
		t.Fatalf("span still ends at %d, so the expanded tool output at index 3 is inside it and "+
			"would be summarized away", trimmed)
	}
	// AND the boundary must not split the exchange: index 2 is the assistant turn that called the
	// tool, so a span ending at 3 would leave its call unanswered and the request would be rejected
	// (400 `tool_use` ids were found without `tool_result` blocks immediately after).
	if trimmed > 2 {
		t.Fatalf("span ends at %d, which leaves the tool_use at index 2 inside the span with its "+
			"result outside: the provider rejects that request", trimmed)
	}
	for _, m := range msgs[trimmed:] {
		if schema.MessageText(m) == expanded {
			return // the expanded content survives outside the span, which is the point
		}
	}
	t.Fatal("the expanded content is not in the kept part of the transcript")
}

// The safe direction when there is nothing left to summarize: decline. summarize already supports
// end <= start, so this must not become a special case — and it must certainly not summarize the
// expanded content because the remaining span is small.
func TestSummarizeDeclinesRatherThanTouchingExpandedContent(t *testing.T) {
	expanded := strings.Repeat("the content the agent asked to have back\n", 40)
	msgs := []bschemas.ChatMessage{
		userMsg("opener"),
		tool(expanded), // immediately after the head, so nothing is summarizable without it
		userMsg("what next?"),
	}
	_, start, end := summarizeSpan(msgs, 1)
	trimmed := trimSpanForKeptVerbatim(msgs, start, end,
		func(text string) bool { return text == expanded })
	if trimmed > start {
		t.Fatalf("span is [%d,%d) and still contains the expanded message at index 1", start, trimmed)
	}
}

// A span with no expanded content must be left exactly as summarizeSpan computed it — the trim is a
// protection, not a new boundary policy, and shrinking a healthy span would cost real savings.
func TestTheTrimIsANoOpWithoutExpandedContent(t *testing.T) {
	filler := strings.Repeat("older chatter that is fine to summarize\n", 40)
	msgs := []bschemas.ChatMessage{
		userMsg("opener"), userMsg(filler), userMsg(filler), userMsg("what next?"),
	}
	_, start, end := summarizeSpan(msgs, 1)
	if got := trimSpanForKeptVerbatim(msgs, start, end, func(string) bool { return false }); got != end {
		t.Fatalf("trimmed a span with nothing expanded in it: %d -> %d", end, got)
	}
}
