package respfacts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

// anthropicStream is a streamed answer of n text deltas and one tool call, the shape of a long turn.
func anthropicStream(n int) []byte {
	ev := func(s string) string { return "event: x\ndata: " + s + "\n\n" }
	var b strings.Builder
	b.WriteString(ev(`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":40000,"output_tokens":1}}}`))
	b.WriteString(ev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for i := 0; i < n; i++ {
		b.WriteString(ev(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"word %d of the answer "}}`, i)))
	}
	b.WriteString(ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c","name":"Bash","input":{}}}`))
	b.WriteString(ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"timeout\":5000}"}}`))
	b.WriteString(ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":300}}`))
	return []byte(b.String())
}

// BenchmarkReadStream is the cost of reading one streamed response: usage, stop reason and calls.
func BenchmarkReadStream(b *testing.B) {
	body := anthropicStream(2000)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := Read(agent.Client{}, "text/event-stream", body, false); f.StopReason != "tool_use" {
			b.Fatal(f.StopReason)
		}
	}
}
