package offload

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fakeAsker stands in for the proxy's prefix asker.
type fakeAsker struct {
	ask       string
	session   string
	reply     string
	cacheRead int
	err       error
	calls     int
}

func (f *fakeAsker) Ask(_ context.Context, session, ask string) (string, components.PrefixUsage, error) {
	f.calls++
	f.ask, f.session = ask, session
	if f.err != nil {
		return "", components.PrefixUsage{}, f.err
	}
	return f.reply, components.PrefixUsage{CacheRead: f.cacheRead}, nil
}

func mergedWithAsker(t *testing.T, sess string, a components.PrefixAsker, m *countingModel) (*components.Report, int, int) {
	t.Helper()
	// THREE ZONES, because the two prompt shapes ship different amounts and the test has to tell them
	// apart precisely:
	//   HEAD (offset 0)      -- a bounded locator, expected in BOTH shapes (~90 chars).
	//   MID  (offset ~900)   -- inside mergedSampleChars, so expected ONLY in the fallback prompt.
	//   DEEP (past 4000)     -- beyond truncation, so expected in NEITHER.
	body := "HEAD_LOCATOR_LINE opening record\n" +
		strings.Repeat("{\"row\":\"filler\"}\n", 40) +
		"{\"row\":\"MID_SAMPLE_MARKER\"}\n" +
		strings.Repeat("{\"row\":\"filler value here padding\"}\n", 400) +
		"{\"row\":\"DEEP_SAMPLE_MARKER\"}\n"
	req := mergedReq(4, body)
	e, err := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	c := &components.Ctx{Ctx: context.Background(), Session: sess,
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}, PrefixAsk: a}
	before := schema.MessagesTokens(req)
	rep := &components.Report{}
	e.(components.Offload).Offload(req, rep, c)
	return rep, before, schema.MessagesTokens(req)
}

// When a prefix ask is available it must be USED, and the question must NOT ship the outputs. Paying
// fresh tokens to send a truncated copy of content the model is already reading from cache defeats
// the entire mechanism -- and shows it an excerpt of something it could read in full.
func TestPrefixAskUsedAndCarriesNoSamples(t *testing.T) {
	raw, _ := json.Marshal([]extract.BulkVerdict{{Index: 2, Verdict: "drop", NeededBy: "none"}})
	a := &fakeAsker{reply: string(raw), cacheRead: 19595}
	m := &countingModel{reply: "[]"}
	rep, before, after := mergedWithAsker(t, "pfx-1", a, m)

	if a.calls != 1 {
		t.Fatalf("prefix asker called %d times, want 1", a.calls)
	}
	if m.calls != 0 {
		t.Error("the plain completion was used even though a prefix ask was available")
	}
	if rep.Gates["prefix_ask_used"] == 0 {
		t.Errorf("prefix_ask_used not counted; gates=%v", rep.Gates)
	}
	if strings.Contains(a.ask, "MID_SAMPLE_MARKER") {
		t.Error("the prefix ask shipped the output BODY; the transcript is already above the " +
			"question, so this pays fresh for content being read from cache -- and shows the model " +
			"a truncated copy of something it could read in full")
	}
	if !strings.Contains(a.ask, "HEAD_LOCATOR_LINE") {
		t.Error("the prefix ask carries no locator head, so the model cannot tell which output in " +
			"the transcript a label refers to")
	}
	if !strings.Contains(a.ask, "Refer to them by these labels only") {
		t.Error("the prefix ask carries no label inventory, so the model has no way to name a candidate")
	}
	if after >= before {
		t.Errorf("verdicts from the prefix ask were not applied: %d -> %d", before, after)
	}
}

// A cache read of zero is the failure this mechanism cannot detect any other way: it costs ~10x and
// is otherwise indistinguishable from success.
func TestPrefixAskCountsZeroCacheRead(t *testing.T) {
	raw, _ := json.Marshal([]extract.BulkVerdict{{Index: 2, Verdict: "keep"}})
	a := &fakeAsker{reply: string(raw), cacheRead: 0}
	rep, _, _ := mergedWithAsker(t, "pfx-2", a, &countingModel{reply: "[]"})
	if rep.Gates["prefix_ask_cache_read_ZERO"] == 0 {
		t.Errorf("a prefix ask that read NOTHING from cache was not counted; gates=%v", rep.Gates)
	}
}

// First turn of a session, feature off, or a transport failure: fall back to the plain completion.
// Treating "no prefix" as "no verdicts" would silently disable the component on every session's first
// turn and read as a model that declined to act.
func TestPrefixAskFallsBackToCompletion(t *testing.T) {
	raw, _ := json.Marshal([]extract.BulkVerdict{{Index: 2, Verdict: "drop", NeededBy: "none"}})
	a := &fakeAsker{err: errors.New("no stashed prefix for this session")}
	m := &countingModel{reply: string(raw)}
	rep, before, after := mergedWithAsker(t, "pfx-3", a, m)
	if m.calls != 1 {
		t.Errorf("plain completion called %d times after the prefix ask failed, want 1", m.calls)
	}
	if rep.Gates["prefix_ask_failed"] == 0 {
		t.Errorf("the failure was not counted; gates=%v", rep.Gates)
	}
	if after >= before {
		t.Errorf("the fallback produced no removal: %d -> %d; a failed prefix ask must not disable "+
			"the component", before, after)
	}
	// And the fallback prompt DOES carry samples, because a bare completion has no other way to show them.
	if !strings.Contains(m.lastAsk, "MID_SAMPLE_MARKER") {
		t.Error("the fallback completion shipped no samples, so the model was shown nothing to judge")
	}
}

// OFFERED is not ANSWERED, and conflating them is what made three iterations read as "the model
// declines to act". Live, verdicts/calls read 2.80 while merged_batch_truncated fired 43 times in 162
// calls -- impossible for offered batches. This asserts the two quantities are recorded separately, so
// a starved batch and a model that answers for a third of a full batch cannot look identical.
func TestMergedRecordsOfferedSeparatelyFromAnswered(t *testing.T) {
	body := strings.Repeat("{\"row\":\"real value here padding\"}\n", 400)
	req := mergedReq(6, body) // six tool outputs => six candidates offered
	// The model answers for exactly ONE of them, which is the behaviour being made visible.
	raw, _ := json.Marshal([]extract.BulkVerdict{{Index: 2, Verdict: "keep"}})
	m := &countingModel{reply: string(raw)}
	e, err := newExtractLLM([]byte("{\"selection_mode\":\"merged\",\"min_tokens\":300,\"allow_on_caching_backend\":true,\"economic_gate\":false}"))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	c := &components.Ctx{Ctx: context.Background(), Session: "offered-1",
		Store: store.NewMemory(store.Options{}), CtxWindow: 200000,
		Model: components.ModelSpec{Incoming: m, Static: m}}
	rep := &components.Report{}
	e.(components.Offload).Offload(req, rep, c)

	offered := rep.Gates["merged_offered"]
	answered := rep.Gates["merged_keep"] + rep.Gates["merged_drop"] +
		rep.Gates["merged_drop_contradicts_obligation"]
	if offered < 2 {
		t.Fatalf("merged_offered = %d; the offered batch size must be recorded, not inferred from "+
			"verdicts; gates=%v", offered, rep.Gates)
	}
	if answered != 1 {
		t.Fatalf("expected exactly 1 verdict answered, got %d; gates=%v", answered, rep.Gates)
	}
	if offered == answered {
		t.Errorf("offered (%d) and answered (%d) are indistinguishable, which is precisely the "+
			"conflation that made a full batch look like a starved one", offered, answered)
	}
}
