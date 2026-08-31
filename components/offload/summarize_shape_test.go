package offload

import (
	"context"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// schema.ValidateShape over what summarize EMITS, which is the one static check that would
// have caught the shape defects this component shipped three times running:
//
//	2edb9d4  the summary emitted as a system message, spliced in front of the kept tail
//	fb5c460  a tool_result left behind by the span that removed its tool_use
//	e7d1aa8  a preserved assistant tool-call head whose results were inside the span
//
// Each was found only by a provider rejecting a live request, because nothing asserted the
// shape of the output and every offline measurement replayed through /compact, which never
// forwards upstream. The transcripts below are the two shapes that make the boundary
// arithmetic go wrong, run across every keep_last that puts the boundary somewhere new.

// callMsg is an assistant turn carrying one or more tool calls — a parallel call when it
// carries several, exactly as apply.attachToolUse lifts an Anthropic assistant message.
func callMsg(ids ...string) bschemas.ChatMessage {
	calls := make([]bschemas.ChatAssistantMessageToolCall, 0, len(ids))
	for i := range ids {
		id := ids[i]
		calls = append(calls, bschemas.ChatAssistantMessageToolCall{
			Index: uint16(i), ID: &id})
	}
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: calls}}
}

// bulkResult is a tool result big enough that summarize finds the span worth compressing.
func bulkResult(id string) bschemas.ChatMessage {
	return toolResultMsgWithID(id,
		strings.Repeat("ran pytest tests/test_"+id+".py, 3 failures in src/mod/file.go\n", 40))
}

func newSummarizeKeepLast(t *testing.T, keepLast int) *Summarize {
	t.Helper()
	c, err := newSummarize([]byte(
		"keep_last: " + strconv.Itoa(keepLast) + "\nmin_tokens: 10\nresummarize_tokens: 0\n" +
			"trigger:\n  min_messages: 2\n  min_request_tokens: 10\n"))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	s := c.(*Summarize)
	s.modelClient = &fixedModel{out: "SUMMARY: explored the handler, 3 tests fail."}
	return s
}

func TestSummarizeEmitsAShapeValidTranscript(t *testing.T) {
	fixtures := map[string][]bschemas.ChatMessage{
		// The ordinary shape: a system prompt at index 0, a PARALLEL tool exchange in the
		// middle, a user turn at the tail. The summary lands at index 1, in front of that
		// tail — the position 2edb9d4's system-role summary was rejected at — and the span
		// boundary can fall between the parallel call and its two results (fb5c460).
		"system head, parallel exchange": {
			sysMsg("you are a coding agent"),
			userMsg("Fix the failing handler in src/mod/file.go and run the tests."),
			callMsg("t_a", "t_b"),
			bulkResult("t_a"), bulkResult("t_b"),
			userMsg("keep going"),
		},
		// The shape e7d1aa8 was about: msgs[0] is itself an assistant tool-call message, so
		// preserving it as the head leaves its calls unanswered once their results are
		// summarized away. dropOrphanedToolResults cannot repair this direction — there is
		// nothing orphaned to drop, the CALL is the thing left dangling.
		"assistant tool-call head": {
			callMsg("t_head"),
			bulkResult("t_head"),
			userMsg("Fix the failing handler and run the tests."),
			userMsg("keep going"),
		},
		// A serial exchange at the tail, so the boundary can land between a single call and
		// its single result too.
		"serial exchange at the tail": {
			sysMsg("you are a coding agent"),
			userMsg("Fix the failing handler and run the tests."),
			bulkResult("t_old"), // an idless-history-shaped result, its call already summarized
			callMsg("t_1"), bulkResult("t_1"),
			userMsg("keep going"),
		},
	}

	acted := 0
	for name, base := range fixtures {
		for keepLast := 1; keepLast <= 4; keepLast++ {
			// The fixture is deliberately re-cloned: Offload reassigns req.Input but the
			// messages themselves are shared, and a leaked mutation would make the next
			// keep_last mean something else.
			req := &bschemas.BifrostChatRequest{Input: schema.CloneMessages(base)}
			var rep components.Report
			c := &components.Ctx{Ctx: context.Background(), Session: "s",
				Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000}
			if _, err := newSummarizeKeepLast(t, keepLast).Offload(req, &rep, c); err != nil {
				t.Fatalf("%s keep_last=%d: Offload: %v", name, keepLast, err)
			}
			if rep.Skipped {
				continue
			}
			acted++
			if vs := schema.ValidateShape(req.Input); len(vs) != 0 {
				t.Errorf("%s keep_last=%d: summarize emitted %d shape violation(s) — a "+
					"provider rejects the whole request:\n%s", name, keepLast, len(vs),
					schema.FormatShapeViolations(vs, req.Input))
			}
		}
	}
	// The fixtures must actually be summarized, or every assertion above is vacuous. Three
	// tests on this branch's ancestor passed with their fix removed for exactly this reason.
	if acted < len(fixtures) {
		t.Fatalf("summarize acted on only %d of %d fixture/keep_last combinations; the shape "+
			"assertions never ran on at least one fixture", acted, len(fixtures))
	}
}

