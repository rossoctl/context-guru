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

// newSummarizeTestComponent builds summarize through its REGISTERED CONSTRUCTOR (same
// reason as newTimeoutTestComponent: the constructor is where defaults and the legacy
// start_from_message fold happen) with thresholds low enough that the fixture below
// clears every gate. Anything higher and the component SKIPS, which would make the
// timeout assertion vacuously pass.
func newSummarizeTestComponent(t *testing.T, model components.Model) *Summarize {
	t.Helper()
	c, err := newSummarize([]byte(
		"keep_last: 1\nmin_tokens: 10\nresummarize_tokens: 0\n" +
			"trigger:\n  min_messages: 3\n  min_request_tokens: 10\n"))
	if err != nil {
		t.Fatalf("newSummarize: %v", err)
	}
	s, ok := c.(*Summarize)
	if !ok {
		t.Fatalf("newSummarize returned %T, want *Summarize", c)
	}
	s.modelClient = model
	s.mode = markerFull
	return s
}

// THE REGRESSION THIS GUARDS AGAINST
//
// summarize's budget was a hardcoded 150s sized against an IDLE server (measured ~19.6s
// mean per call there). Under load the same call must also absorb server-side queue wait
// (p50 17.2s / p95 78.8s under KV pressure) on top of a ~57k-token prefill, so the
// budget has to be raisable per run — and when it IS exceeded that has to be
// distinguishable from the component declining.
//
// Two halves of the contract:
//  1. no content is mutated (the caller can forward the original request), and
//  2. the abandoned call is COUNTED as a TIMEOUT, not as a generic error — the two
//     mean opposite things ("budget too small for this load" vs "the cheap-model route
//     is broken") and the per-component `reverted` count cannot tell them apart.
func TestSummarizeTimeoutIsCountedAndLeavesInputIntact(t *testing.T) {
	timeoutsBefore := SummarizeTimeouts()
	errorsBefore := SummarizeErrors()

	// A short budget keeps the test fast; the code path is identical at 300s.
	t.Setenv("CONTEXT_GURU_SUMMARIZE_TIMEOUT", "150ms")
	prev := summarizeCallTimeout
	summarizeCallTimeout = resolveSummarizeCallTimeout()
	defer func() { summarizeCallTimeout = prev }()
	if summarizeCallTimeout != 150*time.Millisecond {
		t.Fatalf("timeout override not applied: got %v", summarizeCallTimeout)
	}

	model := &slowModel{}
	s := newSummarizeTestComponent(t, model)

	span := strings.Repeat("ran pytest tests/test_handler.py, 3 failures in src/mod/file.py\n", 40)
	req := &bschemas.BifrostChatRequest{
		Input: []bschemas.ChatMessage{
			userMsg("Fix the failing handler in src/mod/file.py and run the tests."),
			toolResultMsg(span),
			toolResultMsg(span),
			userMsg("keep going"),
		},
	}
	before := len(req.Input)

	c := &components.Ctx{
		Session: "summarize-timeout-test",
		Store:   store.NewMemory(store.Options{}),
		Ctx:     context.Background(),
		Model:   components.ModelSpec{Static: model, Incoming: model},
	}

	rep := &components.Report{}
	_, err := s.Offload(req, rep, c)

	// The model must actually have been called, or the test proves nothing: a component
	// that skipped on a trigger/floor also leaves the messages alone.
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatal("model was never called, so the timeout path was never exercised. " +
			"Check the fixture clears trigger.min_messages / min_request_tokens and " +
			"that the span is above min_tokens.")
	}
	if err == nil {
		t.Fatal("Offload returned nil on a blown deadline; the pipeline needs the error " +
			"to revert this component")
	}
	// The message list must be untouched: summarize is the one component that changes
	// the message COUNT, so a partial rebuild on the error path would leave the caller
	// holding a transcript with no summary in it.
	if len(req.Input) != before {
		t.Fatalf("req.Input rebuilt on the error path: %d messages, want %d",
			len(req.Input), before)
	}

	if got := SummarizeTimeouts() - timeoutsBefore; got != 1 {
		t.Errorf("summarize_timeouts += %d, want 1 — an abandoned call must be visible "+
			"in /stats, or an arm that stops summarizing under load reads as an arm "+
			"that got faster", got)
	}
	if got := SummarizeErrors() - errorsBefore; got != 0 {
		t.Errorf("summarize_errors += %d, want 0 — a deadline is not a transport error, "+
			"and conflating them hides which knob to reach for", got)
	}
}

// resolveTimeoutEnv is shared by both NeedsModel components, so its parse rules must
// hold identically for either name — a value accepted for one and silently ignored for
// the other is invisible in a run and looks like the component not firing.
func TestResolveTimeoutEnv(t *testing.T) {
	const def = 300 * time.Second
	for _, tc := range []struct {
		name, val string
		want      time.Duration
	}{
		{"unset", "", def},
		{"bare integer means seconds", "240", 240 * time.Second},
		{"go duration", "4m", 4 * time.Minute},
		{"whitespace tolerated", "  90s  ", 90 * time.Second},
		{"zero falls back", "0", def},
		{"negative falls back", "-5", def},
		{"garbage falls back", "soon", def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CG_TEST_TIMEOUT_KNOB", tc.val)
			if tc.val == "" {
				os.Unsetenv("CG_TEST_TIMEOUT_KNOB")
			}
			if got := resolveTimeoutEnv("CG_TEST_TIMEOUT_KNOB", def); got != tc.want {
				t.Errorf("resolveTimeoutEnv(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
