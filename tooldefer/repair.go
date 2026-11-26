package tooldefer

import (
	"regexp"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A transcript can name a tool the request no longer declares: a search loaded it, then the user
// removed its MCP server and resumed the session. The provider refuses such a request with a 400
// ("Tool reference '<name>' not found in available tools"), and the client's own body holds the same
// block, so sending the original does not help.
//
// RepairRefs adds a deferred stub for each such tool: its name, a fixed description and an empty
// schema, with defer_loading. A deferred tool is outside the cached prefix, so the stub does not
// rewrite the cache (measured: the same cache read with and without stubs), and the transcript keeps
// its bytes. Dropping the reference instead would change a block inside the cached prefix.

// stubDescription tells the model the tool is gone, if it ever loads the stub.
const stubDescription = "This tool is no longer available in this session."

// RepairRefs returns body with a deferred stub for every tool a tool_reference in the transcript
// names and `tools` does not declare. ok=false means nothing to repair; out is then body.
func RepairRefs(body []byte) (out []byte, added []string, ok bool) {
	refs := Loaded(body)
	if len(refs) == 0 {
		return body, nil, false
	}
	declared := map[string]bool{}
	tools := gjson.GetBytes(body, "tools")
	tools.ForEach(func(_, t gjson.Result) bool {
		declared[t.Get("name").String()] = true
		return true
	})
	for n := range refs {
		if !declared[n] {
			added = append(added, n)
		}
	}
	if len(added) == 0 {
		return body, nil, false
	}
	sort.Strings(added)
	return StubTools(body, added)
}

// StubTools appends a deferred stub for each of names to `tools`. ok=false on a shape it does not
// handle; out is then body.
func StubTools(body []byte, names []string) (out []byte, added []string, ok bool) {
	tools := gjson.GetBytes(body, "tools")
	if len(names) == 0 || (tools.Exists() && !tools.IsArray()) {
		return body, nil, false
	}
	var elems []string
	tools.ForEach(func(_, t gjson.Result) bool {
		elems = append(elems, t.Raw)
		return true
	})
	for _, n := range names {
		s, err := sjson.Set(`{"name":"","description":"","input_schema":{"type":"object","properties":{}},"defer_loading":true}`, "name", n)
		if err != nil {
			return body, nil, false
		}
		s, _ = sjson.Set(s, "description", stubDescription)
		elems = append(elems, s)
	}
	out, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(elems, ",")+"]"))
	if err != nil {
		return body, nil, false
	}
	return out, names, true
}

// missingRef matches the provider's refusal of a tool_reference to an undeclared tool.
var missingRef = regexp.MustCompile(`Tool reference '([^']{1,256})' not found in available tools`)

// MissingRef returns the tool name a provider 400 says a tool_reference names but `tools` lacks,
// or "" when the error is another one.
func MissingRef(errBody []byte) string {
	m := missingRef.FindSubmatch(errBody)
	if m == nil {
		return ""
	}
	return string(m[1])
}
