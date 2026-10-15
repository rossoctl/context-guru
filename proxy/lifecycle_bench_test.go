package proxy

import (
	"fmt"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/waitcause"
)

// bigAgentBody is an Anthropic transcript of about 300K tokens (~1.2 MB): 300 tool turns.
func bigAgentBody() []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"start"}`)
	blob := strings.Repeat("lorem ipsum dolor sit amet ", 150) // ~4 KB
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"ls"}}]}`, i)
		fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":%q}]}`, i, blob)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// The two per-request body scans the dashboard writer goroutine runs: wait-cause then
// lifecycle. Twice = each normalizes (before this change); Once = they share one pass.
func BenchmarkWriterBodyScansNormalizeTwice(b *testing.B) {
	body := bigAgentBody()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		waitcause.New().Observe("t", "s", "th", bschemas.Anthropic, body)
		scanLifecycleOld(bschemas.Anthropic, body)
	}
}

func BenchmarkWriterBodyScansNormalizeOnce(b *testing.B) {
	body := bigAgentBody()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var msgs []bschemas.ChatMessage
		var done bool
		norm := func() []bschemas.ChatMessage {
			if !done {
				msgs, done = apply.NormalizeMessages(bschemas.Anthropic, body), true
			}
			return msgs
		}
		waitcause.New().ObserveWith("t", "s", "th", body, norm)
		scanLifecycle(norm, body)
	}
}
