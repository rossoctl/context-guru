package proxy

import (
	"reflect"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/schema"
)

// scanLifecycleOld is scanLifecycle as it was before the normalization was shared: it
// normalized the body itself. Kept here as the reference the new path must equal.
func scanLifecycleOld(provider bschemas.ModelProvider, body []byte) (q dash.LifecycleReq) {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return q
	}
	msgs := apply.NormalizeMessages(provider, body)
	last := -1
	for i, m := range msgs {
		if m.Role == bschemas.ChatMessageRoleAssistant {
			last = i
		}
	}
	for _, m := range msgs[last+1:] {
		if m.ChatToolMessage != nil {
			q.ResultCount++
			q.ResultBytes += len(schema.MessageText(m))
		}
	}
	gjson.GetBytes(body, "messages.@reverse.0.content").ForEach(func(_, b gjson.Result) bool {
		if b.IsObject() && b.Get("type").String() == "tool_result" && b.Get("is_error").Bool() {
			q.ResultError = true
		}
		return !q.ResultError
	})
	return q
}

// Sharing one normalization between waitcause and the lifecycle scan changes nothing: the
// lifecycle facts and the wait-cause observation equal what each computed on its own.
func TestSharedNormalizationIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider bschemas.ModelProvider
		body     string
	}{
		{"anthropic", bschemas.Anthropic, `{"messages":[{"role":"user","content":"go"},
			{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{}},{"type":"tool_use","id":"2","name":"Read","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"0123456789","is_error":true},{"type":"tool_result","tool_use_id":"2","content":[{"type":"text","text":"abc"}]},{"type":"text","text":"more"}]}]}`},
		{"openai chat", bschemas.OpenAI, `{"model":"gpt","messages":[{"role":"user","content":"go"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"sh","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"hello world"}]}`},
		{"responses", bschemas.OpenAI, `{"model":"gpt","input":[{"role":"user","content":"go"},
			{"type":"function_call","call_id":"c1","name":"sh","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"result text"}]}`},
		{"previous_response_id", bschemas.OpenAI, `{"model":"gpt","previous_response_id":"resp_1","input":[
			{"type":"function_call_output","call_id":"c1","output":"result text"}]}`},
		{"no tool results", bschemas.Anthropic, `{"messages":[{"role":"user","content":"hi"}]}`},
		{"garbage", bschemas.Anthropic, `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			norm := func() []bschemas.ChatMessage { return apply.NormalizeMessages(tc.provider, body) }
			if got, want := scanLifecycle(norm, body), scanLifecycleOld(tc.provider, body); got != want {
				t.Errorf("lifecycle: shared %+v, old %+v", got, want)
			}
			calls := 0
			shared := func() []bschemas.ChatMessage { calls++; return norm() }
			got := waitcause.New().ObserveWith("t", "s", "th", body, shared)
			want := waitcause.New().Observe("t", "s", "th", tc.provider, body)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("waitcause: shared %+v, old %+v", got, want)
			}
			if calls > 1 {
				t.Errorf("norm called %d times by one Observe", calls)
			}
		})
	}
}
