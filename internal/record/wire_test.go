package record

import (
	"encoding/json"
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

// The keys the Inference-Perf loader reads, with the types it expects.
func TestWireLineHasTheLoadersFields(t *testing.T) {
	start := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	b, err := Wire(WireCall{
		SessionID: "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33", Path: "/v1/messages",
		Request:  []byte(`{"model":"claude-opus-5","stream":true,"messages":[]}`),
		Response: []byte(sse("hi")), ResponseSSE: true,
		Status: 200, Start: start, End: start.Add(1500 * time.Millisecond),
		FirstByteMS: 120, Rounds: 1,
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
	h, _ := m["headers"].(map[string]any)
	if h["session_id"] != "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33" {
		t.Errorf("headers.session_id = %v; the loader groups a session by it", h)
	}
	if m["cg_first_byte_ms"] != float64(120) || m["cg_recorded_by"] != WireRecordedBy {
		t.Errorf("cg_* keys missing: %v", m)
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

// Only the session id goes in headers. A credential header can never be written, because the
// line has no way to carry one.
func TestWireWritesNoCredentialHeader(t *testing.T) {
	b, _ := Wire(WireCall{SessionID: "s", Path: "/v1/messages", Request: []byte(`{}`),
		Response: []byte(`{}`), Status: 200, Start: time.Now(), End: time.Now()}, 0)
	h, _ := wireLineOf(t, b)["headers"].(map[string]any)
	if len(h) != 1 {
		t.Errorf("headers = %v, want only session_id", h)
	}
}

// A body cut at the cap says so.
func TestWireMarksACappedBody(t *testing.T) {
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 500) + `"}]}`
	b, _ := Wire(WireCall{Path: "/v1/messages", Request: []byte(big), Response: []byte(`{}`),
		Status: 200, Start: time.Now(), End: time.Now()}, 100)
	m := wireLineOf(t, b)
	if r := m["request"].(string); len(r) > 100 {
		t.Errorf("request not capped: %d bytes", len(r))
	}
	if m["cg_incomplete"] != IncompleteBodyCap {
		t.Errorf("cg_incomplete = %v, want %s", m["cg_incomplete"], IncompleteBodyCap)
	}
}
