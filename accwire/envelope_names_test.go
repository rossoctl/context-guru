package accwire

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/expand"
)

func chatTool(name string) string {
	return `{"type":"function","function":{"name":"` + name + `","description":"does a job","parameters":{"type":"object"}}}`
}

func toolArray(items ...string) []gjson.Result {
	s := "["
	for i, it := range items {
		if i > 0 {
			s += ","
		}
		s += it
	}
	return gjson.Parse(s + "]").Array()
}

// On Chat Completions the name sits under function. Read from `name` alone, every tool was unnamed and
// the same, so a removal could not be placed and no operation was reported.
func TestToolsOpsReadsAChatDeclarationName(t *testing.T) {
	cl := toolArray(chatTool("Read"), chatTool("Write"), chatTool("Bash"))
	fw := toolArray(chatTool("Read"), chatTool("Bash"))
	ops := toolsOps(cl, fw)
	if len(ops) != 1 || ops[0].Operator != "context-guru.toolfilter" {
		t.Fatalf("ops = %+v, want one toolfilter removal", ops)
	}
}

// The expand tool is booked to expand, not to toolschema, on Chat as on Anthropic.
func TestToolsOpsBooksAChatExpandInjectionToExpand(t *testing.T) {
	cl := toolArray(chatTool("Read"))
	fw := toolArray(chatTool("Read"), chatTool(expand.ToolName))
	ops := toolsOps(cl, fw)
	if len(ops) != 1 || ops[0].Operator != "context-guru."+OpExpand {
		t.Fatalf("ops = %+v, want one %s", ops, OpExpand)
	}
}

// Two mcp_toolset connectors name themselves by server, so removing one is placed.
func TestToolsOpsNamesConnectorsByServer(t *testing.T) {
	a := `{"type":"mcp_toolset","mcp_server_name":"alpha"}`
	b := `{"type":"mcp_toolset","mcp_server_name":"beta"}`
	ops := toolsOps(toolArray(a, b), toolArray(a))
	if len(ops) != 1 || ops[0].Operator != "context-guru.toolfilter" {
		t.Fatalf("ops = %+v, want one toolfilter removal", ops)
	}
}
