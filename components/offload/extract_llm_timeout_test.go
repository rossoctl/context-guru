package offload

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// slowModel blocks until its context is cancelled, i.e. it always exhausts whatever
// per-call deadline extract_llm imposes. That is the behaviour of a real compaction
// model queued behind a saturated GPU.
type slowModel struct{ calls int64 }

func (m *slowModel) Complete(ctx context.Context, _ string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	<-ctx.Done()
	return "", ctx.Err()
}

func toolResultMsg(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{
		Role:    bschemas.ChatMessageRoleTool,
		Content: &bschemas.ChatMessageContent{ContentStr: &t},
	}
}

// A USER message is mandatory in the fixture: Offload derives its relevance
// `goal` from the first/last user turn (common.go:214) and returns early with
// rep.Skipped when `keywords(goal)` is empty. A request of only tool messages
// therefore never calls the model, and the timeout assertion would silently
// vacuously "pass" (it skipped) — which is how this test first fooled me.
func userMsg(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{
		Role:    bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: &t},
	}
}

// newTimeoutTestComponent builds the component through its REGISTERED CONSTRUCTOR and
// then injects the model client.
//
// Do not hand-roll `&ExtractLLM{...}` here. The struct carries maps that only
// newExtractLLM initializes (llmSeen, prevTokens), so a struct literal panics with
// "assignment to entry in nil map" the moment #28's per-session size tracking runs —
// which is how this test broke when it was rebased onto the economic-gate work. Going
// through the constructor also means a future field cannot silently leave this fixture
// in a shape production never has.
//
// `economic_gate: false` keeps the test about the DEADLINE: with the gate on, a
// suppressed call would leave the output verbatim too, so a gate change could make this
// test pass for the wrong reason.
func newTimeoutTestComponent(t *testing.T, model components.Model) *ExtractLLM {
	t.Helper()
	c, err := newExtractLLM([]byte("min_tokens: 1\nstrategy: code\neconomic_gate: false\n"))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	e, ok := c.(*ExtractLLM)
	if !ok {
		t.Fatalf("newExtractLLM returned %T, want *ExtractLLM", c)
	}
	e.modelClient = model
	e.mode = markerFull
	return e
}

