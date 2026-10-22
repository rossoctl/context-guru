package proxy

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

func liveHeapMiB() float64 {
	runtime.GC()
	runtime.GC()
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	return float64(s.HeapInuse) / (1 << 20)
}

// A RETAINED Meta MUST NOT PIN ITS REQUEST BODY.
//
// metaFromBody reads a handful of short strings (thinking.type, output_config.effort,
// reasoning_effort, tool_choice) out of the WHOLE inbound body with gjson, whose String() returns
// a substring of that body without copying. The event carrying the Meta outlives the request: the
// SSE hub keeps the last 256 events for late subscribers. So every kept event held its request's
// entire body alive — on CG2, 1-20 MB bodies from 1,000+ message sessions, 256 of them, past the
// 8 GiB cgroup limit: four OOM kills on 2026-10-19/20.
func TestMetaDoesNotPinTheRequestBody(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 8<<20)
	head := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},` +
		`"tool_choice":{"type":"auto"},"messages":"`)
	base := liveHeapMiB()
	kept := make([]dash.Meta, 0, 32)
	for i := 0; i < 32; i++ {
		body := append(append(append([]byte(nil), head...), big...), '"', '}')
		kept = append(kept, metaFromBody(body))
	}
	retained := liveHeapMiB() - base
	if kept[0].ThinkingMode != "adaptive" || kept[0].ReasoningEffort != "high" || kept[0].ToolChoice != "auto" {
		t.Fatalf("meta = %+v: the fields were not read", kept[0])
	}
	// 32 x 8 MiB = 256 MiB if each Meta pins its body; a handful of short strings otherwise.
	if retained > 16 {
		t.Errorf("32 retained Metas keep %.0f MiB alive; each one is pinning its whole request body", retained)
	}
	runtime.KeepAlive(kept)
}
