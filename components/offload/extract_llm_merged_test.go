package offload

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// countingModel records how many times it was called and returns a canned adjudication.
type countingModel struct {
	calls   int64
	reply   string
	lastAsk string
}

func (m *countingModel) Complete(ctx context.Context, prompt string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	m.lastAsk = prompt
	return m.reply, nil
}

func mkToolMsg(id, text string) bschemas.ChatMessage {
	i := id
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		Content:         &bschemas.ChatMessageContent{ContentStr: &text},
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &i}}
}

func mergedReq(n int, body string) *bschemas.BifrostChatRequest {
	u := "find the failing test in src/auth.py"
	msgs := []bschemas.ChatMessage{{Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: &u}}}
	for i := 0; i < n; i++ {
		id := "call_" + string(rune('a'+i))
		nm := "Read"
		msgs = append(msgs,
			bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
				Content: &bschemas.ChatMessageContent{ContentStr: &nm},
				ChatAssistantMessage: &bschemas.ChatAssistantMessage{
					ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id,
						Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &nm}}}}},
			mkToolMsg(id, body))
	}
	tail := "keep going"
	msgs = append(msgs, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: &tail}})
	return &bschemas.BifrostChatRequest{Input: msgs}
}

// The merged shape's defining property: ONE model call for the whole batch. Per-candidate calls
// would be the per-output design that measured 6% live-kept, so this is a correctness property and
// not a performance nicety.
func TestMergedMakesExactlyOneModelCall(t *testing.T) {
	body := "[" + strings.TrimSuffix(strings.Repeat("{\"row\":\"value src/auth.py TOKEN_GRACE_41ab\"},", 400), ",") + "]"
	req := mergedReq(6, body)
	// Adjudicate: drop the first two tool outputs (message indices 2 and 4), keep the rest.
	vs := []extract.BulkVerdict{{Index: 2, Verdict: "drop"}, {Index: 4, Verdict: "drop"}}
	raw, _ := json.Marshal(vs)
	m := &countingModel{reply: string(raw)}

	e, err := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	off, ok := e.(components.Offload)
	if !ok {
		t.Fatal("not an Offload")
	}
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-1",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	before := schema.MessagesTokens(req)
	rep := &components.Report{}
	if _, err := off.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	t.Logf("gates=%v skipped=%v", rep.Gates, rep.Skipped)
	if got := atomic.LoadInt64(&m.calls); got != 1 {
		t.Errorf("merged mode made %d model calls, want exactly 1 -- per-candidate calls are the "+
			"refuted per-output design", got)
	}
	after := schema.MessagesTokens(req)
	if after >= before {
		t.Errorf("merged mode removed nothing: %d -> %d tokens", before, after)
	}
	// The prompt must carry BOTH the co-reference evidence and the cost-honest framing; those are
	// the two measured ingredients, and a prompt missing either is a different experiment.
	if !strings.Contains(m.lastAsk, "novel=") || !strings.Contains(m.lastAsk, "refs=") {
		t.Error("prompt carries no co-reference evidence, so it is not the merged design")
	}
	if !strings.Contains(m.lastAsk, "NOT notice the gap") {
		t.Error("prompt lacks the cost-honest framing, worth ~26 points of live-kept when measured")
	}
	if strings.Contains(strings.ToLower(m.lastAsk), "recoverable") {
		t.Error("prompt reassures the model that removals are recoverable -- the exact clause that " +
			"measured 91% removal at 6% live-kept")
	}
}

// TRIM IS GONE, and a model that answers with it anyway must degrade to the SAFE direction. Dropping
// the verdict outright would leave the output unjudged, which reads identically to "the model said
// nothing about it" in the counters -- the same indistinguishability the Report.Gates comment warns
// about. Trim was removed because it was chosen zero times in 21 probe opportunities and, in
// production, accepted once against eight rejected as invented.
func TestMergedTrimDegradesToKeep(t *testing.T) {
	body := strings.Repeat("{\"row\":\"real value here\"}\n", 400)
	req := mergedReq(3, body)
	raw, _ := json.Marshal([]map[string]any{{"i": 2, "verdict": "trim", "kept": "text the model invented"}})
	m := &countingModel{reply: string(raw)}
	e, _ := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	off := e.(components.Offload)
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-2",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	before := schema.MessagesTokens(req)
	rep := &components.Report{}
	off.Offload(req, rep, c)
	if schema.MessagesTokens(req) != before {
		t.Error("a trim verdict changed the request; trim is no longer a supported action and must " +
			"degrade to keep rather than splice model-authored text")
	}
	if rep.Gates["merged_keep"] == 0 {
		t.Errorf("a trim verdict was not counted as a keep; gates=%v", rep.Gates)
	}
}