// THE REGRESSION THIS GUARDS AGAINST
//
// extract_llm wraps each model call in a per-call deadline and then DISCARDS the
// error (`res, sum, _ :=`), leaving the tool output verbatim. Failing open is
// correct — compaction must never break the agent's request — but it used to be
// SILENT, and that silence is what made a real measurement unreadable:
//
// On a KV-pressured on-prem vLLM (server-side queue wait p50 17.2s against a 15s
// budget) the component's llm_calls fell 2,093 -> 255 at equal request volume while
// per-request overhead ROSE 55%. The arm had partially switched itself off, and every
// dashboard read that as a 42-point latency IMPROVEMENT.
//
// So this asserts BOTH halves of the contract:
//  1. the tool output is unchanged (fail-open preserved), and
//  2. the abandoned call is COUNTED (llm_timeouts increments).
//
// If the ctx.Err() branch is ever "simplified" away, (1) still passes and only (2)
// catches it — which is exactly why (2) exists.
func TestExtractLLMTimeoutIsCountedAndFailsOpen(t *testing.T) {
	timeoutsBefore := LLMTimeouts()
	errorsBefore := LLMErrors()

	// A short budget keeps the test fast; the code path is identical at 90s.
	t.Setenv("CONTEXT_GURU_LLM_TIMEOUT", "150ms")
	prev := llmCallTimeout
	llmCallTimeout = resolveLLMCallTimeout()
	defer func() { llmCallTimeout = prev }()
	if llmCallTimeout != 150*time.Millisecond {
		t.Fatalf("timeout override not applied: got %v", llmCallTimeout)
	}

	model := &slowModel{}
	e := newTimeoutTestComponent(t, model)

	original := strings.Repeat("src/mod/file.py:12: def handler(request, context):\n", 200)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			userMsg("Fix the failing handler in src/mod/file.py and run the tests."),
			toolResultMsg(original),
		},
	}
	c := &components.Ctx{
		Session: "timeout-test",
		Store:   store.NewMemory(store.Options{}),
		Ctx:     context.Background(),
		Model:   components.ModelSpec{Static: model, Incoming: model},
	}

	rep := &components.Report{}
	if _, err := e.Offload(req, rep, c); err != nil {
		// Fail-open means the component must not surface an error either.
		t.Fatalf("Offload returned an error on timeout; it must fail open: %v", err)
	}

	// The model must actually have been called, or the test proves nothing: a
	// component that declined for an unrelated reason also leaves the text alone.
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatal("model was never called, so the timeout path was never exercised. " +
			"Check that the fixture has a USER message (goal/keywords) and a tool " +
			"output above the floor.")
	}

	// (1) FAIL-OPEN: the request must remain VALID and no content may be lost
	// irrecoverably. Note it need not be byte-identical: `code` mode falls back to
	// the `deterministic` strategy when the LLM call dies (extract.go:229-234,
	// AllowDeterministic), so a timed-out LLM call can still yield a smaller output
	// via pure rules. That is correct behaviour — and it is also precisely why
	// llm_calls collapsing under load was so hard to see: the arm keeps compacting a
	// little, so nothing looks broken.
	got := ""
	if m := req.Input[1]; m.Content != nil && m.Content.ContentStr != nil {
		got = *m.Content.ContentStr
	}
	if got == "" {
		t.Fatal("tool output was emptied after a timed-out call; fail-open broken")
	}
	if len(got) < len(original) {
		// Reversibility is the invariant that matters when content did shrink.
		if !strings.Contains(got, "<<cg:") {
			t.Fatalf("output shrank (%d -> %d) with NO <<cg:HASH>> marker: the original "+
				"is unrecoverable, which violates the reversibility invariant",
				len(original), len(got))
		}
		t.Logf("deterministic fallback shrank the output %d -> %d (marker present, "+
			"reversible) even though the LLM call timed out", len(original), len(got))
	}

	// (2) OBSERVABILITY: the abandoned call must be counted, not swallowed.
	gotT := LLMTimeouts() - timeoutsBefore
	gotE := LLMErrors() - errorsBefore
	if gotT == 0 {
		t.Fatalf("a deadline-exceeded call was NOT counted: llm_timeouts +%d, llm_errors +%d.\n"+
			"This is the silent fail-open regression: an arm that stops compacting under "+
			"load would again read as an efficiency win.", gotT, gotE)
	}
	t.Logf("counted correctly: llm_timeouts +%d, llm_errors +%d, model calls=%d, budget=%v",
		gotT, gotE, atomic.LoadInt64(&model.calls), llmCallTimeout)
}

// trackerState reads the ratio tracker's accumulators under its own lock. Safe to call
// after Offload returns: it wg.Wait()s its call goroutines before writing back.
func trackerState(r *ratioTracker) (removed, total int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removed, r.total
}

