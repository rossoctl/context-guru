package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// #723 (a). The fresh span ends before the LAST tool exchange: the assistant turn that asked for the
// tool and its results stay verbatim. No tool result at or after start means no change.
func TestRetreatPastLastToolExchange(t *testing.T) {
	tool := func(id string) bschemas.ChatMessage {
		m := caMsg(bschemas.ChatMessageRoleTool, "out "+id)
		m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
		return m
	}
	u := func(s string) bschemas.ChatMessage { return caMsg(bschemas.ChatMessageRoleUser, s) }
	a := func(s string) bschemas.ChatMessage { return caMsg(bschemas.ChatMessageRoleAssistant, s) }
	// 0 u, 1 a(call), 2 tool, 3 a, 4 u, 5 a(call), 6 tool, 7 a, 8 u, 9 a, 10 u
	msgs := []bschemas.ChatMessage{u("task"), a("call 1"), tool("t1"), a("read a"), u("read b"),
		a("call 2"), tool("t2"), a("read b"), u("one word"), a("ten"), u("again")}
	if got := retreatPastLastToolExchange(msgs, nil, 1, 9); got != 5 {
		t.Fatalf("end = %d, want 5 (the assistant turn that called the last tool)", got)
	}
	if got := lastKeptToolResultID(msgs, 5); got != "t2" {
		t.Fatalf("anchor = %q, want t2", got)
	}
	// The last exchange is already in the kept tail: nothing to move.
	if got := retreatPastLastToolExchange(msgs, nil, 1, 4); got != 4 {
		t.Fatalf("end = %d, want 4 unchanged", got)
	}
	// No tool result after start.
	plain := []bschemas.ChatMessage{u("task"), a("x"), u("y"), a("z"), u("w")}
	if got := retreatPastLastToolExchange(plain, nil, 1, 3); got != 3 {
		t.Fatalf("end = %d, want 3 unchanged with no tool exchange", got)
	}
	// The only exchange starts at the head: nothing would be left to summarize.
	head := []bschemas.ChatMessage{u("task"), a("call"), tool("t"), a("done"), u("go")}
	if got := retreatPastLastToolExchange(head, nil, 1, 3); got != 3 {
		t.Fatalf("end = %d, want 3 unchanged when the exchange starts at the span start", got)
	}
}

// #730. Where the host cannot write a breakpoint (OpenAI), the summary covers the last tool
// exchange too. Where it can (Anthropic), the span still stops before that exchange.
func TestSummarySpanRetreatsOnlyWhereABreakpointCanBeWritten(t *testing.T) {
	covered := func(noBreakpoint bool) (got, retreated int) {
		s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
		s.modelClient = &capturingModel{out: "<summary>read the docs.</summary>"}
		in := []bschemas.ChatMessage{caMsg(bschemas.ChatMessageRoleUser, "TASK: fix the handler")}
		in = append(in, caToolPair("reading", "read", `{"p":"a"}`, "call_a", strings.Repeat("doc line\n", 40))...)
		in = append(in, caFixture()[1:]...)
		in = append(in, caToolPair("reading again", "read", `{"p":"b"}`, "call_b", strings.Repeat("doc line\n", 40))...)
		in = append(in, caFixture()[1:]...)
		_, start, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
		retreated = retreatPastLastToolExchange(in, nil, start, end) - start
		if retreated >= end-start {
			t.Fatalf("fixture: the retreat must move the end (%d >= %d)", retreated, end-start)
		}
		c := caCtx("ca-730-" + map[bool]string{true: "openai", false: "anthropic"}[noBreakpoint])
		c.NoCacheBreakpoint = noBreakpoint
		caTurn(t, s, c, in)
		if !WaitForSummaryForTest(convKey(c), 5*time.Second) {
			t.Fatal("the commissioned summary never landed")
		}
		cp, ok := loadCheckpoint(c)
		if !ok {
			t.Fatal("no checkpoint was written")
		}
		return cp.CoveredCount, retreated
	}
	openai, retreated := covered(true)
	if openai <= retreated {
		t.Errorf("OpenAI summary covers %d messages, want more than the retreated %d", openai, retreated)
	}
	anthropic, _ := covered(false)
	if anthropic != retreated {
		t.Errorf("Anthropic summary covers %d messages, want the retreated %d", anthropic, retreated)
	}
}
