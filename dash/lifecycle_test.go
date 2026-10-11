package dash

import (
	"path/filepath"
	"testing"
	"time"
)

const anthropicToolTurn = `{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cch=NONCE"},{"type":"text","text":"sys"}],
"tools":[{"name":"Bash"}],"messages":[
{"role":"user","content":"fix it"},
{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"SECRET-INPUT"}},{"type":"tool_use","id":"2","name":"mcp__srv__read","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"0123456789","is_error":true},{"type":"tool_result","tool_use_id":"2","content":"abc"}]}]}`

func TestScanLifecycleAnthropic(t *testing.T) {
	q := ScanLifecycle([]byte(anthropicToolTurn))
	if q.Messages != 3 || q.PrevToolCount != 2 || q.PrevToolNames != "Bash,mcp__srv__read" {
		t.Errorf("tool_use side wrong: %+v", q)
	}
	if q.ResultCount != 2 || q.ResultBytes != len(`"0123456789"`)+len(`"abc"`) || !q.ResultError {
		t.Errorf("tool_result side wrong: %+v", q)
	}
}

func TestScanLifecycleOpenAI(t *testing.T) {
	q := ScanLifecycle([]byte(`{"messages":[{"role":"user","content":"x"},
	{"role":"assistant","tool_calls":[{"function":{"name":"grep","arguments":"{}"}}]},
	{"role":"tool","content":"hello"}]}`))
	if q.PrevToolNames != "grep" || q.PrevToolCount != 1 || q.ResultCount != 1 || q.ResultBytes != 7 {
		t.Errorf("%+v", q)
	}
}

// Client-supplied names pass through metaEnum; a credential-shaped one is replaced, and the
// tool INPUT never reaches the row.
func TestScanLifecycleRedactsNamesAndDropsInputs(t *testing.T) {
	q := ScanLifecycle([]byte(`{"messages":[{"role":"assistant","content":[
	{"type":"tool_use","name":"sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA","input":{"k":"SECRET-INPUT"}}]}]}`))
	if q.PrevToolNames != Redacted {
		t.Errorf("name = %q, want %q", q.PrevToolNames, Redacted)
	}
}

func TestThreadIDIgnoresBillingNonce(t *testing.T) {
	a := ScanLifecycle([]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cch=1"},{"type":"text","text":"s"}]}`))
	b := ScanLifecycle([]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cch=2"},{"type":"text","text":"s"}]}`))
	c := ScanLifecycle([]byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cch=2"},{"type":"text","text":"other"}]}`))
	if a.SystemHash != b.SystemHash || a.SystemHash == c.SystemHash {
		t.Error("system hash must ignore the billing block and track the rest")
	}
}

func TestObserveLifecycleThreadsGapsAndTTL(t *testing.T) {
	r := &Recorder{life: newLifecycle()}
	main := LifecycleReq{ToolsHash: 1, SystemHash: 1, Messages: 3, PrevToolNames: "Bash", PrevToolCount: 1, ResultCount: 1, ResultBytes: 50}
	sub := LifecycleReq{ToolsHash: 2, SystemHash: 2, Messages: 1}

	r1 := r.ObserveLifecycle("t", "s", "m", 1000, 1400, main, "tool_use", 0, 0)
	if r1.ThreadSeq != 1 || r1.GapMs != -1 || r1.SincePrevEndMs != -1 || r1.ParentThreadID != "" {
		t.Errorf("first request: %+v", r1)
	}
	main.Messages = 5
	r2 := r.ObserveLifecycle("t", "s", "m", 61000, 62000, main, "end_turn", 100, 0)
	if r2.ThreadID != r1.ThreadID || r2.ThreadSeq != 2 || r2.GapMs != 60000 ||
		r2.SincePrevEndMs != 59600 || r2.PrevStopReason != "tool_use" || r2.GrantedTTL != "5m" {
		t.Errorf("second request: %+v", r2)
	}
	// A subagent: other tools/system -> its own thread, parent hint = the session's first thread.
	rs := r.ObserveLifecycle("t", "s", "m", 63000, 64000, sub, "end_turn", 0, 200)
	if rs.ThreadID == r1.ThreadID || rs.ParentThreadID != r1.ThreadID || rs.GapMs != -1 || rs.GrantedTTL != "1h" {
		t.Errorf("subagent: %+v", rs)
	}
	// Main resumes: its gap is measured against ITS previous request, not the subagent's.
	main.Messages = 7
	r3 := r.ObserveLifecycle("t", "s", "m", 70000, 71000, main, "end_turn", 5, 5)
	if r3.ThreadID != r1.ThreadID || r3.GapMs != 9000 || r3.GrantedTTL != "mixed" {
		t.Errorf("main after subagent: %+v", r3)
	}
	// Messages falling below the thread's last count is a different cache entry.
	main.Messages = 2
	r4 := r.ObserveLifecycle("t", "s", "m", 80000, 81000, main, "end_turn", 0, 0)
	if r4.ThreadID == r1.ThreadID || r4.ThreadSeq != 1 || r4.GapMs != -1 {
		t.Errorf("message-count drop must start a new thread: %+v", r4)
	}
	// Tenants never share a thread id.
	if o := r.ObserveLifecycle("u", "s", "m", 90000, 91000, main, "", 0, 0); o.ThreadID == r4.ThreadID {
		t.Error("tenant not part of the thread id")
	}
	// A ping row links to the session's latest thread on that model.
	if p := r.PingLifecycle("t", "s", "m", 2); p.ThreadID != r4.ThreadID || p.PingN != 2 {
		t.Errorf("ping link: %+v", p)
	}
}

func TestLifecycleOffRecordsNothing(t *testing.T) {
	r := &Recorder{}
	if r.LifecycleOn() || r.ObserveLifecycle("t", "s", "m", 1, 2, LifecycleReq{}, "", 0, 0) != nil || r.PingLifecycle("t", "s", "m", 1) != nil {
		t.Error("flag off must be inert")
	}
}

func TestLifecycleRoundTripThroughStore(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ev := mkEvent(time.Now().UnixMilli(), "s", "m", 100, 90)
	ev.Lifecycle = &LifecycleRow{ThreadID: "abc", ThreadSeq: 2, PrevStopReason: "tool_use", PrevToolNames: "Bash",
		PrevToolCount: 1, ResultCount: 1, ResultBytes: 9, ResultError: true, GapMs: 1234, SincePrevEndMs: 1000, GrantedTTL: "1h"}
	ping := mkEvent(ev.TS+1, "s", "m", 100, 100)
	ping.KeepAlive = true
	ping.Lifecycle = &LifecycleRow{ThreadID: "abc", GapMs: -1, SincePrevEndMs: -1, PingN: 1}
	plain := mkEvent(ev.TS+2, "s", "m", 100, 90) // flag off: no lifecycle row
	if err := db.insertBatch([]*Event{ev, ping, plain}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Lifecycles()
	if err != nil || len(got) != 2 {
		t.Fatalf("rows=%d err=%v", len(got), err)
	}
	if got[0] != *ev.Lifecycle || got[1] != *ping.Lifecycle {
		t.Errorf("round trip changed the rows:\n%+v\n%+v", got[0], got[1])
	}
}
