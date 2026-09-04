package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
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
//
// THIS TEST AND THE TWO BELOW EXERCISE THE BOUNDARY ARITHMETIC IN ISOLATION, with a stub predicate
// and no store — deliberately, because the retreat rule is worth pinning on its own. They do NOT
// establish that Offload calls it: see TestSummarizeOffloadKeepsExpandedContent, which is the test
// that fails if the call goes away.
func TestSummarizeLeavesExpandedContentInTheTranscript(t *testing.T) {
	expanded := strings.Repeat("the content the agent asked to have back\n", 40)
	filler := strings.Repeat("older chatter that is fine to summarize\n", 40)

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

// AND THE ARITHMETIC MUST ACTUALLY BE REACHED, which the three tests above do not establish.
//
// They call trimSpanForKeptVerbatim directly with a stub predicate, so deleting the call from
// Offload — or passing a predicate that never consults the store, or moving the call below the point
// end is used — leaves all three green and the whole suite green, and the regression is back. The
// tell was in the fixture: the first test built a store and marked content, then never used it,
// because the stub had replaced it. Dead setup is what an assertion that stopped reaching its
// subject looks like.
//
// This is the same shape as the vacuous cross-check on #204, and its sibling in this package
// (TestTheFlipProbeDoesNotRenewTheDecisionItAsksAbout) is the version that gets it right: the
// assertion has to live where the behaviour is.
func TestSummarizeOffloadKeepsExpandedContent(t *testing.T) {
	// Derived from bulkResult rather than hand-copied from its format string. The precondition
	// below would fail loudly if the two drifted apart, so a copy was safe — but it was safe
	// because of a guard elsewhere rather than by construction, and the next person editing
	// bulkResult has no reason to look here.
	expanded := schema.MessageText(bulkResult("t2"))
	build := func() []bschemas.ChatMessage {
		msgs := []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleUser},
			callMsg("t1"), bulkResult("t1"),
			callMsg("t2"), bulkResult("t2"),
			callMsg("t3"), bulkResult("t3"),
		}
		schema.SetMessageText(&msgs[0], "fix the failing tests")
		return msgs
	}
	present := func(msgs []bschemas.ChatMessage) bool {
		for _, m := range msgs {
			if schema.MessageText(m) == expanded {
				return true
			}
		}
		return false
	}

	// PRECONDITION: with nothing marked, this fixture DOES summarize that message away. Without
	// this, "the content survived" would also pass on a fixture summarize never touched.
	s := newSummarizeKeepLast(t, 1)
	plain := &bschemas.BifrostChatRequest{Input: build()}
	if !present(plain.Input) {
		t.Fatal("the fixture does not contain the content to begin with")
	}
	var rep components.Report
	if _, err := s.Offload(plain, &rep, ctxFor(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if present(plain.Input) {
		t.Fatal("summarize did not remove this message even unmarked, so the assertion below " +
			"would pass vacuously — the fixture is not exercising the span")
	}

	// THE PROPERTY: marked kept-verbatim, the same fixture must keep it.
	st := store.NewMemory(store.Options{})
	MarkKeptVerbatim(st, expanded)
	req := &bschemas.BifrostChatRequest{Input: build()}
	rep = components.Report{}
	if _, err := s.Offload(req, &rep, ctxFor(st)); err != nil {
		t.Fatal(err)
	}
	if !present(req.Input) {
		t.Fatalf("summarize removed content the agent had expanded: the model is left with a "+
			"pointer to nothing where it used to get the content, and the agent will expand it "+
			"again next turn. gates=%v", rep.Gates)
	}
}
