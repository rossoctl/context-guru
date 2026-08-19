package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// TestDiagStarlarkOnRealOutput runs the real code-strategy prompt against the real gateway
// on a real captured tool output and prints exactly what came back and what the sandbox did
// with it. It exists because RunExtractionDetail collapses "no reply", "transport error" and
// "the sandbox refused the program" into one reason string.
func TestDiagStarlarkOnRealOutput(t *testing.T) {
	if os.Getenv("CG_DIAG") == "" {
		t.Skip("set CG_DIAG=1")
	}
	body := os.Getenv("CG_DIAG_BODY")
	if body == "" {
		t.Skip("set CG_DIAG_BODY to a file holding one tool output")
	}
	raw, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	m := cheapmodel.Anthropic{
		BaseURL:    os.Getenv("CHEAP_MODEL_BASE"),
		APIKey:     os.Getenv("CHEAP_MODEL_AUTH"),
		Model:      os.Getenv("CHEAP_MODEL"),
		AuthScheme: "bearer",
	}
	sys, user := buildCodePromptSplit(string(raw), "explore the repository", nil, true, false, AggroMedium)
	fmt.Printf("system blocks=%d total=%d chars   user=%d chars\n", len(sys), totalLen(sys), len(user))
	src, err := completeSplit(context.Background(), m, sys, user)
	fmt.Printf("reply err=%v len=%d\n", err, len(src))
	fmt.Printf("---- raw reply ----\n%s\n---- end ----\n", clip(src, 1500))
	stripped := stripFences(src)
	out, summary := execStarlarkSummary(context.Background(), string(raw), stripped)
	fmt.Printf("sandbox: out_len=%d (input %d) summary=%q\n", len(out), len(raw), summary)
	if out == "" {
		fmt.Printf("SANDBOX PRODUCED NOTHING — stripped source was:\n%s\n", clip(stripped, 1200))
	}
	b, _ := json.Marshal(map[string]any{"in": len(raw), "out": len(out)})
	fmt.Println(string(b))
}

func totalLen(s []string) (n int) {
	for _, x := range s {
		n += len(x)
	}
	return
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[clipped]"
}
