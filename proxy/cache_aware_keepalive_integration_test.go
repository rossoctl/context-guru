package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// ⭐ THE REGRESSION THIS SUITE EXISTS TO CATCH: every other keep-alive-substitute test in this
// package registers a candidate directly (offload.RegisterKeepAliveCandidateForTest) or drives
// cache_aware_summarizer.Offload directly against a hand-built Ctx — neither ever goes through
// proxy.go's own serve() path, so neither could ever have caught the bug a live test found: an
// unconditional offload.ClearKeepAliveCandidate(tr.Session), called AFTER the pipeline ran,
// deleted the very candidate that SAME pipeline run had just registered a few lines earlier in
// the same request. Every real request therefore cleared its own registration before any ping
// could ever see it, and every substitute counter stayed at zero in production. This test drives
// two real HTTP requests through the actual Mux()/serve() path and a real keep-alive ping, and
// would have failed against that code (reason=no_candidate) before the fix
// (offload.ClearStaleKeepAliveCandidate, gated on a timestamp captured before the pipeline ran
// rather than clearing unconditionally after it).
//
// Shape of the two turns, both needed for the same reason the live test's own log line was
// (`cache_state_declined_warm`), not an artefact of this test's plumbing:
//
//	turn 1: first-ever request for the session — phase is Unknown (no prior turn to measure
//	        idle against), which `cache_state: pre_expiry` PERMITS, so this turn commissions a
//	        checkpoint immediately. A session's keep-alive entry is also not "pingable" at all
//	        until its SECOND request (proxy/keepalive.go's own documented rule — turn 0 is
//	        79% of all sessions and 0.9% of the value), so a second turn is required regardless.
//	turn 2: the SAME session, moments later — idle is now measurable (~0ms) against the
//	        default 300s TTL, which `pre_expiry_seconds: 299` makes JUST short of "pre-expiry":
//	        phase resolves to Warm. `cache_state: pre_expiry` DECLINES on Warm — the live bug's
//	        own `cache_state_declined_warm` — but registration happens before that decline is
//	        even checked. `resummarize_tokens: 1` keeps turn 1's checkpoint from being silently
//	        REUSED here (which would return before ever reaching registration again): any new
//	        material at all counts as stale, so turn 2 falls through to the fresh-commission
//	        gates, registers ITS OWN (larger) span, and only then declines on `phased`.
func TestARealRequestsKeepAliveCandidateSurvivesAndAPingDispatchesIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// cache_read_input_tokens nonzero on every response — not because the mock's own
		// bookkeeping needs it, but because the keeper RETIRES an entry outright, never
		// tracking it for a ping at all, when a request's usage reports both cache tiers as
		// zero (proxy/keepalive.go's own "nothing to protect" guard). Simulating a warm read
		// is what makes this entry pingable in the first place.
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"aws/claude-sonnet-5",` +
			`"content":[{"type":"text","text":"<summary>explored the handler, 3 tests fail.</summary>"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":8,` +
			`"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`))
	}))
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [cache_aware_summarizer]\n" +
		"components:\n  cache_aware_summarizer:\n    keep_last_turns: 1\n    min_tokens: 1\n" +
		"    resummarize_tokens: 1\n" +
		"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n" +
		"      cache_state: pre_expiry\n      pre_expiry_seconds: 299\n"))
	if err != nil {
		t.Fatalf("config.LoadBytes: %v", err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatalf("cfg.Build: %v", err)
	}
	// A dashboard recorder is REQUIRED for the keeper to retain anything at all — see
	// keeper.record's own "NO AUDIT SINK, NO RETENTION" comment. Omitting it is exactly the
	// failure this repo's own memory already diagnosed once (a sandbox proxy launched without
	// --dashboard, keep-alive silently retiring every entry) and would make this test
	// vacuously pass for the wrong reason: not because the registration bug is fixed, but
	// because the keeper never tracks the session long enough to try pinging it at all.
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: upstream.URL,
		Dashboard:         rec,
		Cache: CachePolicy{KeepAlive: true, Idle: 2 * time.Second, MaxPings: 1,
			MaxUSDPerPing: 1, MinPrefixTokens: 0},
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	const session = "it-keepalive-survives-a-real-request"
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })

	post := func(messages []any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"model":      "aws/claude-sonnet-5",
			"max_tokens": 64,
			"system": []any{
				map[string]any{"type": "text", "text": "you are claude code",
					"cache_control": map[string]any{"type": "ephemeral"}},
			},
			"messages": messages,
		})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", session)
		req.Header.Set("x-api-key", "sk-test-key")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("proxy returned %d", resp.StatusCode)
		}
	}

	// Turn 1: the first 3 messages, verbatim — kept identical in turn 2 below so the covered
	// prefix's hash still matches and tryReuse recognizes it as the SAME checkpoint, merely
	// stale, rather than a diverged conversation.
	head := []any{
		map[string]any{"role": "user", "content": "fix the failing tests, there is a lot of context here"},
		map[string]any{"role": "assistant", "content": "working on it, let me check"},
		map[string]any{"role": "user", "content": "keep going, the tests still fail and there is more output to review"},
	}
	post(head)
	if !offload.WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatal("turn 1's commissioned summary never landed")
	}

	// Turn 2: the same head, plus two more messages — grows the span tryReuse would otherwise
	// have replayed byte-identically, which is what makes resummarize_tokens: 1 call it stale.
	grown := append(append([]any{}, head...),
		map[string]any{"role": "assistant", "content": "ran them again, same failure"},
		map[string]any{"role": "user", "content": "try a different approach then"})
	post(grown)

	// THE CORE ASSERTION: the candidate turn 2's own pipeline run just registered must still be
	// there. Before the fix, ClearKeepAliveCandidate (called unconditionally, after the
	// pipeline, in proxy.go) deleted it in this exact request.
	if _, _, reason, ok := offload.KeepAliveSubstitute(session); !ok {
		t.Fatalf("the candidate did not survive the real request that registered it (reason=%q) — "+
			"this is the bug a live test found: every real request cleared its own registration",
			reason)
	}

	// AND: a following ping actually dispatches it, rather than merely leaving a candidate
	// sitting unused in the registry. The real keep-alive ticker (proxy/keepalive.go,
	// keepAliveTick=2s) drives this — no fake clock, so the wait below is real wall-clock time,
	// bounded well under the test timeout.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.keeper.summarySubstituted.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.keeper.summarySubstituted.Load(); got == 0 {
		t.Fatalf("summarySubstituted = %d after %v — the following ping never dispatched the "+
			"candidate (noCandidate=%d checkpointExists=%d phaseMismatch=%d overBudget=%d failed=%d)",
			got, 10*time.Second,
			h.keeper.summarySubstituteNoCandidate.Load(), h.keeper.summarySubstituteCheckpointExists.Load(),
			h.keeper.summarySubstitutePhaseMismatch.Load(), h.keeper.summarySubstituteOverBudget.Load(),
			h.keeper.summarySubstituteFailed.Load())
	}
}
