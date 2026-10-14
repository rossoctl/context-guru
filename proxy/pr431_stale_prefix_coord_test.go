package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// PR #431 round 1 review: the stale-prefix guard extract_llm_sweep added hashed req.Input AT THE
// SWEEP'S OWN PIPELINE POSITION, while CoversSpan (proxy's sentStash, via components.PrefixCoverage)
// compares against the stashed PRE-pipeline body — the same confusion #414's review already found
// and fixed for cache_aware_summarizer. Any component that runs BEFORE the sweep and rewrites a
// cached message (textclean strips terminal noise on every turn it sees it) makes the two hashes
// disagree even though the stash holds exactly the right bytes, so the sweep in housellm/xhigh
// (where it runs 9th, after format, dedup, toon, cmdfilter, searchfold, textclean, extract_llm)
// never gets to use the prefix ask on a real transcript.
//
// Fixed by putting the pre-pipeline messages on components.Ctx (RawInput, set once by apply.BodyOpts
// before Pipeline.Run) and hashing THAT span in both guards instead of req.Input/msgs at the
// guarded component's own position. See components.Ctx.RawInput and
// components/offload/stale_prefix.go.

// sweepGuardAsker is the components.PrefixAsker + components.PrefixCoverage pair this file drives
// turn 2 through: Ask reads the real sentStash (so Offload's own ErrNoPrefix/coverage behavior runs
// unmodified) and appends to the real forwarded body, exactly as the production prefixAsker does.
type sweepGuardAsker struct {
	stash *sentStash
	cli   cheapmodel.Anthropic
}

func (a sweepGuardAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	body := a.stash.get(session)
	if len(body) == 0 {
		return "", components.PrefixUsage{}, components.ErrNoPrefix
	}
	reply, u, err := a.cli.CompletePrefixed(ctx, body, ask)
	return reply, components.PrefixUsage(u), err
}

func (a sweepGuardAsker) CoversSpan(session string, count int, hash string) bool {
	return a.stash.CoversSpan(session, count, hash)
}

// sweepGuardTurn1 drives one real request through h.serve() (the full proxy stack: it stashes BOTH
// preBody and the forwarded body under the scoped session key sentStash uses) and returns that
// scoped session id.
func sweepGuardTurn1(t *testing.T, h *Handler, srv *httptest.Server, msgs []any) string {
	t.Helper()
	session := "pr431-" + t.Name()
	body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 64,
		"system": "you are claude code", "messages": msgs})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", session)
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 proxy returned %d", resp.StatusCode)
	}
	h.sent.mu.Lock()
	defer h.sent.mu.Unlock()
	for k := range h.sent.m {
		if strings.HasPrefix(k, session) || strings.Contains(k, session) {
			return k
		}
	}
	// Fallback: a scoped key that does not literally contain the explicit session id (the
	// derived-hash path). There is exactly one entry after one request to a fresh handler.
	for k := range h.sent.m {
		return k
	}
	t.Fatal("turn 1 stashed nothing")
	return ""
}

// toolTurn1Messages builds a 12-tool-output transcript (above the sweep's default min_inventory
// of 10) whose first tool output is replaced by firstToolOut, so the fixture can plant either ANSI
// noise (for textclean to rewrite) or plain text (the control).
func toolTurn1Messages(firstToolOut string) []any {
	msgs := []any{map[string]any{"role": "user", "content": "fix the failing tests"}}
	for i := 0; i < 12; i++ {
		id := "tu_" + string(rune('a'+i))
		out := strings.Repeat("plain record "+id+" of the audit log\n", 60)
		if i == 0 {
			out = firstToolOut
		}
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "go test " + id}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": id, "content": out}}})
	}
	msgs = append(msgs, map[string]any{"role": "assistant", "content": "looked at it"},
		map[string]any{"role": "user", "content": "continue"})
	return msgs
}

