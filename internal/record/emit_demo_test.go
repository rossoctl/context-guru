package record

import (
	"os"
	"testing"
	"time"
)

// TestWriteDemoFile is an escape hatch, not a unit test: with -run TestWriteDemoFile and
// CG_RECORD_DEMO_OUT set, it writes one realistic two-turn trace file so the EXISTING
// Python analysis pipeline can be pointed at it. Compatibility asserted in Go is a claim
// about my reading of the corpus; a file that the real scripts load is evidence.
//
// It no-ops without the env var, so it costs nothing in a normal run.
func TestWriteDemoFile(t *testing.T) {
	out := os.Getenv("CG_RECORD_DEMO_OUT")
	if out == "" {
		t.Skip("set CG_RECORD_DEMO_OUT to write a demo trace file")
	}
	start := time.Date(2026, 9, 28, 14, 0, 0, 896000000, time.UTC)
	turns := []Turn{
		{
			SessionID: "cg-demo-0001", SpanID: "aaaa000000000001",
			Model: "claude-sonnet-5", System: "anthropic",
			Start: start, End: start.Add(2 * time.Second),
			MaxTokens: 64000, Effort: "high", OutputTy: "json",
			RequestBody:  `[{"role":"user","content":"my key is sk-ant-api03-SECRETSECRETSECRETSECRET-XX"}]`,
			ResponseBody: `[{"role":"assistant","content":"noted"}]`,
			ToolDefs:     `[{"name":"Read","input_schema":{"type":"object"}}]`,
			FreshInput:   1200, CacheRead: 18000, CacheWrite: 400, Output: 350,
			StopReason: "end_turn", UpstreamMS: 1900, FirstByteMS: 380, Rounds: 1,
			ResponseSize: 4096,
		},
		{
			SessionID: "cg-demo-0001", SpanID: "aaaa000000000002", ParentID: "aaaa000000000001",
			Model: "claude-sonnet-5", System: "anthropic",
			Start: start.Add(3 * time.Second), End: start.Add(5 * time.Second),
			MaxTokens: 64000, OutputTy: "text",
			RequestBody: `[{"role":"user","content":"continue"}]`,
			// No response body, plus a reason: the unrecoverable path, emitted honestly.
			Incomplete: IncompleteRetainCap,
			FreshInput: 800, CacheRead: 19500, Output: 120,
			UpstreamMS: 2100, Rounds: 2,
		},
	}
	spans := make([]Span, 0, len(turns))
	for i := range turns {
		turns[i].Redact(1 << 20)
		spans = append(spans, turns[i].ToSpan())
	}
	b, err := NewFile("cg-demo-0001", spans, start.Add(6*time.Second)).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d bytes to %s", len(b), out)
}
