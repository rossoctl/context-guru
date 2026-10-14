package proxy

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func TestScanLifecycle(t *testing.T) {
	q := scanLifecycle(bschemas.Anthropic, []byte(`{"messages":[
	{"role":"user","content":"fix it"},
	{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{"c":"SECRET"}},{"type":"tool_use","id":"2","name":"Read","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"0123456789","is_error":true},{"type":"tool_result","tool_use_id":"2","content":"abc"},{"type":"text","text":"also this"}]}]}`))
	if q.ResultCount != 2 || q.ResultBytes != 13 || !q.ResultError {
		t.Errorf("%+v", q)
	}
	// Results answered earlier in the transcript are not counted again.
	q = scanLifecycle(bschemas.Anthropic, []byte(`{"messages":[
	{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"x","is_error":true}]},
	{"role":"assistant","content":[{"type":"text","text":"done"}]},
	{"role":"user","content":"thanks"}]}`))
	if q.ResultCount != 0 || q.ResultError {
		t.Errorf("stale results counted: %+v", q)
	}
	if q = scanLifecycle(bschemas.Anthropic, []byte(`not json`)); q.ResultCount != 0 {
		t.Errorf("garbage: %+v", q)
	}
}