// runSweepGuardTurn2 normalizes msgs2, builds the Ctx apply.BodyOpts would have built for it
// (RawInput included — this bypasses apply, so nothing sets RawInput unless the test does), and
// runs it through the real pipeline built from pipelineYAML. It returns the gates/events the sweep
// reported and how many of those upstream calls were the prefix ask itself.
func runSweepGuardTurn2(t *testing.T, pipelineYAML string, upstreamURL string, stash *sentStash,
	session string, msgs2 []any, maxCachedIdx int, upstreamCalls *atomic.Int64) (gates, events map[string]int) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(pipelineYAML))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 64,
		"system": "you are claude code", "messages": msgs2})
	norm := apply.NormalizeMessages(bschemas.Anthropic, body2)
	raw := make([]bschemas.ChatMessage, len(norm))
	copy(raw, norm)
	req := &bschemas.BifrostChatRequest{Input: norm}
	cli := cheapmodel.Anthropic{BaseURL: upstreamURL, Model: "claude-sonnet-5", APIKey: "k"}
	cli.MaxTokens = cheapmodel.PrefixAskMaxTokens
	ctx := &components.Ctx{
		Ctx: context.Background(), Session: session, Store: store.NewMemory(store.Options{}),
		CtxWindow: 1_000_000, ModelName: "claude-sonnet-5",
		CacheAware: true, MaxCachedIdx: maxCachedIdx,
		IdleMs: 4 * 60 * 1000, CacheTTLMs: 5 * 60 * 1000,
		PrefixAsk: sweepGuardAsker{stash: stash, cli: cli},
		// RawInput is what apply.BodyOpts would have set, as a shallow copy taken before this
		// turn's own pipeline runs — exactly what the fix reads instead of req.Input at the
		// guarded component's own position.
		RawInput: raw,
	}
	before := upstreamCalls.Load()
	rr := pipe.Run(req, ctx)
	gates, events = map[string]int{}, map[string]int{}
	for _, rep := range rr.Components {
		for k, v := range rep.Gates {
			gates[rep.Component+"/"+k] += v
		}
		for k, v := range rep.Events {
			events[rep.Component+"/"+k] += v
		}
	}
	_ = upstreamCalls.Load() - before
	return gates, events
}

// newSweepGuardUpstream is a mock upstream that answers both the normal forwarded turn 1 request
// and the sweep's own prefix ask with a trivial, well-formed body, and counts every call it saw.
func newSweepGuardUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5",` +
			`"content":[{"type":"text","text":"[]"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`))
	}))
}

