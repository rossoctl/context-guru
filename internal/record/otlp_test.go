package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraceIDForUUIDKeepsTheSessionID(t *testing.T) {
	const uuid = "6D1F2A44-9e3b-4c7a-8f21-0b5d7c9e1a33"
	if got := TraceIDFor(uuid); got != "6d1f2a449e3b4c7a8f210b5d7c9e1a33" {
		t.Errorf("TraceIDFor(%q) = %q, want the UUID without dashes, lower case", uuid, got)
	}
}

func TestTraceIDForFallbackIsValidAndStable(t *testing.T) {
	for _, key := range []string{"cg-derived-t1:abc", "cg-unkeyed-0a1b2c3d", "x"} {
		a, b := TraceIDFor(key), TraceIDFor(key)
		if a != b {
			t.Errorf("%q: not stable: %q then %q", key, a, b)
		}
		if len(a) != 32 || !isHex(a) {
			t.Errorf("%q: %q is not 32 hex characters", key, a)
		}
	}
	if TraceIDFor("cg-derived-a") == TraceIDFor("cg-derived-b") {
		t.Error("two different session keys map to one trace id")
	}
}

// OTLP and the translated form differ only in how a tool_call's arguments are encoded.
func TestOTLPMessagesMakeArgumentsJSON(t *testing.T) {
	corpus := `[{"role":"assistant","finish_reason":"tool_use","parts":[{"type":"text","content":"ok"},` +
		`{"type":"tool_call","id":"toolu_1","name":"Bash","arguments":"{\"command\":\"ls\"}"}]}]`
	otlp := OTLPMessages(corpus)
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(otlp), &msgs); err != nil {
		t.Fatalf("OTLP messages are not valid JSON: %v", err)
	}
	args := msgs[0]["parts"].([]any)[1].(map[string]any)["arguments"]
	if _, isObject := args.(map[string]any); !isObject {
		t.Errorf("OTLP tool_call arguments = %#v, want a JSON object", args)
	}
}

func TestMessagesWithoutToolCallsAreUnchanged(t *testing.T) {
	corpus := `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`
	if got := OTLPMessages(corpus); got != corpus {
		t.Errorf("changed messages with no tool call: %s", got)
	}
	if got := OTLPMessages("not json"); got != "not json" {
		t.Errorf("malformed input not passed through: %s", got)
	}
}

func TestToOTLPAddsSessionProviderAndOpenInference(t *testing.T) {
	tr := Turn{
		SessionID: "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33", SpanID: "0123456789abcdef",
		Model: "claude-opus-5", System: "anthropic",
		Start: time.Unix(100, 0), End: time.Unix(101, 0),
		RequestBody:  `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`,
		ResponseBody: `[{"role":"assistant","finish_reason":"end_turn","parts":[{"type":"text","content":"hello"}]}]`,
		FreshInput:   10, CacheRead: 100, CacheWrite: 5, Output: 3,
	}
	sp := tr.ToOTLP()
	if sp.TraceID != "6d1f2a449e3b4c7a8f210b5d7c9e1a33" {
		t.Errorf("trace id = %q", sp.TraceID)
	}
	want := map[string]any{
		AttrSessionID:        tr.SessionID,
		AttrProviderName:     "anthropic",
		AttrSystem:           "anthropic", // deprecated name kept for one release
		AttrOISpanKind:       OISpanKindLLM,
		AttrOIModelName:      "claude-opus-5",
		AttrOITokenPrompt:    115,
		AttrOITokenComplete:  3,
		AttrOITokenTotal:     118,
		AttrOITokenCacheRead: 100,
		AttrOITokenCacheNew:  5,
	}
	for k, v := range want {
		if sp.Attributes[k] != v {
			t.Errorf("%s = %#v, want %#v", k, sp.Attributes[k], v)
		}
	}
	if v, _ := sp.Attributes[AttrOIInputValue].(string); !strings.Contains(v, "hi") {
		t.Errorf("input.value missing the request messages: %q", v)
	}
}
