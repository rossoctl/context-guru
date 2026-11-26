package schema

import "github.com/tidwall/gjson"

// ToolFnName is the name of a client function declaration, or "". Anthropic and Responses write it as a
// top-level `name`; Chat Completions nests it under `function`. The two places never both hold a name on
// one wire, so the reader needs no wire argument: a caller that sees only a bare tools array (the
// accountant's envelope diff) reads every wire the same way.
func ToolFnName(t gjson.Result) string {
	if n := t.Get("name").String(); n != "" {
		return n
	}
	return t.Get("function.name").String()
}

// ToolDeclName is the name a tools[] element goes by, on any wire. It is the one string the inventory
// shows, the removal matches, and the accountant keys on. A declaration that has no function name
// names itself through the first of these that is set: `mcp_server_name` (an Anthropic mcp_toolset
// connector, one element for a whole server), `server_label` (a Responses `mcp` tool), then `type`
// (a provider tool such as web_search_*).
func ToolDeclName(t gjson.Result) string {
	if n := ToolFnName(t); n != "" {
		return n
	}
	if n := t.Get("mcp_server_name").String(); n != "" {
		return n
	}
	if n := t.Get("server_label").String(); n != "" {
		return n
	}
	return t.Get("type").String()
}

// ToolFnNameOn is ToolFnName for a host that knows its wire: it reads the one place the wire writes
// the name, so a body of another shape is not read. Anthropic and Responses write a top-level `name`.
// Every other wire, an empty one too, is read as Chat Completions, which nests it under `function`.
func ToolFnNameOn(wire string, t gjson.Result) string {
	if wire == WireAnthropic || IsResponses(wire) {
		return t.Get("name").String()
	}
	return t.Get("function.name").String()
}

// HasToolNamed reports whether body's tools array holds a function declaration called name on wire.
func HasToolNamed(wire string, body []byte, name string) bool {
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		found = ToolFnNameOn(wire, t) == name
		return !found
	})
	return found
}
