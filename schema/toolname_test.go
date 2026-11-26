package schema

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestToolDeclName(t *testing.T) {
	for _, c := range []struct{ name, tool, want string }{
		{"anthropic", `{"name":"Read","input_schema":{}}`, "Read"},
		{"chat", `{"type":"function","function":{"name":"Read"}}`, "Read"},
		{"responses", `{"type":"function","name":"Read"}`, "Read"},
		{"connector", `{"type":"mcp_toolset","mcp_server_name":"srv"}`, "srv"},
		{"responses mcp", `{"type":"mcp","server_label":"srv","server_url":"u"}`, "srv"},
		{"server name before label", `{"type":"mcp","mcp_server_name":"a","server_label":"b"}`, "a"},
		{"provider tool", `{"type":"web_search_20250305"}`, "web_search_20250305"},
		{"empty", `{}`, ""},
	} {
		if got := ToolDeclName(gjson.Parse(c.tool)); got != c.want {
			t.Errorf("%s: ToolDeclName = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestToolFnNameOnReadsOnePlace(t *testing.T) {
	flat, nested := gjson.Parse(`{"type":"function","name":"A"}`), gjson.Parse(`{"type":"function","function":{"name":"A"}}`)
	for _, c := range []struct {
		wire string
		t    gjson.Result
		want string
	}{
		{WireAnthropic, flat, "A"}, {"", flat, ""}, {"", nested, "A"}, {WireResponses, flat, "A"}, {WireOpenAI, flat, ""},
		{WireAnthropic, nested, ""}, {WireResponses, nested, ""}, {WireOpenAI, nested, "A"},
	} {
		if got := ToolFnNameOn(c.wire, c.t); got != c.want {
			t.Errorf("ToolFnNameOn(%q, %s) = %q, want %q", c.wire, c.t.Raw, got, c.want)
		}
	}
}

func TestHasToolNamed(t *testing.T) {
	chat := []byte(`{"tools":[{"type":"function","function":{"name":"x"}},{"type":"function","function":{"name":"want"}}]}`)
	if !HasToolNamed(WireOpenAI, chat, "want") || HasToolNamed(WireAnthropic, chat, "want") || HasToolNamed(WireOpenAI, chat, "nope") {
		t.Error("HasToolNamed reads the wire's own name field")
	}
}
