package proxy

import (
	"fmt"
	"github.com/tidwall/gjson"
	"strings"
)

// Stream-chunk builders for the end-to-end tests. inband/sse_*_test.go holds the same builders for the unit tests.

func ccChunk(id, delta, finish string) string {
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","model":"gpt-5.5","choices":[{"index":0,"delta":` +
		delta + `,"finish_reason":` + f + `}]}` + "\n\n"
}

func ccRole(id string) string { return ccChunk(id, `{"role":"assistant","content":""}`, "") }
func ccText(id, s string) string {
	return ccChunk(id, `{"content":"`+s+`"}`, "")
}
func ccReason(id, s string) string { return ccChunk(id, `{"reasoning_content":"`+s+`"}`, "") }
func ccCallStart(id string, idx int, callID, name string) string {
	return ccChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}`,
		idx, callID, name), "")
}
func ccCallArgs(id string, idx int, args string) string {
	return ccChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}`, idx, args), "")
}
func ccFinish(id, why string) string { return ccChunk(id, `{}`, why) }
func ccUsage(id string) string {
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}` + "\n\n"
}

const ccDone = "data: [DONE]\n\n"

func rsEv(typ, body string) string {
	return "event: " + typ + "\ndata: {\"type\":\"" + typ + "\"," + body + "}\n\n"
}

func rsResp(id, status, output string) string {
	return `"response":{"id":"` + id + `","object":"response","status":"` + status + `","output":` + output +
		`,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
}

func rsCreated(id string) string {
	return rsEv("response.created", `"sequence_number":0,`+rsResp(id, "in_progress", "[]"))
}

func rsMsgItem(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q}]}`, id, text)
}

func rsText(idx int, id, text string) string {
	return rsEv("response.output_item.added", fmt.Sprintf(`"sequence_number":1,"output_index":%d,"item":{"id":%q,"type":"message","status":"in_progress","role":"assistant","content":[]}`, idx, id)) +
		rsEv("response.output_text.delta", fmt.Sprintf(`"sequence_number":2,"output_index":%d,"item_id":%q,"content_index":0,"delta":%q`, idx, id, text)) +
		rsEv("response.output_item.done", fmt.Sprintf(`"sequence_number":3,"output_index":%d,"item":%s`, idx, rsMsgItem(id, text)))
}

func rsCallItem(id, name string) string {
	return fmt.Sprintf(`{"id":"fc_%s","type":"function_call","status":"completed","call_id":%q,"name":%q,"arguments":"{}"}`, id, id, name)
}

func rsCall(idx int, id, name string) string {
	return rsEv("response.output_item.added", fmt.Sprintf(`"sequence_number":4,"output_index":%d,"item":{"id":"fc_%s","type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":""}`, idx, id, id, name)) +
		rsEv("response.function_call_arguments.done", fmt.Sprintf(`"sequence_number":5,"output_index":%d,"item_id":"fc_%s","arguments":"{}"`, idx, id)) +
		rsEv("response.output_item.done", fmt.Sprintf(`"sequence_number":6,"output_index":%d,"item":%s`, idx, rsCallItem(id, name)))
}

func rsCompleted(id string, items ...string) string {
	return rsEv("response.completed", `"sequence_number":9,`+rsResp(id, "completed", "["+strings.Join(items, ",")+"]"))
}

// rsDatas parses the data payload of each event in a Responses stream.
func rsDatas(out string) []gjson.Result {
	var ds []gjson.Result
	for _, blk := range strings.Split(out, "\n\n") {
		for _, line := range strings.Split(blk, "\n") {
			if p, ok := strings.CutPrefix(line, "data:"); ok {
				if p = strings.TrimSpace(p); p != "" && p != "[DONE]" {
					ds = append(ds, gjson.Parse(p))
				}
				break
			}
		}
	}
	return ds
}
