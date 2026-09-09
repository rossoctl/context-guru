package all_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// countingModel is a stubModel that records how many times it was called, so a
// test can prove a second turn reused prior state instead of re-calling the LLM.
type countingModel struct {
	resp  string
	mu    sync.Mutex
	calls int
}

func (m *countingModel) Complete(context.Context, string) (string, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return m.resp, nil
}

func sysMsg(s string) bschemas.ChatMessage {
	t := s
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleSystem, Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

// TestSummarizeReusesCheckpoint: once a summary exists, a later turn whose covered
// prefix is unchanged and whose new tail is small must REUSE the prior summary —
// no second model call, and the summary message byte-identical (KV-cache stable).
func TestSummarizeReusesCheckpoint(t *testing.T) {
	off := newComp(t, "summarize", "keep_last: 1\nstart_from_message: 0\nmin_tokens: 1\ntrigger: {min_request_frac: 0}\nresummarize_tokens: 100000\n")
	st := store.NewMemory(store.Options{})
	cm := &countingModel{resp: "essential facts"}
	tool := toolMsg(strings.Repeat("verbose tool output line\n", 40))
	base := func() []bschemas.ChatMessage {
		return []bschemas.ChatMessage{sysMsg("you are helpful"), tool, userMsg("continue")}
	}
	run := func(msgs []bschemas.ChatMessage) *bschemas.BifrostChatRequest {
		req := &bschemas.BifrostChatRequest{Input: msgs}
		c := &components.Ctx{Ctx: context.Background(), Session: "sess1", Store: st,
			Model: components.ModelSpec{Incoming: cm}}
		var rep components.Report
		if _, err := off.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return req
	}

	req1 := run(base())
	if cm.calls != 1 {
		t.Fatalf("turn 1 must summarize once, calls=%d", cm.calls)
	}
	sum1 := schema.MessageText(req1.Input[1])

	// Turn 2: the client re-sends the full original transcript plus new messages.
	turn2 := append(base(), userMsg("next question"), toolMsg("small follow-up output"))
	req2 := run(turn2)
	if cm.calls != 1 {
		t.Fatalf("turn 2 must REUSE the summary (no new model call), calls=%d", cm.calls)
	}
	if got := schema.MessageText(req2.Input[1]); got != sum1 {
		t.Fatalf("reused summary must be byte-identical:\n turn1=%q\n turn2=%q", sum1, got)
	}
	// The new tail is retained verbatim after the summary.
	if schema.MessageText(req2.Input[len(req2.Input)-1]) != "small follow-up output" {
		t.Fatal("newly appended tail must be kept verbatim on reuse")
	}
}

// TestExtractReusesResultCache: the same large tool output re-sent on a later turn
// reuses the prior compaction — no second model call — and is still reduced.
func TestExtractReusesResultCache(t *testing.T) {
	// economic_gate: false — this is a MECHANISM test (does the model-written filter
	// run and reduce?), and its small fixture output is genuinely uneconomic, so the
	// #28 gate would correctly suppress the call. Gate economics are tested in
	// components/offload/extract_econ_test.go against the dollar figures directly.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	// Records padded so dropping the non-keep record shrinks the output by far more
	// than the recovery marker costs (marker-inclusive never-worse guard, D1).
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"},{"id":3,"name":"keep that ` + pad + `"}]`
	run := func() *bschemas.BifrostChatRequest {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("find the keep records"), toolMsg(body),
		}}
		c := &components.Ctx{Ctx: context.Background(), Session: "sX", Store: st,
			Model: components.ModelSpec{Static: cm}}
		var rep components.Report
		if _, err := off.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return req
	}

	req1 := run()
	if cm.calls != 1 {
		t.Fatalf("turn 1 must call the model once, calls=%d", cm.calls)
	}
	if strings.Contains(schema.MessageText(req1.Input[1]), "drop this") {
		t.Fatal("turn 1 should have reduced the output")
	}

	req2 := run()
	if cm.calls != 1 {
		t.Fatalf("turn 2 must REUSE the cached extraction (no new model call), calls=%d", cm.calls)
	}
	if strings.Contains(schema.MessageText(req2.Input[1]), "drop this") {
		t.Fatal("turn 2 reused reduction must still drop the non-keep record")
	}
}
