package expand

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsesExpandWireAdapter(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"find <<cg:HASH>>"}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	injected, ok := Inject("responses", InjectAuto, body, true)
	if !ok || !HasTool("responses", injected) || gjson.GetBytes(injected, "tools.1.name").String() != ToolName ||
		gjson.GetBytes(injected, "tools.1.parameters.type").String() != "object" {
		t.Fatalf("Responses tool was not injected in native shape: %s", injected)
	}
	if again, ok := Inject("responses", InjectAuto, injected, true); ok || string(again) != string(injected) {
		t.Fatal("Responses injection was not idempotent")
	}
	resp := []byte(`{"output":[{"type":"reasoning","encrypted_content":"opaque"},` +
		`{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}]}`)
	calls, other := ResponseCalls("responses", resp)
	if other || len(calls) != 1 || calls[0].CallID != "c1" || calls[0].HashID != "HASH" {
		t.Fatalf("Responses calls = %+v other=%v", calls, other)
	}
	next, ok := Continuation("responses", injected, resp, map[string]string{"c1": "original text"})
	if !ok || gjson.GetBytes(next, "input.1.encrypted_content").String() != "opaque" ||
		gjson.GetBytes(next, "input.3.type").String() != "function_call_output" ||
		gjson.GetBytes(next, "input.3.output").String() != "original text" {
		t.Fatalf("Responses continuation lost state or result: %s", next)
	}
}

func TestResponsesSSEAndRequestRepair(t *testing.T) {
	stream := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}]}}` + "\n\n")
	resp, ok := AggregateSSE("responses", stream)
	if !ok || gjson.GetBytes(resp, "output.0.call_id").String() != "c1" {
		t.Fatalf("Responses SSE terminal event was not recovered: %s", resp)
	}
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"No such tool available"}]}`)
	got, restored := RepairToolResults("responses", body, func(id string) (string, bool) {
		return "original text", id == "HASH"
	})
	if len(restored) != 1 || restored[0] != "original text" || gjson.GetBytes(got, "input.1.output").String() != "original text" {
		t.Fatalf("Responses repair failed: %s restored=%v", got, restored)
	}
	if !HasMarkersInMessages([]byte(`{"input":[{"role":"user","content":"` + Marker("HASH") + `"}]}`)) {
		t.Fatal("Responses marker was not found")
	}
	if strings.Contains(string(ToolDefRaw("responses")), `"function":`) {
		t.Fatal("Responses definition used Chat Completions shape")
	}
}

func TestResponsesRepairIsIdempotentAndAvoidsDuplicateContent(t *testing.T) {
	resolve := func(id string) (string, bool) { return "original text", id == "HASH" }
	body := []byte(`{"input":[{"role":"user","content":"original text"},` +
		`{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"No such tool"}]}`)
	first, restored := RepairToolResults("responses", body, resolve)
	if len(restored) != 1 || gjson.GetBytes(first, "input.2.output").String() != RestoredInPlace("HASH") {
		t.Fatalf("repair duplicated content already in input: %s restored=%v", first, restored)
	}
	second, _ := RepairToolResults("responses", first, resolve)
	if string(second) != string(first) {
		t.Fatalf("second repair changed the body: first=%s second=%s", first, second)
	}
	withoutOriginal := []byte(strings.Replace(string(body), `{"role":"user","content":"original text"},`, ``, 1))
	full, _ := RepairToolResults("responses", withoutOriginal, resolve)
	if gjson.GetBytes(full, "input.1.output").String() != "original text" {
		t.Fatalf("repair pointed at absent content: %s", full)
	}
	imageSpan := `[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]`
	imageBody := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},` +
		`{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"No such tool"}]}`)
	imageRepaired, _ := RepairToolResults("responses", imageBody, func(id string) (string, bool) {
		return imageSpan, id == "HASH"
	})
	if gjson.GetBytes(imageRepaired, "input.2.output").String() != RestoredInPlace("HASH") {
		t.Fatalf("native image span was duplicated in the tool output: %s", imageRepaired)
	}
}

func TestResponsesRepairKeepsEarlierSuccessWhenLaterWriteFails(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"H1\"}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"not found"},` +
		`{"type":"function_call","call_id":"c2","name":"context_guru_expand","arguments":"{\"id\":\"H2\"}"},` +
		`{"type":"function_call_output","call_id":"c2","output":"not found"}]}`)
	set := func(b []byte, path string, value interface{}) ([]byte, error) {
		if path == "input.3.output" {
			return nil, errors.New("simulated malformed second item")
		}
		return sjson.SetBytes(b, path, value)
	}
	got, restored := repairResponsesToolResultsWithSet(body, func(id string) (string, bool) {
		return id, true
	}, set)
	if gjson.GetBytes(got, "input.1.output").String() != "H1" ||
		gjson.GetBytes(got, "input.3.output").String() != "not found" ||
		len(restored) != 1 || restored[0] != "H1" {
		t.Fatalf("failed second repair rolled back first: %s restored=%v", got, restored)
	}
}

func TestResponsesSummaryExpandRestoresImageAsImage(t *testing.T) {
	req := []byte(`{"input":[{"role":"user","content":"=== History Summary === old image <<cg:HASH>>"},` +
		`{"role":"user","content":"what was in it?"}]}`)
	resp := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}]}`)
	stash := `[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},` +
		`{"role":"assistant","content":"I saw a diagram"}]`
	next, ok := Continuation("responses", req, resp, map[string]string{"c1": stash})
	if !ok || gjson.GetBytes(next, "input.0.content.0.type").String() != "input_image" ||
		gjson.GetBytes(next, "input.1.content").String() != "I saw a diagram" ||
		gjson.GetBytes(next, "input.2.content").String() != "what was in it?" ||
		gjson.GetBytes(next, "input.4.output").String() != "[expand: original history restored in place above]" {
		t.Fatalf("multimodal original was not restored in its native place: %s", next)
	}
}

// #727 review, round 3. A cache_aware_summarizer stash holds NORMALIZED chat messages. When its span
// held a tool exchange, those are a role=tool message and an assistant message with tool_calls,
// which are not Responses input items: restored in place they make the Responses API return 400.
// Such a stash must be answered as the flattened string, and the request must stay valid.
func TestResponsesExpandOfACacheAwareToolStashStaysValid(t *testing.T) {
	summary := "=== Compacted Context (cache-aware summary) ===\nfacts\n" + Marker("HASH")
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"task"},{"role":"user","content":` +
		jsonString(summary) + `},{"role":"user","content":"continue"}],"tools":[{"type":"function","name":"` + ToolName + `","parameters":{"type":"object"}}]}`)
	resp := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"` + ToolName + `","arguments":"{\"id\":\"HASH\"}"}]}`)
	stash := `[{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"t1","content":"tool output"}]`
	next, ok := Continuation("responses", body, resp, map[string]string{"c1": stash})
	if !ok {
		t.Fatal("continuation failed")
	}
	gjson.GetBytes(next, "input").ForEach(func(_, item gjson.Result) bool {
		if r := item.Get("role").String(); r == "tool" || item.Get("tool_calls").Exists() {
			t.Fatalf("a normalized tool message was spliced into the Responses input: %s", item.Raw)
		}
		return true
	})
	if got := gjson.GetBytes(next, "input.#(type==\"function_call_output\").output").String(); got != stash {
		t.Fatalf("the stash did not travel as the flattened answer: %q", got)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