// A drop that CONTRADICTS its own obligation answer must be refused. This is the one verification
// that points the dangerous way: the model has said an outstanding obligation still needs the output
// and then asked to remove it anyway.
func TestMergedRefusesDropThatNamesAnObligation(t *testing.T) {
	body := strings.Repeat("{\"row\":\"real value here\"}\n", 400)
	req := mergedReq(3, body)
	raw, _ := json.Marshal([]map[string]any{{"i": 2, "verdict": "drop",
		"needed_by": "b", "quote": "find the failing test in src/auth.py"}})
	m := &countingModel{reply: string(raw)}
	e, _ := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	off := e.(components.Offload)
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-4",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	before := schema.MessagesTokens(req)
	rep := &components.Report{}
	off.Offload(req, rep, c)
	if schema.MessagesTokens(req) != before {
		t.Errorf("an output was dropped although the model said obligation (b) still needs it; "+
			"gates=%v", rep.Gates)
	}
	if rep.Gates["merged_drop_contradicts_obligation"] == 0 {
		t.Errorf("the contradiction was not counted; gates=%v", rep.Gates)
	}
}

// A FABRICATED obligation quote must be counted. It argues for keeping, so it is not dangerous, but
// quote fidelity degraded with batch size when measured (4 of 37 non-verbatim at batch 16 against 0
// of 16 at batch 10), which makes this counter the signal that the batch is too large.
func TestMergedCountsFabricatedObligationQuote(t *testing.T) {
	body := strings.Repeat("{\"row\":\"real value here\"}\n", 400)
	req := mergedReq(3, body)
	raw, _ := json.Marshal([]map[string]any{{"i": 2, "verdict": "keep",
		"needed_by": "b", "quote": "a sentence that appears nowhere in the transcript at all"}})
	m := &countingModel{reply: string(raw)}
	e, _ := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	off := e.(components.Offload)
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-5",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	rep := &components.Report{}
	off.Offload(req, rep, c)
	if rep.Gates["merged_quote_not_verbatim"] == 0 {
		t.Errorf("an invented obligation quote was accepted without being counted; gates=%v", rep.Gates)
	}
}

// The prompt must carry the CRITERION and demand the obligation evidence. Arms with an identical
// criterion but no required evidence field measured 4/4 false drops against 2/4 -- so a prompt that
// states the rule without forcing the answer is a different, measurably worse experiment.
func TestMergedPromptForcesObligationEvidence(t *testing.T) {
	body := strings.Repeat("{\"row\":\"real value here\"}\n", 400)
	req := mergedReq(3, body)
	m := &countingModel{reply: "[]"}
	e, _ := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	off := e.(components.Offload)
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-6",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	off.Offload(req, &components.Report{}, c)
	for _, want := range []string{"NOT YET COMPLETE", "needed_by", "quote", "VERBATIM"} {
		if !strings.Contains(m.lastAsk, want) {
			t.Errorf("prompt is missing %q, so the forcing function measured to halve false drops "+
				"is not present", want)
		}
	}
	if strings.Contains(m.lastAsk, "\"trim\"") {
		t.Error("prompt still offers trim, which was removed")
	}
}

// A mistyped selection_mode must fail at construction, not silently run the default shape.
func TestMergedRejectsUnknownSelectionMode(t *testing.T) {
	if _, err := newExtractLLM([]byte("{\"selection_mode\":\"bluk\"}")); err == nil {
		t.Error("a typo in selection_mode was accepted; the arm would silently measure the default")
	}
}

// PLAIN TEXT must be droppable too. corefStub returns "" for anything that is not a JSON array or
// object, which is most real tool output -- logs, file reads, tracebacks. Before mergedResidue's
// fallback the drop verdict was recorded in the gate counters while the projection came back empty
// and phase 3 skipped it, so the arm looked like it was deciding and removing nothing. This test
// exists because that is precisely the failure mode the repo's own "acted: 0 is not diagnosable"
// warning describes, one layer further in.
func TestMergedCanDropUnstructuredOutput(t *testing.T) {
	body := strings.Repeat("2026-08-22T10:00:00Z INFO handler finished request in 12ms\n", 500)
	req := mergedReq(4, body)
	vs := []extract.BulkVerdict{{Index: 2, Verdict: "drop"}}
	raw, _ := json.Marshal(vs)
	m := &countingModel{reply: string(raw)}
	e, err := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	off := e.(components.Offload)
	c := &components.Ctx{Ctx: context.Background(), Session: "merged-3",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	before := schema.MessagesTokens(req)
	rep := &components.Report{}
	off.Offload(req, rep, c)
	if schema.MessagesTokens(req) >= before {
		t.Errorf("merged mode could not drop UNSTRUCTURED output: %d -> %d tokens, gates=%v. "+
			"corefStub only handles JSON, so a residue fallback is required or the arm silently "+
			"decides and removes nothing.", before, schema.MessagesTokens(req), rep.Gates)
	}
}
