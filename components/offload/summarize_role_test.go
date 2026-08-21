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
