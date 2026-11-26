package record

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func wireLineOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if len(b) == 0 || b[len(b)-1] != '\n' || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("not exactly one JSONL line: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	return m
}

// The keys the Inference-Perf loader reads, with the types it expects, and nothing else.
func TestWireLineHasTheLoadersFieldsOnly(t *testing.T) {
	start := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	b, err := Wire(WireCall{
		Path:     "/v1/messages",
		Headers:  http.Header{"Anthropic-Version": {"2023-06-01"}},
		Request:  []byte(`{"model":"claude-opus-5","stream":true,"messages":[]}`),
		Response: []byte(sse("hi")), ResponseSSE: true,
		Status: 200, Start: start, End: start.Add(1500 * time.Millisecond),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := wireLineOf(t, b)
	if m["path"] != "/v1/messages" {
		t.Errorf("path = %v", m["path"])
	}
	if _, ok := m["request"].(string); !ok {
		t.Errorf("request must be a JSON string, got %T", m["request"])
	}
	if r, _ := m["response"].(string); !strings.Contains(r, "message_stop") {
		t.Errorf("response must be the SSE text, with its message_stop: %q", r)
	}
	if m["status"] != float64(200) || m["model"] != "claude-opus-5" {
		t.Errorf("status/model = %v/%v", m["status"], m["model"])
	}
	if m["start_unix"] != float64(start.Unix()) || m["total_ms"] != 1500.0 {
		t.Errorf("timing = %v / %v", m["start_unix"], m["total_ms"])
	}
	allowed := map[string]bool{"path": true, "request": true, "response": true, "status": true,
		"start_unix": true, "total_ms": true, "model": true, "headers": true}
	for k := range m {
		if !allowed[k] {
			t.Errorf("key %q is not part of the wire format; the line must hold only what was sent", k)
		}
	}
}

// Both bodies are redacted, and a secret split across two streamed events is caught.
func TestWireRedactsBothBodies(t *testing.T) {
	key := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB"
	req := `{"model":"m","messages":[{"role":"user","content":"my key is ` + key + `"}]}`
	b, err := Wire(WireCall{Path: "/v1/messages", Request: []byte(req),
		Response: []byte(sse("echo "+key[:5], key[5:])), ResponseSSE: true, Status: 200,
		Start: time.Now(), End: time.Now()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, key) || strings.Contains(s, key[5:]) {
		t.Errorf("a key survived in the wire line:\n%s", s)
	}
	m := wireLineOf(t, b)
	if !json.Valid([]byte(m["request"].(string))) {
		t.Error("the redacted request is no longer valid JSON")
	}
}

// The client's headers are recorded as sent, except any that carries a credential. A credential
// in a header nobody expected is still caught, by redacting every value.
func TestWireHeadersDropCredentials(t *testing.T) {
	key := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB"
	h := http.Header{
		"Authorization":            {"Bearer " + key},
		"X-Api-Key":                {key},
		"Cookie":                   {"session=abc"},
		"X-Context-Guru-Token":     {"cg_live_abcdefghijklmnopqrstuvwxyz"},
		"X-Some-Access-Token":      {"opaque"},
		"Anthropic-Version":        {"2023-06-01"},
		"X-Claude-Code-Session-Id": {"6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33"},
		"X-Debug-Note":             {"pasted " + key},
	}
	b, _ := Wire(WireCall{Path: "/v1/messages", Headers: h, Request: []byte(`{}`),
		Response: []byte(`{}`), Status: 200, Start: time.Now(), End: time.Now()}, 0)
	got, _ := wireLineOf(t, b)["headers"].(map[string]any)
	for _, gone := range []string{"authorization", "x-api-key", "cookie", "x-context-guru-token", "x-some-access-token"} {
		if _, ok := got[gone]; ok {
			t.Errorf("credential header %q was written", gone)
		}
	}
	if got["anthropic-version"] != "2023-06-01" || got["x-claude-code-session-id"] == nil {
		t.Errorf("ordinary headers missing: %v", got)
	}
	if strings.Contains(string(b), key) {
		t.Errorf("a key survived in a header value:\n%s", b)
	}
}

// A body longer than the cap is cut.
func TestWireCapsABody(t *testing.T) {
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 500) + `"}]}`
	b, _ := Wire(WireCall{Path: "/v1/messages", Request: []byte(big), Response: []byte(`{}`),
		Status: 200, Start: time.Now(), End: time.Now()}, 100)
	if r := wireLineOf(t, b)["request"].(string); len(r) > 100 {
		t.Errorf("request not capped: %d bytes", len(r))
	}
}
