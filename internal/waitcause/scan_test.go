package waitcause

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

const scanBgBody = `{"model":"claude-sonnet-5-5","max_tokens":100,"messages":[
{"role":"user","content":"build"},
{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make","run_in_background":true}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"Command running in background with ID: bash1."}]}]}`

const scanPlainBody = `{"model":"claude-sonnet-5-5","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`

func TestScanBodyFindsOpenBackgroundWork(t *testing.T) {
	s := ScanBody(nil, bschemas.Anthropic, []byte(scanBgBody))
	if s == nil || s.OpenBackground != 1 || s.OpenTeammate != 0 || len(s.Messages()) != 3 {
		t.Fatalf("scan = %+v, want 1 background task and 3 messages", s)
	}
	if s := ScanBody(nil, bschemas.Anthropic, []byte(scanPlainBody)); s == nil || s.OpenBackground != 0 {
		t.Fatalf("plain body scan = %+v, want 0 open", s)
	}
}

// The scan is a pure function: it leaves the tracker's state alone.
func TestScanBodyWritesNoTrackerState(t *testing.T) {
	tr := New()
	ScanBody(nil, bschemas.Anthropic, []byte(scanBgBody))
	if bg, tm := tr.OpenCounts("t", "s", ""); bg != 0 || tm != 0 {
		t.Fatalf("tracker counts %d/%d after a scan, want 0/0", bg, tm)
	}
}

func TestScanBodyUnknownCases(t *testing.T) {
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", MaxScanBytes) + `"}]}`
	many := `{"messages":[` + strings.Repeat(`{"role":"user","content":"a"},`, MaxScanMessages) + `{"role":"user","content":"a"}]}`
	for name, body := range map[string]string{
		"empty":                  "",
		"over the byte bound":    big,
		"over the message bound": many,
		"no message list":        `{"model":"x"}`,
		"server-held history":    `{"previous_response_id":"resp_1","input":[{"role":"user","content":"a"}]}`,
		"not json":               `not json`,
	} {
		if s := ScanBody(nil, bschemas.Anthropic, []byte(body)); s != nil {
			t.Errorf("%s: scan = %+v, want unknown (nil)", name, s)
		}
	}
}

func TestScanBodyStops(t *testing.T) {
	if s := ScanBody(func() bool { return true }, bschemas.Anthropic, []byte(scanBgBody)); s != nil {
		t.Fatal("a stopped scan returned a result")
	}
	// Stop turns true after the normalization, inside the per-message loop.
	calls := 0
	s := ScanBody(func() bool { calls++; return calls > 2 }, bschemas.Anthropic, []byte(scanBgBody))
	if s != nil {
		t.Fatalf("a scan stopped mid-way returned %+v", s)
	}
}

// The deterministic bound: a stop that is polled once per message ends the scan after the message in
// progress, whatever the body size.
func TestScanBodyPollsStopPerMessage(t *testing.T) {
	body := `{"messages":[` + strings.Repeat(`{"role":"user","content":"a"},`, 999) + `{"role":"user","content":"a"}]}`
	polls := 0
	ScanBody(func() bool { polls++; return polls >= 10 }, bschemas.Anthropic, []byte(body))
	if polls != 10 {
		t.Fatalf("stop polled %d times, want 10: the scan went on after it turned true", polls)
	}
}

func TestScanBodyPanicFailsOpen(t *testing.T) {
	if s := ScanBody(func() bool { panic("boom") }, bschemas.Anthropic, []byte(scanBgBody)); s != nil {
		t.Fatalf("a panicking scan returned %+v, want nil", s)
	}
}
