package apply

import (
	"testing"

	"github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

func BenchmarkDeclName(b *testing.B) {
	tools := map[string]gjson.Result{
		"anthropic": gjson.Parse(`{"name":"Read","description":"reads","input_schema":{"type":"object"}}`),
		"chat":      gjson.Parse(`{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}`),
		"mcp":       gjson.Parse(`{"type":"mcp","server_label":"srv","server_url":"https://x"}`),
	}
	for name, t := range tools {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = schema.ToolDeclName(t)
			}
		})
	}
}
