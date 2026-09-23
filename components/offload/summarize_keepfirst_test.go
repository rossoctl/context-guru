package offload

import (
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// THE DEFECT THIS GUARDS AGAINST
//
// The head was a hardcoded 1, so exactly one leading message was ever kept verbatim. "Preserve
// msgs[0]" means two different things across the dialects, and only one of them keeps the task:
//
//	Anthropic traffic  — the system prompt is a TOP-LEVEL `system` field the pipeline never sees,
//	                     so msgs[0] IS the opening user turn. The task is pinned. Correct.
//	OpenAI-shaped      — msgs[0] is the system prompt and the task statement is msgs[1], which is
//	                     the FIRST message of the summarized span. The task is folded into the
//	                     summary on every eligible turn, and no config could stop it.
//
// It is not a lost saving: the agent is left working from a paraphrase of its own instructions.
// The task text does still reach the summarizer as the goal (conversationGoal scans by ROLE, so it
// finds the first user message wherever it sits), which is why this was survivable rather than
// obvious — the summary is written toward the task, but the verbatim instruction is gone.
//
// The contract is therefore: keep_first messages stay verbatim at the head, and an OpenAI-shaped
// deployment can set 2 to retain [system, task].
func summarizeWithKeepFirst(t *testing.T, keepFirst int) *Summarize {
	t.Helper()
	c, err := newSummarize([]byte(
		"keep_first: " + itoa(keepFirst) + "\nkeep_last: 2\nmin_tokens: 10\nresummarize_tokens: 0\n" +
			"trigger:\n  min_messages: 4\n  min_request_tokens: 10\n  min_request_frac: 0\n  cache_state: any\n"))
	if err != nil {
		t.Fatalf("newSummarize(keep_first: %d): %v", keepFirst, err)
	}
	s, ok := c.(*Summarize)
	if !ok {
		t.Fatalf("newSummarize returned %T, want *Summarize", c)
	}
	s.modelClient = &fixedModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	s.mode = markerOff
	return s
}

// openAIShaped is the layout the defect is about: index 0 is the system prompt and index 1 is the
// task statement, exactly as an OpenAI-compatible client sends them.
func openAIShaped() []bschemas.ChatMessage {
	span := strings.Repeat("ran pytest tests/test_handler.py, 3 failures in src/mod/file.py\n", 40)
	return []bschemas.ChatMessage{
		sysMsg("you are a coding agent"),
		userMsg("TASK: Fix the failing handler in src/mod/file.py and run the tests."),
		toolResultMsg(span),
		toolResultMsg(span),
		toolResultMsg(span),
		userMsg("keep going"),
	}
}

// runSummarize drives the real two-turn path: the summary is commissioned OFF the hot path, so
// turn 1 forwards untouched and turn 2 is the turn that splices. Draining on the production
// channel (rather than sleeping) is what makes this deterministic — see summarize_testhook.go.
func runSummarize(t *testing.T, s *Summarize, session string, msgs []bschemas.ChatMessage) []bschemas.ChatMessage {
	t.Helper()
	c := &components.Ctx{Ctx: context.Background(), Session: session, Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1}
	var rep0 components.Report
	turn1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	if _, err := s.Offload(turn1, &rep0, c); err != nil {
		t.Fatalf("turn 1 Offload: %v", err)
	}
	if !WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatalf("turn 1's summary never landed, so turn 2 has nothing to splice")
	}
	turn2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep components.Report
	if _, err := s.Offload(turn2, &rep, c); err != nil {
		t.Fatalf("turn 2 Offload: %v", err)
	}
	// A vacuity guard: if turn 2 forwarded the transcript unchanged, every assertion about WHICH
	// messages survived would pass for the wrong reason.
	if len(turn2.Input) >= len(msgs) {
		t.Fatalf("turn 2 spliced nothing (%d messages in, %d out) — the fixture is not exercising "+
			"the span, so the head assertions below would be vacuous", len(msgs), len(turn2.Input))
	}
	return turn2.Input
}

// The default (1) reproduces the historical shape: only msgs[0] survives, so on OpenAI-shaped
// traffic the task statement is inside the span. This is asserted rather than fixed, because
// changing the default would change every existing deployment's output — the fix is that 2 is now
// reachable at all.
func TestSummarizeKeepFirstOneDropsTheTaskOnOpenAIShapedTraffic(t *testing.T) {
	out := runSummarize(t, summarizeWithKeepFirst(t, 1), "kf1", openAIShaped())
	if !holdsText(out, "you are a coding agent") {
		t.Errorf("keep_first 1 dropped msgs[0]; head must always survive")
	}
	if holdsText(out, "TASK: Fix the failing handler") {
		t.Errorf("keep_first 1 kept the task verbatim — the fixture no longer reproduces the "+
			"layout this test exists for, so the keep_first 2 case below proves nothing.\ngot: %v",
			roles(out))
	}
}

// keep_first: 2 is the fix an OpenAI-shaped deployment applies: [system, task] both stay verbatim.
func TestSummarizeKeepFirstTwoRetainsTheTaskStatement(t *testing.T) {
	out := runSummarize(t, summarizeWithKeepFirst(t, 2), "kf2", openAIShaped())
	if !holdsText(out, "you are a coding agent") {
		t.Errorf("keep_first 2 dropped the system prompt at msgs[0]")
	}
	if !holdsText(out, "TASK: Fix the failing handler") {
		t.Fatalf("keep_first 2 did NOT retain the task statement at msgs[1] — the defect is back.\ngot: %v",
			roles(out))
	}
	// The role contract from summarize_role_test.go still holds with a larger head.
	for i := range out {
		if out[i].Role == bschemas.ChatMessageRoleSystem && i != 0 {
			t.Errorf("system-role message at index %d — the provider rejects this", i)
		}
	}
}

// A head that would END on an assistant message whose tool results are inside the span is retreated
// past, exactly as the hardcoded rule did at 1 — otherwise the request carries an unanswered call
// and the provider rejects the whole thing.
func TestSummarizeKeepFirstRetreatsOffADanglingToolCall(t *testing.T) {
	span := strings.Repeat("output line\n", 40)
	msgs := []bschemas.ChatMessage{
		sysMsg("you are a coding agent"),
		asstMsg("calling the tool", "grep", `{"q":"x"}`), // its result is inside the span
		toolResultMsg(span),
		toolResultMsg(span),
		userMsg("keep going"),
	}
	headCount, start, end := summarizeSpan(msgs, 2, 1)
	if headCount != 1 {
		t.Errorf("keep_first 2 ending on a tool-calling assistant: headCount = %d, want 1 "+
			"(the dangling call must fold into the span)", headCount)
	}
	if start != headCount {
		t.Errorf("start = %d, want headCount %d", start, headCount)
	}
	if end <= start {
		t.Errorf("end = %d <= start = %d, so nothing would be summarized", end, start)
	}
}

// keep_first 0 is a real configuration — summarize everything, pin nothing — and must not be
// confused with unset. A negative value is a typo and is refused rather than clamped.
func TestSummarizeKeepFirstZeroIsHonouredAndNegativeIsRefused(t *testing.T) {
	if _, start, _ := summarizeSpan(openAIShaped(), 0, 2); start != 0 {
		t.Errorf("keep_first 0: start = %d, want 0", start)
	}
	if _, err := newSummarize([]byte("keep_first: -1\n")); err == nil {
		t.Errorf("keep_first: -1 was accepted; a negative head must be refused, not read as 0")
	}
}