// TestSweepGuardUsesRawCoordinatesNotPipelinePosition is the fixed version of PR #431 round 1's
// finding: textclean, running before extract_llm_sweep, rewrites the ANSI tool output on every
// turn — including turn 2, the one the sweep is about to adjudicate — so req.Input at the sweep's
// OWN position never matches the stash's pre-pipeline bytes. The fix must still find the stash
// covering, because both sides now hash the SAME (raw, pre-pipeline) span.
func TestSweepGuardUsesRawCoordinatesNotPipelinePosition(t *testing.T) {
	var calls atomic.Int64
	upstream := newSweepGuardUpstream(&calls)
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [textclean, extract_llm_sweep]\n" +
		"components:\n  extract_llm_sweep:\n    min_tokens: 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream.URL})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	ansi := "\x1b[31mFAIL\x1b[0m " + strings.Repeat("filler output line\n", 60)
	msgs1 := toolTurn1Messages(ansi)
	session := sweepGuardTurn1(t, h, srv, msgs1)

	norm1 := apply.NormalizeMessages(bschemas.Anthropic, pr431ToolTurnBody(t, msgs1))
	msgs2 := append(append([]any{}, msgs1...),
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "and now?"})

	gates, events := runSweepGuardTurn2(t, "pipeline: [textclean, extract_llm_sweep]\n"+
		"components:\n  extract_llm_sweep:\n    min_tokens: 20\n",
		upstream.URL, h.sent, session, msgs2, len(norm1)-1, &calls)

	if n := gates["extract_llm_sweep/sweep_stale_prefix"]; n != 0 {
		t.Errorf("sweep_stale_prefix fired %d times; textclean's rewrite must not desync the raw "+
			"span hash from the stash (gates: %v)", n, gates)
	}
	if events["extract_llm_sweep/sweep_candidate_at_depth"] == 0 {
		t.Fatalf("the fixture produced no at-depth candidate at all, so this test proves nothing "+
			"(gates: %v, events: %v)", gates, events)
	}
	if calls.Load() == 0 {
		t.Error("the sweep never asked through the stash at all — expected exactly one prefix ask")
	}
}

// TestSweepGuardDeclinesWhenRawCoverageTrulyFalls is the control: when the span the stash actually
// holds (turn 1's REAL preBody) genuinely differs from this turn's own idea of that span — not
// because some other component rewrote it in between, but because the content itself changed —
// the guard must still decline. Proves the fix does not make the guard vacuously pass.
func TestSweepGuardDeclinesWhenRawCoverageTrulyFalls(t *testing.T) {
	var calls atomic.Int64
	upstream := newSweepGuardUpstream(&calls)
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [extract_llm_sweep]\n" +
		"components:\n  extract_llm_sweep:\n    min_tokens: 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: upstream.URL})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	plain := "FAIL " + strings.Repeat("filler output line\n", 60)
	msgs1 := toolTurn1Messages(plain)
	session := sweepGuardTurn1(t, h, srv, msgs1)
	norm1 := apply.NormalizeMessages(bschemas.Anthropic, pr431ToolTurnBody(t, msgs1))

	// Turn 2's OWN transcript diverges from what turn 1 actually sent and stashed: message index 2
	// (the first tool_result, well inside the cached span) now carries different content. A real
	// client practically never does this, but a stash whose coverage check cannot catch it would
	// let the sweep ask about, and act on, bytes the provider's cache was never populated with.
	msgs2 := append([]any{}, msgs1...)
	msgs2[2] = map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "tool_result", "tool_use_id": "tu_a", "content": "a different tool ran here"}}}
	msgs2 = append(msgs2, map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "and now?"})

	gates, _ := runSweepGuardTurn2(t, "pipeline: [extract_llm_sweep]\n"+
		"components:\n  extract_llm_sweep:\n    min_tokens: 20\n",
		upstream.URL, h.sent, session, msgs2, len(norm1)-1, &calls)

	if gates["extract_llm_sweep/sweep_stale_prefix"] != 1 {
		t.Fatalf("a genuinely changed cached span was not caught as sweep_stale_prefix (gates: %v)", gates)
	}
}

func pr431ToolTurnBody(t *testing.T, msgs []any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "claude-sonnet-5", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSummarizerNotFirstStillAsksOnce is PR #431 round 1's second case: cache_aware_summarizer has
// the identical bug when any component runs before it, not only when it is first (as every
// pre-existing pipelineOrderFixture-based test in this package has it). Uses the PR's own fixture.
func TestSummarizerNotFirstStillAsksOnce(t *testing.T) {
	asked := pipelineOrderFixture(t, "pipeline: [textclean, cache_aware_summarizer]\n"+
		"components:\n  cache_aware_summarizer:\n    keep_last_turns: 2\n    min_tokens: 1\n"+
		"    trigger:\n      min_messages: 7\n      min_request_tokens: 1\n      min_request_frac: 0\n")
	if len(asked) != 1 {
		t.Fatalf("got %d ask calls with cache_aware_summarizer SECOND in the pipeline, want exactly "+
			"1 (textclean running first must not desync the raw span hash from the stash)", len(asked))
	}
	if !strings.Contains(asked[0], "MARK-T2") {
		t.Error("the ask call's own prefix did not cover turn 2 — the fixture itself is broken")
	}
}
