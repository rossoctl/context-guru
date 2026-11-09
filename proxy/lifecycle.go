package proxy

import (
	"github.com/tidwall/gjson"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/schema"
)

// scanLifecycle reads the tool_results a request answers, from the same normalized
// conversation the pipeline and waitcause use: every role=tool message after the last
// assistant turn. Count and bytes come from there; is_error is the one thing the normalizer
// drops, so it is read straight off the last Anthropic user message. Runs on the dashboard
// writer goroutine, never the request path.
// norm yields the normalized conversation (shared with waitcause so the body is normalized
// once per request).
func scanLifecycle(norm func() []bschemas.ChatMessage, body []byte) (q dash.LifecycleReq) {
	defer func() { recover() }() //nolint:errcheck // fail open: a body it cannot read yields no facts
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return q // server-held history: `input` is only the newest turn
	}
	msgs := norm()
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