// The paired proof that the assertion above has teeth: the transcripts the PRE-FIX code
// emitted, fed to the validator directly. Each must be rejected, and each must be rejected for
// the rule that names the defect — a check that fires for the wrong reason is not a check.
func TestValidateShapeRejectsTheHistoricalSummarizeOutputs(t *testing.T) {
	summary := func(role bschemas.ChatMessageRole) bschemas.ChatMessage {
		m := bschemas.ChatMessage{Role: role}
		schema.SetMessageText(&m, "=== History Summary ===\nthe earlier trajectory")
		return m
	}
	cases := []struct {
		name string
		rule string
		msgs []bschemas.ChatMessage
	}{
		// 2edb9d4: [msgs[0], summary(system), tail...] with the system prompt at index 0.
		{"system-role summary", schema.RuleSystemPosition, []bschemas.ChatMessage{
			sysMsg("you are a coding agent"),
			summary(bschemas.ChatMessageRoleSystem),
			userMsg("keep going")}},
		// fb5c460: the span took the call, the tail kept the result.
		{"orphaned tool_result", schema.RulePairedToolResult, []bschemas.ChatMessage{
			sysMsg("s"), summary(bschemas.ChatMessageRoleUser),
			bulkResult("t_gone"), userMsg("keep going")}},
		// e7d1aa8: the head kept the call, the span took the result.
		{"unanswered tool_use head", schema.RuleAnsweredToolUse, []bschemas.ChatMessage{
			callMsg("t_head"), summary(bschemas.ChatMessageRoleUser),
			userMsg("keep going")}},
	}
	for _, tc := range cases {
		vs := schema.ValidateShape(tc.msgs)
		if len(vs) == 0 {
			t.Errorf("%s: the pre-fix transcript was accepted; the validator cannot have "+
				"caught this defect", tc.name)
			continue
		}
		found := false
		for _, v := range vs {
			if v.Rule == tc.rule {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: rejected, but not for %s:\n%s", tc.name, tc.rule,
				schema.FormatShapeViolations(vs, tc.msgs))
		}
	}
}

// e9bf3a7 is the one of the four this validator CANNOT catch, and saying so is the point: it
// was a panic inside the boundary arithmetic, so there was never an output list to inspect
// (and pipeline.runOne swallowed the panic into verdict=reverted). What is assertable is the
// property that replaced it — a transcript too short to summarize comes back untouched and
// well-formed, rather than half-rewritten.
func TestSummarizeLeavesAShortTranscriptShapeValid(t *testing.T) {
	base := []bschemas.ChatMessage{userMsg("hi"), assistantMsg("hello")}
	for keepLast := 1; keepLast <= 20; keepLast++ {
		req := &bschemas.BifrostChatRequest{Input: schema.CloneMessages(base)}
		var rep components.Report
		c := &components.Ctx{Ctx: context.Background(), Session: "s",
			Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000}
		// No recover() on purpose: a panic must fail this test, not be absorbed the way
		// pipeline.runOne absorbs it in production.
		if _, err := newSummarizeKeepLast(t, keepLast).Offload(req, &rep, c); err != nil {
			t.Fatalf("keep_last=%d: Offload: %v", keepLast, err)
		}
		if vs := schema.ValidateShape(req.Input); len(vs) != 0 {
			t.Errorf("keep_last=%d: a short turn came back malformed:\n%s", keepLast,
				schema.FormatShapeViolations(vs, req.Input))
		}
	}
}
