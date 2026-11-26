package expand

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/schema"
)

func BenchmarkRepairToolResults(b *testing.B) {
	var msgs []string
	for i := 0; i < 100; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Read","input":{"p":"%s"}}]}`, i, strings.Repeat("x", 200)))
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}`, i, strings.Repeat("y", 400)))
	}
	msgs = append(msgs, `{"role":"assistant","content":[{"type":"tool_use","id":"e1","name":"`+ToolName+`","input":{"id":"abc"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"e1","is_error":true,"content":"No such tool"}]}`)
	body := []byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`)
	resolve := func(string) (string, bool) { return "original text", true }
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		RepairToolResults(schema.WireAnthropic, body, resolve)
	}
}
