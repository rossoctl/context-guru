package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fixedModel returns one canned summary, so the test asserts on the SHAPE of what
// summarize emits rather than on a model's wording.
type fixedModel struct{ out string }

func (m *fixedModel) Complete(context.Context, string) (string, error) { return m.out, nil }

// THE REGRESSION THIS GUARDS AGAINST
//
// summarize emitted its summary as a SYSTEM-role message and spliced it in as
// [msgs[0], summary, tail...]. When msgs[0] is itself the system prompt — the normal case —
// that puts a system role at index 1, and Anthropic rejects the entire request:
//
//	400 messages.1: role 'system' must precede an 'assistant' message or end the array
//
// It shipped because nothing asserted the summary's role, and because every measurement
// before this replayed through /compact, which never forwards upstream and so never had a
// body validated by a provider. It was found only when LOCA-bench ran against a real API:
// every task that triggered a summarization failed, including in an arm with no other
// component enabled.
//
// The contract is therefore: the summary must NOT be system-role, and no system-role
// message may appear anywhere except index 0.
func TestSummarizeEmitsNoSystemRoleAwayFromTheHead(t *testing.T) {
	s := newSummarizeTestComponent(t, &fixedModel{out: "SUMMARY: explored the handler, 3 tests fail."})
	span := strings.Repeat("ran pytest tests/test_handler.py, 3 failures in src/mod/file.py\n", 40)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			// index 0 is the system prompt, exactly as a real agent sends it
			sysMsg("you are a coding agent"),
			userMsg("Fix the failing handler in src/mod/file.py and run the tests."),
			toolResultMsg(span),
			toolResultMsg(span),
			userMsg("keep going"),
		},
	}
	var rep components.Report
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{})}
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if rep.Skipped {
		t.Skip("summarize declined on this fixture; the role assertion needs it to act")
	}
	for i, m := range req.Input {
		if m.Role == bschemas.ChatMessageRoleSystem && i != 0 {
			t.Fatalf("system-role message at index %d — Anthropic rejects this "+
				"(400 messages.%d: role 'system' must precede an 'assistant' message or "+
				"end the array). Messages: %s", i, i, roleList(req.Input))
		}
	}
}

// sysMsg is a system-role message, the shape a real agent puts at index 0.
func sysMsg(text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleSystem}
	schema.SetMessageText(&m, text)
	return m
}

func roleList(msgs []bschemas.ChatMessage) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(string(m.Role))
	}
	return b.String()
}

// THE SECOND REGRESSION, found only after the first was fixed
//
// summarize replaces a span with one summary message, so the kept tail can begin part-way
// through a tool exchange — its leading `tool_result` blocks answer `tool_use` blocks that
// were just deleted. Anthropic rejects the whole request:
//
//	400 messages.0.content.2: unexpected `tool_use_id` found in `tool_result` blocks
//
// Measured on live LOCA-bench traffic: 5 of 12 tasks failed this way once the system-role
// defect was fixed and summarize could finally act at all.
//
// This is an invariant for any component that DELETES messages. coref does not need it —
// it rewrites a tool message's text in place and never removes a message, so pairing holds
// by construction.
func TestDropOrphanedToolResults(t *testing.T) {
	call := func(id string) bschemas.ChatMessage {
		m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &bschemas.ChatAssistantMessage{
				ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id}},
			}}
		return m
	}
	result := func(id string) bschemas.ChatMessage {
		m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
			ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
		schema.SetMessageText(&m, "output for "+id)
		return m
	}

	// A well-formed history must pass through untouched — the repair has to be idempotent
	// and must never drop a result whose call is present, even at a distance (a summary can
	// legitimately sit between a call and its result).
	ok := []bschemas.ChatMessage{userMsg("go"), call("t1"), result("t1"), call("t2"), result("t2")}
	got, n := dropOrphanedToolResults(ok)
	if n != 0 || len(got) != len(ok) {
		t.Errorf("well-formed history must be unchanged, dropped %d (%d -> %d)", n, len(ok), len(got))
	}

	// The summarize shape: the span holding call("t1") was replaced by a summary, so the
	// tail's result("t1") is orphaned and must go, while result("t2") stays.
	orphaned := []bschemas.ChatMessage{
		userMsg("go"),
		userMsg("SUMMARY: earlier work, including a call whose result follows"),
		result("t1"), // its call was deleted
		call("t2"), result("t2"),
	}
	got, n = dropOrphanedToolResults(orphaned)
	if n != 1 {
		t.Fatalf("expected exactly 1 orphaned result dropped, got %d", n)
	}
	for _, m := range got {
		if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil && *m.ChatToolMessage.ToolCallID == "t1" {
			t.Error("orphaned tool_result for t1 survived the repair")
		}
	}
	if len(got) != len(orphaned)-1 {
		t.Errorf("repair removed %d messages, want 1", len(orphaned)-len(got))
	}
}
