// Package inbandtest holds the stream-chunk builders that tests of the in-band splicers share
// (inband's own tests and a host's end-to-end tests). Only tests import it.
package inbandtest

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

func CCChunk(id, delta, finish string) string {
	f := "null"
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","model":"gpt-5.5","choices":[{"index":0,"delta":` +
		delta + `,"finish_reason":` + f + `}]}` + "\n\n"
}

func CCRole(id string) string { return CCChunk(id, `{"role":"assistant","content":""}`, "") }
func CCText(id, s string) string {
	return CCChunk(id, `{"content":"`+s+`"}`, "")
}
func CCReason(id, s string) string { return CCChunk(id, `{"reasoning_content":"`+s+`"}`, "") }
func CCCallStart(id string, idx int, callID, name string) string {
	return CCChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}`,
		idx, callID, name), "")
}
func CCCallArgs(id string, idx int, args string) string {
	return CCChunk(id, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}`, idx, args), "")
}
func CCFinish(id, why string) string { return CCChunk(id, `{}`, why) }
func CCUsage(id string) string {
	return `data: {"id":"` + id + `","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}` + "\n\n"
}

const CCDone = "data: [DONE]\n\n"

func RSEv(typ, body string) string {
	return "event: " + typ + "\ndata: {\"type\":\"" + typ + "\"," + body + "}\n\n"
}

func RSResp(id, status, output string) string {
	return `"response":{"id":"` + id + `","object":"response","status":"` + status + `","output":` + output +
		`,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
}

func RSCreated(id string) string {
	return RSEv("response.created", `"sequence_number":0,`+RSResp(id, "in_progress", "[]"))
}

func RSMsgItem(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q}]}`, id, text)
}

func RSText(idx int, id, text string) string {
	return RSEv("response.output_item.added", fmt.Sprintf(`"sequence_number":1,"output_index":%d,"item":{"id":%q,"type":"message","status":"in_progress","role":"assistant","content":[]}`, idx, id)) +
		RSEv("response.output_text.delta", fmt.Sprintf(`"sequence_number":2,"output_index":%d,"item_id":%q,"content_index":0,"delta":%q`, idx, id, text)) +
		RSEv("response.output_item.done", fmt.Sprintf(`"sequence_number":3,"output_index":%d,"item":%s`, idx, RSMsgItem(id, text)))
}

func RSCallItem(id, name string) string {
	return fmt.Sprintf(`{"id":"fc_%s","type":"function_call","status":"completed","call_id":%q,"name":%q,"arguments":"{}"}`, id, id, name)
}

func RSCall(idx int, id, name string) string {
	return RSEv("response.output_item.added", fmt.Sprintf(`"sequence_number":4,"output_index":%d,"item":{"id":"fc_%s","type":"function_call","status":"in_progress","call_id":%q,"name":%q,"arguments":""}`, idx, id, id, name)) +
		RSEv("response.function_call_arguments.done", fmt.Sprintf(`"sequence_number":5,"output_index":%d,"item_id":"fc_%s","arguments":"{}"`, idx, id)) +
		RSEv("response.output_item.done", fmt.Sprintf(`"sequence_number":6,"output_index":%d,"item":%s`, idx, RSCallItem(id, name)))
}

func RSCompleted(id string, items ...string) string {
	return RSEv("response.completed", `"sequence_number":9,`+RSResp(id, "completed", "["+strings.Join(items, ",")+"]"))
}

// RSDatas parses the data payload of each event in a Responses stream.
func RSDatas(out string) []gjson.Result {
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
