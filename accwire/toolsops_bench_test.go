package accwire

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// toolsBody builds a tools array of n named declarations, with the last one removed in the forwarded copy.
func toolsBody(n int, chat, drop bool) []gjson.Result {
	var parts []string
	for i := 0; i < n; i++ {
		if drop && i == n-1 {
			break
		}
		if chat {
			parts = append(parts, fmt.Sprintf(`{"type":"function","function":{"name":"tool_%d","description":"does a job","parameters":{"type":"object"}}}`, i))
		} else {
			parts = append(parts, fmt.Sprintf(`{"name":"tool_%d","description":"does a job","input_schema":{"type":"object"}}`, i))
		}
	}
	return gjson.Parse("[" + strings.Join(parts, ",") + "]").Array()
}

func BenchmarkToolsOps(b *testing.B) {
	for _, chat := range []bool{false, true} {
		name := "anthropic"
		if chat {
			name = "chat"
		}
		cl, fw := toolsBody(60, chat, false), toolsBody(60, chat, true)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				toolsOps(cl, fw)
			}
		})
	}
}
