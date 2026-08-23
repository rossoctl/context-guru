package expand

import (
	"strings"
	"testing"
)

// WHEN IS THE EXPAND TOOL ACTUALLY AVAILABLE TO THE MODEL?
//
// Live runs showed the model calling context_guru_expand and the CLIENT answering
// "Tool context_guru_expand not found" -- 17 of 38 attempts in one arm, 48 of 108 in another. This
// pins the advertise rule that decides availability, instead of paraphrasing the comments around it.
func TestWhenIsExpandAdvertised(t *testing.T) {
	withMarker := `{"model":"claude-x","tools":[{"name":"Read"}],"messages":[
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"200 records <<cg:abc123>>"}]}]}`
	noMarker := `{"model":"claude-x","tools":[{"name":"Read"}],"messages":[
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"200 records, nothing removed"}]}]}`
	noTools := `{"model":"claude-x","messages":[
      {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x <<cg:abc123>>"}]}]}`

	for _, tc := range []struct {
		name, body string
		persists   bool
		mode       string
	}{
		{"auto + tools + marker", withMarker, true, InjectAuto},
		{"auto + tools + NO marker", noMarker, true, InjectAuto},
		{"auto + marker but NO tools", noTools, true, InjectAuto},
		{"auto + marker + store does NOT persist", withMarker, false, InjectAuto},
		{"always + NO marker", noMarker, true, InjectAlways},
	} {
		out, injected := Inject("anthropic", tc.mode, []byte(tc.body), tc.persists)
		t.Logf("%-42s injected=%-5v tool_in_body=%v", tc.name, injected,
			strings.Contains(string(out), ToolName))
	}
}
