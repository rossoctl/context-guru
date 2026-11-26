package offload

import (
	"context"
	"os"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
	"github.com/rossoctl/context-guru/schema"
)

// Live example of the summarize component: real model compresses a realistic
// mid-trajectory span into one summary. Gated by CG_LIVE=1 (+ CG_BASE/CG_TOKEN).
func TestLiveSummarizeExample(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... to run the live example")
	}
	d, ref := dispatchtest.HTTP(os.Getenv("CG_BASE"), dispatchtest.AnthropicAuth(os.Getenv("CG_TOKEN"), ""))
	model := cheapmodel.Anthropic{D: d, Ref: ref, Model: "claude-sonnet-4-6", MaxTokens: 2048}
	msg := func(role bschemas.ChatMessageRole, text string) bschemas.ChatMessage {
		m := bschemas.ChatMessage{Role: role}
		schema.SetMessageText(&m, text)
		return m
	}
	// A realistic exploration span: the agent greps, reads a file, runs the tests.
	span := []bschemas.ChatMessage{
		msg(bschemas.ChatMessageRoleAssistant, "Let me find where col_insert is defined."),
		msg(bschemas.ChatMessageRoleTool, "sympy/matrices/common.py:84:    def col_insert(self, pos, other):\nsympy/matrices/common.py:90:    def row_insert(self, pos, other):"),
		msg(bschemas.ChatMessageRoleAssistant, "Reading the col_insert implementation."),
		msg(bschemas.ChatMessageRoleTool, strings.Repeat("    # boilerplate docstring line describing the method contract\n", 30)+
			"    def col_insert(self, pos, other):\n        if pos < 0:\n            pos = self.cols + pos\n        if pos < 0:\n            pos = 0\n        elif pos > self.cols:\n            pos = self.cols\n        return self._eval_col_insert(pos, other)\n"),
		msg(bschemas.ChatMessageRoleAssistant, "Running the failing test."),
		msg(bschemas.ChatMessageRoleTool, "tests/test_matrices.py::test_col_insert FAILED\nE   IndexError: Index out of range: a[2]\nsympy/matrices/common.py:86: IndexError\n1 failed, 180 passed"),
	}
	goal := "Fix the failing test test_col_insert (col_insert IndexError in sympy/matrices/common.py)."

	s, _ := newSummarize([]byte("include_tool_calls: true"))
	sum := s.(*Summarize)
	out, err := sum.summarize(context.Background(), model, span, goal)
	if err != nil {
		t.Fatal(err)
	}
	before := schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: span})
	after := schema.TextTokens(out)
	t.Logf("\n----- SPAN SUMMARIZED (%d messages, %d tokens) -> summary (%d tokens) -----\n%s\n",
		len(span), before, after, out)
	if strings.TrimSpace(out) == "" {
		t.Fatal("expected a summary")
	}
}
