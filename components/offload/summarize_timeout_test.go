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
			"trigger:\n  min_messages: 3\n  min_request_tokens: 10\n  min_request_frac: 0\n"))
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

	beforeTimeouts := atomic.LoadInt64(&summarizeTimeouts)
	rep := &components.Report{}
	_, err := s.Offload(req, rep, c)

	// A BLOWN DEADLINE NO LONGER REACHES THE CALLER, and that is the contract this test now
	// pins. The summary is produced off the hot path, so the request has already been answered
	// by the time the call gives up: Offload returns nil, the transcript is untouched, and the
	// only trace is the counter.
	//
	// This is the whole point of the async execution model rather than a weakening of the old
	// one. Measured on a live run against the SYNCHRONOUS path: a call sat for its full 300s
	// budget while upstream itself took 2.8s, the prompt cache entry expired inside the
	// pipeline, and the turn then paid a 197,879-token full-prefix rewrite — the exact cost the
	// trigger exists to avoid, caused by our own latency. A timeout that costs nothing but a
	// counter increment is the outcome worth having.
	if err != nil {
		t.Fatalf("Offload returned %v on a blown deadline; with the call off the hot path the "+
			"request is already answered and there is nobody to hand an error to", err)
	}

	// The model must actually have been called, or the test proves nothing: a component that
	// skipped on a trigger/floor also leaves the messages alone. Drained on the production
	// channel rather than slept, so this stays deterministic.
	if !WaitForSummaryForTest(c.Session, 5*time.Second) {
		t.Fatal("the background summary never finished, so the timeout path was never exercised")
	}
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatal("model was never called, so the timeout path was never exercised. " +
			"Check the fixture clears trigger.min_messages / min_request_tokens and " +
			"that the span is above min_tokens.")
	}
	// The counter is the ONLY place a degraded summarizer now shows up, so it has to move.
	if got := atomic.LoadInt64(&summarizeTimeouts); got <= beforeTimeouts {
		t.Errorf("summarize_timeouts did not move (%d -> %d): with nothing on the hot path "+
			"waiting for the call, this counter is the only evidence a summary was paid for "+
			"and lost", beforeTimeouts, got)
	}
	// And no checkpoint was written, because there is no summary to checkpoint.
	if _, ok := loadCheckpoint(c); ok {
		t.Error("a timed-out call left a checkpoint; a later turn would splice a summary that " +
			"was never produced")
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
