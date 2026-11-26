package tooldefer

import (
	"fmt"
	"strings"
	"testing"
)

// loadedBody is a transcript of n turns; every fifth turn is a search that loaded a tool.
func loadedBody(n int, refs bool) []byte {
	var msgs []string
	for i := 0; i < n; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Read","input":{"p":"%s"}}]}`, i, strings.Repeat("x", 200)))
		if refs && i%5 == 0 {
			msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":[{"type":"tool_reference","tool_name":"mcp__a__b%d"}]}]}`, i, i))
		} else {
			msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}`, i, strings.Repeat("y", 400)))
		}
	}
	return []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`)
}

func BenchmarkLoaded(b *testing.B) {
	for name, body := range map[string][]byte{"no_refs": loadedBody(100, false), "refs": loadedBody(100, true)} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				Loaded(body)
			}
		})
	}
}

func BenchmarkReadCalls(b *testing.B) {
	body := loadedBody(100, true)
	// miss: no call in the transcript is of a deferred tool, the usual case. hit: every call is.
	for name, deferred := range map[string]map[string]bool{"miss": {"mcp__a__b": true}, "hit": {"Read": true}} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ReadCalls(body, deferred)
			}
		})
	}
}
