package extract

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// Diagnostic: what does a sonnet-class model actually REPLY with, and why did a real
// session's reply hit the output cap? Gated exactly like the other live test.
func TestProbeRawReply(t *testing.T) {
	if os.Getenv("CG_LIVE") == "" {
		t.Skip("live probe")
	}
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "2024-01-01T00:%02d:%02d GET /users/%d 200 12ms handler=src/api/users.py\n", i%60, i%60, i)
	}
	b.WriteString("2024-01-01T01:00:00 ERROR auth timeout on token refresh user=42\n")
	body := b.String()
	sys, user := buildCodePromptSplit(body, "Find the auth timeout in src/api/users.py and fix it.",
		[]string{"auth", "users.py"}, true, AggroMedium)
	fmt.Printf("system blocks: %d (%d + %d chars)\nuser: %d chars\n",
		len(sys), len(sys[0]), len(sys[1]), len(user))
	client := cheapmodel.Anthropic{
		BaseURL: os.Getenv("CG_BASE"), APIKey: os.Getenv("CG_TOKEN"), AuthScheme: "bearer",
		Model: liveModel(), MaxTokens: 4096,
	}
	raw, err := client.CompleteBlocks(context.Background(), sys, user)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	fmt.Printf("--- RAW REPLY (%d chars) ---\n%s\n--- END ---\n", len(raw), raw)
	out, sum := execStarlarkSummary(context.Background(), body, stripFences(raw))
	fmt.Printf("executed: out=%d chars (input %d), summary=%q\n", len(out), len(body), sum)
}