// A TIMEOUT IS NOT EVIDENCE ABOUT COMPRESSIBILITY.
//
// #28's gate learns this workload's compression ratio from outcomes, and counts a call
// that produced nothing as ratio 0 — correct when the MODEL looked at the output and
// could not shrink it. A call abandoned on the deadline never got that far: it says the
// server is slow, not that the content is incompressible.
//
// Feeding it in anyway makes the gate shut itself permanently on exactly the deployment
// whose budget is already too small, and minRatioSampleTokens (1500) makes that cheap:
// ONE timed-out medium output ends this session's exploration and starts pulling ratio()
// below the 0.12 prior, until evaluateGate suppresses everything. The tracker lives on
// the Pipeline for the proxy's lifetime, so nothing revises it afterwards — the
// self-justifying prior that extract_econ.go's exploration budget exists to prevent,
// re-entered through the timeout path.
//
// This is a live regime, not a hypothetical: 13 timeouts in one 50-task arm at the 90s
// budget on a KV-pressured TP=1 server.
func TestExtractLLMTimeoutDoesNotPoisonRatioTracker(t *testing.T) {
	t.Setenv("CONTEXT_GURU_LLM_TIMEOUT", "150ms")
	prev := llmCallTimeout
	llmCallTimeout = resolveLLMCallTimeout()
	defer func() { llmCallTimeout = prev }()

	timeoutsBefore := LLMTimeouts()
	model := &slowModel{}
	e := newTimeoutTestComponent(t, model)

	// Deliberately UNDER sampleChars (4000). The `code` strategy falls back to
	// `deterministic`, which returns a relevance WINDOW of maxChars — on a larger body
	// that window is smaller than the input, so the timed-out call still yields a result
	// and the tracker is then observed legitimately (that is the other test's logged
	// path). Keeping the body under the window size makes the fallback unable to shrink
	// it, which is the only way to reach the "timed out with nothing back" branch.
	original := strings.Repeat("src/mod/file.py:12: def handler(request, context):\n", 55)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			userMsg("Fix the failing handler in src/mod/file.py and run the tests."),
			toolResultMsg(original),
		},
	}
	c := &components.Ctx{
		Session: "ratio-poison-test",
		Store:   store.NewMemory(store.Options{}),
		Ctx:     context.Background(),
		Model:   components.ModelSpec{Static: model, Incoming: model},
	}
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
		t.Fatalf("Offload must fail open, got error: %v", err)
	}

	// Guard against a vacuous pass three ways: the model must have been called, the
	// deadline must have fired, and the fallback must NOT have produced a result (or we
	// are in the legitimately-observed branch and prove nothing).
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatal("model was never called; the timeout path was not exercised")
	}
	if got := LLMTimeouts() - timeoutsBefore; got == 0 {
		t.Fatalf("expected the deadline to fire and be counted, got +%d", got)
	}
	got := ""
	if m := req.Input[1]; m.Content != nil && m.Content.ContentStr != nil {
		got = *m.Content.ContentStr
	}
	if got != original {
		t.Skipf("the deterministic fallback shrank this body (%d -> %d), so the tracker "+
			"was observed legitimately; this case needs a body the fallback cannot "+
			"reduce (under sampleChars=%d)", len(original), len(got), 4000)
	}

	removed, total := trackerState(&e.ratios)
	if total != 0 || removed != 0 {
		t.Fatalf("a timed-out call with NO result was fed to the ratio tracker "+
			"(removed=%d, total=%d, want 0/0).\nThat records 'this workload compresses "+
			"0%%' from a server-latency failure, which drives ratio() below the %.2f "+
			"prior and — past minRatioSampleTokens=%d — ends exploration for good. The "+
			"component then switches ITSELF off on the loaded server, silently, which is "+
			"the exact failure llm_timeouts exists to expose.",
			removed, total, defaultCompressionRatio, minRatioSampleTokens)
	}
}

// The default must stay generous enough for a loaded self-hosted server, and the
// override must accept both "90s" and a bare "90".
func TestResolveLLMCallTimeout(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultLLMCallTimeout},
		{"90s", 90 * time.Second},
		{"90", 90 * time.Second}, // bare integer = seconds
		{"2m", 2 * time.Minute},
		{"garbage", defaultLLMCallTimeout},
		{"0", defaultLLMCallTimeout}, // zero would disable compaction entirely
		{"-5s", defaultLLMCallTimeout},
	}
	for _, tc := range cases {
		if tc.env == "" {
			os.Unsetenv("CONTEXT_GURU_LLM_TIMEOUT")
		} else {
			t.Setenv("CONTEXT_GURU_LLM_TIMEOUT", tc.env)
		}
		if got := resolveLLMCallTimeout(); got != tc.want {
			t.Errorf("CONTEXT_GURU_LLM_TIMEOUT=%q: got %v, want %v", tc.env, got, tc.want)
		}
	}
	// The 15s value that caused the measured failure must not become the default again.
	if defaultLLMCallTimeout <= 15*time.Second {
		t.Errorf("default budget %v is back at or below the 15s that silently "+
			"disabled the component under load", defaultLLMCallTimeout)
	}
}
