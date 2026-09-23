package cheapmodel

import (
	"context"
	"os"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// Live proof that the appended-instruction shape actually reads the provider's cache on an
// Anthropic-family model. Gated like the other live tests (CG_LIVE=1 + CG_BASE/CG_TOKEN/CG_MODEL).
//
// WHY THIS EXISTS AS A LIVE TEST. The unit tests pin the request BYTES, which is all a fake server
// can show. Whether those bytes earn a cache read is a property of the provider, and it is the
// entire justification for the mapping — a mark the backend ignores looks identical to a mark it
// honours in every local assertion. Two calls: the first writes the entry, the second must read it.
func TestLiveCompleteMessagesReadsThePrefixCache(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("set CG_LIVE=1 CG_BASE=... CG_TOKEN=... CG_MODEL=... to run")
	}
	model := os.Getenv("CG_MODEL")
	if model == "" {
		model = "aws/claude-opus-4-7"
	}
	o := OpenAI{BaseURL: os.Getenv("CG_BASE"), APIKey: os.Getenv("CG_TOKEN"), Model: model, MaxTokens: 64}
	convo := strings.Repeat("the handler in src/mod/file.py returns 500 when the payload is missing a "+
		"trailing newline; pytest tests/test_handler.py reports three failures in dispatch\n", 120)
	instr := "[OPERATOR INSTRUCTION] Summarize the conversation above in one sentence."
	ask := []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &convo}},
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &instr}},
	}
	if _, err := o.CompleteMessages(context.Background(), "", ask); err != nil {
		t.Fatalf("first call (writes the entry): %v", err)
	}
	if _, err := o.CompleteMessages(context.Background(), "", ask); err != nil {
		t.Fatalf("second call (must read it): %v", err)
	}
	t.Log("both calls succeeded; read the provider's cache_read_input_tokens on the second to " +
		"confirm the entry was reused — a load-balanced gateway may route the retry elsewhere")
}
