package all_test

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func userMsg(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

func TestExtractProjectsRelevantLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("noise line about unrelated internals blah blah\n")
	}
	b.WriteString("the authentication token refresh succeeded for user\n")
	for i := 0; i < 40; i++ {
		b.WriteString("more noise line about unrelated internals\n")
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("why did the authentication token refresh happen"),
		toolMsg(b.String()),
	}}
	before := schema.MessagesTokens(req)
	_, st := run(t, "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 50}\n", req)
	got := schema.MessageText(req.Input[1])
	if schema.TextTokens(got) >= before {
		t.Fatalf("extract should project down: %q", got[:min(80, len(got))])
	}
	if !strings.Contains(got, "authentication token refresh succeeded") {
		t.Fatal("the query-relevant line must be kept")
	}
	if keys := expand.ParseMarkers(got); len(keys) != 1 {
		t.Fatal("extract should leave an expand marker")
	} else if _, ok := expand.Resolve(st, keys[0]); !ok {
		t.Fatal("extract original must be recoverable")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
