package offload

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/store"
)

// shrinkingModel returns a Starlark program that drops every line but the first, so the
// extraction is ACCEPTED and the record under test carries a real saving rather than a
// rejection. Nothing here is about the program's cleverness: the test needs the accept branch.
type shrinkingModel struct{ calls int }

func (m *shrinkingModel) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	return "```python\ndef transform(text):\n    return text.split(\"\\n\")[0]\n```", nil
}

// debugCtx returns a context carrying a DEBUG-level JSON logger and the buffer it writes to.
func debugCtx(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logging.With(context.Background(), l)
	if !logging.Debugging(ctx) {
		t.Fatal("the fixture's logger is not at DEBUG, so a guarded record would be skipped " +
			"and this test would pass vacuously")
	}
	return ctx, &buf
}

// records returns every logged record whose msg matches.
func records(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", ln, err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// THE DEFECT (#177). extract_llm emitted exactly one message, `cg.extract_llm`, once per
// request, carrying the DECISION — tools, cands, skip_tail, skip_floor, floor — and nothing
// about the CALL. So /stats could attribute 101 calls at 59,009 ms mean latency and a net value
// of -$1.162 to this component with no per-request trace to check it against, and three
// questions were unanswerable: which requests made the calls, whether the 59-second mean was
// many slow calls or a few multi-minute outliers (opposite fixes), and which candidates lost
// money. extract_llm_sweep's `cg.sweep.ask` already made its economics reconstructable.
//
// This pins the record's EXISTENCE and its economically load-bearing fields. It asserts on the
// rendered log line, not on a struct: the fields are handed to slog as a variadic list, so one
// could be computed correctly and never reach the handler.
func TestExtractLLMLogsOneRecordPerCall(t *testing.T) {
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model) // min_tokens: 1, strategy: code, gate off
	ctx, buf := debugCtx(t)

	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			userMsg("Summarize the worker log and tell me if any batch failed."),
			toolResultMsg(original),
		},
	}
	c := &components.Ctx{
		Session: "callrecord-test",
		Store:   store.NewMemory(store.Options{}),
		Ctx:     ctx,
		Model:   components.ModelSpec{Static: model, Incoming: model},
	}
	rep := &components.Report{Component: "extract_llm"}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// Without a call there is nothing to record, and the assertions below would pass
	// vacuously on an empty slice — the failure mode this repo has been bitten by.
	if model.calls == 0 {
		t.Fatal("the model was never called, so the call-record path was never reached")
	}
	if len(rep.Calls) == 0 {
		t.Fatal("no ModelCall was reported, so there was no call to log")
	}

	got := records(t, buf, "cg.extract_llm.call")
	if len(got) != len(rep.Calls) {
		t.Fatalf("got %d cg.extract_llm.call records for %d reported calls; #177 is that "+
			"there were 0. Log was:\n%s", len(got), len(rep.Calls), buf.String())
	}
	rec := got[0]
	// The economics of one call must be reconstructable from this line alone: whose session,
	// which candidate, how big, on what model, how long, what it consumed, what it cost,
	// whether the never-worse check took it, and what that bought.
	for _, k := range []string{
		"session", "content_key", "candidate_tokens", "model", "latency_ms",
		"input_tokens", "output_tokens", "cost_usd", "accepted", "saved_tokens", "rejection",
	} {
		if _, ok := rec[k]; !ok {
			t.Errorf("cg.extract_llm.call is missing %q — the field it exists to carry", k)
		}
	}
	if rec["session"] != "callrecord-test" {
		t.Errorf("session = %v, want callrecord-test: a call that cannot be tied to a "+
			"request is the exact gap #177 reports", rec["session"])
	}
	if rec["content_key"] == "" || rec["content_key"] == nil {
		t.Error("content_key is empty; without the candidate's identity the record cannot " +
			"be joined to the replay, freeze and cache lookups keyed on it")
	}
	if ct, _ := rec["candidate_tokens"].(float64); ct <= 0 {
		t.Errorf("candidate_tokens = %v, want the candidate's real size", rec["candidate_tokens"])
	}
	// ACCEPT/REJECT must agree with what the component actually did. A record that says
	// accepted while the request kept the original is worse than no record.
	if rec["accepted"] != rep.Calls[0].Accepted {
		t.Errorf("accepted = %v but the reported call says %v", rec["accepted"],
			rep.Calls[0].Accepted)
	}
	if rep.Calls[0].Accepted {
		if st, _ := rec["saved_tokens"].(float64); st <= 0 {
			t.Errorf("saved_tokens = %v on an accepted extraction", rec["saved_tokens"])
		}
	}
	// latency_ms may legitimately be 0 on a fast fake model, so the assertion is on the
	// FIELD's presence (above) rather than on a value the fixture cannot guarantee.
	if _, ok := rec["latency_ms"].(float64); !ok {
		t.Errorf("latency_ms = %v, want a number", rec["latency_ms"])
	}
}

// The record must be DEBUG-guarded, per this repo's stated rule: at INFO nothing may be
// emitted. This also proves the record is not being written through a path that ignores the
// level (a fmt.Println, a logger captured at construction).
//
// VACUITY, stated because it is not what it looks like: NO SINGLE-POINT MUTATION KILLS THIS
// TEST. The property is defended twice over — by the `if dbg` guard, which exists so the
// payload is not built when nobody is reading, and by the slog level on Debug(), which exists
// so it is not printed. Remove the guard and the level still suppresses it; switch Debug to
// Info and the guard still skips it. Verified: `Debug(` -> `Info(` alone leaves this test
// PASSING. Only the combined mutant (`if dbg` -> `if true` AND `Debug` -> `Info`) fails it,
// which was run and does fail. So read this test as pinning the OUTCOME, not either mechanism,
// and do not take its passing as evidence that the guard is present — TestExtractLLMLogsOneRecordPerCall
// is the one that dies when the record itself goes away.
func TestExtractLLMCallRecordIsDebugGated(t *testing.T) {
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := logging.With(context.Background(), l)

	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			userMsg("Summarize the worker log and tell me if any batch failed."),
			toolResultMsg(original),
		},
	}
	c := &components.Ctx{
		Session: "callrecord-info",
		Store:   store.NewMemory(store.Options{}),
		Ctx:     ctx,
		Model:   components.ModelSpec{Static: model, Incoming: model},
	}
	if _, err := e.Offload(req, &components.Report{Component: "extract_llm"}, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if model.calls == 0 {
		t.Fatal("the model was never called, so this proves nothing about the guard")
	}
	if strings.Contains(buf.String(), "cg.extract_llm.call") {
		t.Errorf("the per-call record was emitted at INFO: %s", buf.String())
	}
}
