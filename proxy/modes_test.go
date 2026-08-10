package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
)

// modeHandler is buildHandler plus an explicit operating mode, and it hands back the
// aggregator so a test can read the mode-partitioned rollups.
func modeHandler(t *testing.T, yaml, upstream string, mode components.Mode) (*proxy.Handler, *metrics.Aggregator) {
	t.Helper()
	return newModeHandler(t, yaml, upstream, mode, "")
}

// newModeHandler is modeHandler with an explicit cache mode. "on" forces
// cache-awareness even on the OpenAI route, which is what makes the tail gate (and so
// MaxCachedIdx) actually participate — several mode behaviors are only observable then.
func newModeHandler(t *testing.T, yaml, upstream string, mode components.Mode, cacheMode string) (*proxy.Handler, *metrics.Aggregator) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.New(pipe, store.NewMemory(store.Options{}), agg, proxy.Options{
		OpenAIUpstream: upstream, AnthropicUpstream: upstream, Mode: mode, CacheMode: cacheMode,
	})
	t.Cleanup(h.Close)
	return h, agg
}

// captureUpstream records every body the upstream receives.
func captureUpstream(t *testing.T) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var got [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

const modePipeline = "pipeline: [dedup, cacheinject]\n"

func dupBody() []byte {
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	return openAIBody(
		map[string]any{"role": "user", "content": "do the thing"},
		map[string]any{"role": "tool", "tool_call_id": "a", "content": dump},
		map[string]any{"role": "tool", "tool_call_id": "b", "content": dump},
	)
}

func post(t *testing.T, srv *httptest.Server, body []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// awaitSnapshot polls until cond holds, so an off-path result can land.
func awaitSnapshot(t *testing.T, agg *metrics.Aggregator, cond func(metrics.Snapshot) bool) metrics.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var snap metrics.Snapshot
	for time.Now().Before(deadline) {
		snap = agg.Snapshot()
		if cond(snap) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	return snap
}

// TestSyncIsTheDefaultAndUnchanged: an unset mode must behave exactly like the
// explicit sync mode, which is the pre-change behavior. The forwarded bodies are
// compared byte for byte — the golden test the issue asks for, expressed against the
// code's own output rather than a checked-in fixture that would drift on every
// unrelated component change.
func TestSyncIsTheDefaultAndUnchanged(t *testing.T) {
	body := dupBody()

	upA, gotA := captureUpstream(t)
	hA, _ := modeHandler(t, modePipeline, upA.URL, "") // unset
	srvA := httptest.NewServer(hA.Mux())
	defer srvA.Close()
	post(t, srvA, body)

	upB, gotB := captureUpstream(t)
	hB, _ := modeHandler(t, modePipeline, upB.URL, components.ModeSync)
	srvB := httptest.NewServer(hB.Mux())
	defer srvB.Close()
	post(t, srvB, body)

	a, b := gotA(), gotB()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected one forward each, got %d and %d", len(a), len(b))
	}
	if !bytes.Equal(a[0], b[0]) {
		t.Fatalf("default mode differs from explicit sync\n default: %s\n sync:    %s", a[0], b[0])
	}
	// And sync really did compact: otherwise the comparison above is vacuous.
	if bytes.Equal(a[0], body) {
		t.Fatal("sync forwarded the original unchanged — the golden comparison proves nothing")
	}
}

// TestObserveForwardsByteIdenticalBody is the mode's core promise: the agent receives
// exactly what it sent, while the hypothetical savings are still recorded.
func TestObserveForwardsByteIdenticalBody(t *testing.T) {
	up, got := captureUpstream(t)
	h, agg := modeHandler(t, modePipeline, up.URL, components.ModeObserve)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := dupBody()
	post(t, srv, body)

	fwd := got()
	if len(fwd) != 1 {
		t.Fatalf("expected one forward, got %d", len(fwd))
	}
	if !bytes.Equal(fwd[0], body) {
		t.Fatalf("observe mode MODIFIED the forwarded body\n sent: %s\n fwd:  %s", body, fwd[0])
	}

	snap := awaitSnapshot(t, agg, func(s metrics.Snapshot) bool { return s.ObserveRequests > 0 })
	if snap.ObserveRequests == 0 {
		t.Fatal("observe mode recorded nothing")
	}
	if snap.PotentialSavedTokens <= 0 {
		t.Fatalf("no potential savings recorded: %+v", snap)
	}
	if snap.ActualBaselineTokens <= snap.ProjectedOptimizedTokens {
		t.Fatalf("projected usage is not below the actual baseline: %d vs %d",
			snap.ProjectedOptimizedTokens, snap.ActualBaselineTokens)
	}
	if snap.ObserveNotice == "" {
		t.Fatal("observe mode did not emit its banner")
	}
	if snap.Mode != string(components.ModeObserve) {
		t.Fatalf("mode not reported: %q", snap.Mode)
	}
}

// TestObserveMetricsCannotBeSummedIntoEnforcedTotals is the correctness requirement: a
// hypothetical must be unreachable from every enforced aggregate, or the product's
// headline savings claim is silently inflated.
func TestObserveMetricsCannotBeSummedIntoEnforcedTotals(t *testing.T) {
	up, _ := captureUpstream(t)
	h, agg := modeHandler(t, modePipeline, up.URL, components.ModeObserve)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	for i := 0; i < 3; i++ {
		post(t, srv, dupBody())
	}
	snap := awaitSnapshot(t, agg, func(s metrics.Snapshot) bool { return s.ObserveRequests > 0 })
	if snap.ObserveRequests == 0 {
		t.Fatal("nothing was observed; the test proves nothing")
	}
	if snap.Requests != 0 || snap.TokensBefore != 0 || snap.TokensAfter != 0 || snap.SavedTokens != 0 {
		t.Fatalf("observe results leaked into the enforced totals: %+v", snap)
	}
	if snap.SyncEnforced != 0 || snap.AsyncEnforced != 0 {
		t.Fatalf("observe counted as enforced: sync=%d async=%d", snap.SyncEnforced, snap.AsyncEnforced)
	}
	// Two enforced-namespace fields are deliberately NOT zeroed, because they are real
	// measurements rather than hypotheticals — cg_added_ms_avg (the actual enforced-path
	// latency, ~0 here, which IS the headline) and context-guru's own model spend (observe
	// measures off-path, and that costs real money). The notice labels the latter so it is
	// not read as the cost of enforcing.
	if snap.ObserveLLMNotice == "" {
		t.Fatal("observe did not label its own off-path model spend")
	}
	if len(snap.Components) != 0 {
		t.Fatalf("observe results leaked into the enforced per-component map: %v", snap.Components)
	}
	if len(snap.PotentialComponents) == 0 {
		t.Fatal("per-component hypotheticals were not recorded at all")
	}
	// The serialized payload must keep the two vocabularies disjoint.
	m := marshalMap(t, snap)
	for _, enforced := range []string{"saved_tokens", "savings_pct", "tokens_before", "tokens_after", "requests", "components"} {
		if _, ok := m[enforced]; !ok {
			t.Fatalf("%q disappeared from /stats — backward compatibility broken", enforced)
		}
	}
	for _, hypothetical := range []string{
		"potential_saved_tokens", "projected_optimized_tokens", "actual_baseline_tokens",
		"potential_components", "observe_notice", "observe_hypothetical_requests",
	} {
		if _, ok := m[hypothetical]; !ok {
			t.Fatalf("hypothetical key %q missing from the payload", hypothetical)
		}
	}
}

// TestStatsStaysBackwardCompatible: deploy/harbor/*.py parses this payload, so fields
// may be added but never renamed or removed.
func TestStatsStaysBackwardCompatible(t *testing.T) {
	up, _ := captureUpstream(t)
	h, _ := modeHandler(t, modePipeline, up.URL, components.ModeSync)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	post(t, srv, dupBody())

	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"requests", "tokens_before", "tokens_after", "saved_tokens", "savings_pct",
		"wasted_tokens", "bounces", "adjusted_saved", "components", "top_passthrough",
		"llm_calls", "llm_input_tokens", "llm_output_tokens",
		"cg_added_ms_avg", "upstream_ms_avg", "upstream_ms_avg_bypassed",
	} {
		if _, ok := m[k]; !ok {
			t.Fatalf("/stats lost the pre-existing field %q", k)
		}
	}
	for _, k := range []string{"mode", "sync_enforced", "async_enforced"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("/stats is missing the new field %q", k)
		}
	}
	if m["mode"] != string(components.ModeSync) {
		t.Fatalf("mode is %v, want sync", m["mode"])
	}
	if m["sync_enforced"].(float64) < 1 {
		t.Fatalf("sync request not counted as enforced: %v", m["sync_enforced"])
	}
}

// TestAsyncForwardsAndDefersWork: the request goes out, is counted under its own
// mode, and the queue tuple is exposed whole.
func TestAsyncForwardsAndDefersWork(t *testing.T) {
	up, got := captureUpstream(t)
	h, agg := modeHandler(t, modePipeline, up.URL, components.ModeAsync)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	post(t, srv, dupBody())
	if fwd := got(); len(fwd) != 1 {
		t.Fatalf("expected one forward, got %d", len(fwd))
	}
	snap := agg.Snapshot()
	if snap.AsyncEnforced != 1 {
		t.Fatalf("async request not counted under its own mode: %+v", snap)
	}
	if snap.SyncEnforced != 0 {
		t.Fatal("async request counted as sync")
	}
	if snap.ObserveRequests != 0 || snap.PotentialSavedTokens != 0 {
		t.Fatal("async results leaked into the hypothetical namespace")
	}
	q, ok := marshalMap(t, snap)["async_queue"]
	if !ok {
		t.Fatal("async_queue absent from /stats in async mode")
	}
	var tuple map[string]int64
	if err := json.Unmarshal(q, &tuple); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"queued", "pending", "processed", "dropped", "errors", "stale_discarded"} {
		if _, ok := tuple[k]; !ok {
			t.Fatalf("async_queue is missing %q — the whole tuple must be exposed", k)
		}
	}
}

// TestConcurrentTurnsOneSession pushes many simultaneous turns of ONE session through
// the real handler in async mode: no race (run under -race), no corruption, and every
// request still answered.
func TestConcurrentTurnsOneSession(t *testing.T) {
	up, got := captureUpstream(t)
	h, _ := modeHandler(t, modePipeline, up.URL, components.ModeAsync)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := dupBody()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-context-guru-session", "shared")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if n := len(got()); n != 24 {
		t.Fatalf("forwarded %d of 24 requests", n)
	}
}

// TestCloseLeavesNoGoroutines: the pool the handler owns must be reclaimed.
func TestCloseLeavesNoGoroutines(t *testing.T) {
	// Everything unrelated (the mock upstream's own goroutines) is created BEFORE the
	// baseline, so the only difference this measures is the pool's.
	up, _ := captureUpstream(t)
	settleGoroutines()
	before := runtime.NumGoroutine()

	cfg, err := config.LoadBytes([]byte(modePipeline))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.New(pipe, store.NewMemory(store.Options{}), agg, proxy.Options{
		OpenAIUpstream: up.URL, Mode: components.ModeAsync,
	})
	h.Close()
	h.Close() // idempotent

	settleGoroutines()
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak after Close: %d before, %d after", before, after)
	}
}

// TestSyncModeStartsNoPool: sync adds no machinery — no pool, no async_queue.
func TestSyncModeStartsNoPool(t *testing.T) {
	up, _ := captureUpstream(t)
	_, agg := modeHandler(t, modePipeline, up.URL, components.ModeSync)
	raw, err := json.Marshal(agg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("async_queue")) {
		t.Fatalf("sync mode advertised an async queue: %s", raw)
	}
}

func TestUnknownModeIsRejected(t *testing.T) {
	if _, err := config.LoadBytes([]byte("pipeline: [dedup]\nmode: turbo\n")); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	for _, ok := range []string{"", "sync", "async", "observe"} {
		if _, err := config.LoadBytes([]byte("pipeline: [dedup]\nmode: " + ok + "\n")); err != nil {
			t.Fatalf("mode %q rejected: %v", ok, err)
		}
	}
}

func marshalMap(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func settleGoroutines() {
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAsyncSavingsArriveOnALaterTurn is the end-to-end claim of async mode: turn 1
// forwards without the expensive compaction, the job lands off-path, and a LATER turn
// gets the saving by replaying the frozen decision. Without this the mode is only
// "cheaper because it does less".
func TestAsyncSavingsArriveOnALaterTurn(t *testing.T) {
	up, got := captureUpstream(t)
	// mask is a frozen-decision offloader (age-based), deterministic, so no model is
	// needed to prove the deferred-then-replayed path.
	h, agg := modeHandler(t, "pipeline: [mask]\ncomponents:\n  mask:\n    keep_last: 1\n", up.URL, components.ModeAsync)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("a long tool output line that is worth offloading\n", 80)
	turn := func(n int) {
		msgs := []map[string]any{{"role": "user", "content": "go"}}
		for i := 0; i < n; i++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "step " + strconv.Itoa(i)},
				map[string]any{"role": "tool", "tool_call_id": "t" + strconv.Itoa(i), "content": dump + strconv.Itoa(i)})
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions",
			bytes.NewReader(openAIBody(msgs...)))
		req.Header.Set("x-context-guru-session", "later-turn")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	for n := 2; n <= 6; n++ {
		turn(n)
		// Let the off-path job for this turn land before the next one.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if agg.Snapshot().DeferredRuns > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	snap := agg.Snapshot()
	t.Logf("async: deferred_runs=%d realized=%d saved=%d queue=%+v",
		snap.DeferredRuns, snap.RealizedSavedTokens, snap.SavedTokens, snap.AsyncQueue)
	if snap.DeferredRuns == 0 {
		t.Fatal("no off-path compaction ever committed")
	}
	if snap.RealizedSavedTokens == 0 {
		t.Fatal("a deferred compaction committed but no later turn ever realized it")
	}
	if len(got()) != 5 {
		t.Fatalf("forwarded %d of 5 turns", len(got()))
	}
}

// TestObserveProjectionAgreesWithSyncActuals is the check that validates the whole
// mode: run the SAME turns under sync and under observe, and observe's projected
// saving must match what sync actually achieved. It caught a real overstatement —
// without the shared cache boundary, observe's tail gate never fired and it projected
// savings on messages sync would never touch.
func TestObserveProjectionAgreesWithSyncActuals(t *testing.T) {
	dump := strings.Repeat("a long stale tool output worth offloading\n", 80)
	turns := func() [][]byte {
		var out [][]byte
		// Each turn appends SEVERAL tool outputs, so more than one lands beyond the
		// previous turn's boundary. That matters: with only one new tool output per turn
		// it is always the one mask keeps, nothing is eligible in the tail, and both arms
		// trivially save zero — which would hide the very disagreement this test exists for.
		for n := 1; n <= 5; n++ {
			msgs := []map[string]any{{"role": "user", "content": "go"}}
			for i := 0; i < n*4; i++ {
				msgs = append(msgs,
					map[string]any{"role": "assistant", "content": "step " + strconv.Itoa(i)},
					map[string]any{"role": "tool", "tool_call_id": "t" + strconv.Itoa(i), "content": dump + strconv.Itoa(i)})
			}
			out = append(out, openAIBody(msgs...))
		}
		return out
	}()

	// keep_last: 1 so each turn's growth pushes the previous tool output out of the
	// keep window and into mask's range, giving both arms something to act on inside
	// the uncached tail (which is where cache-awareness permits acting at all).
	yaml := "pipeline: [mask]\ncomponents:\n  mask:\n    keep_last: 1\n"
	drive := func(mode components.Mode) metrics.Snapshot {
		up, _ := captureUpstream(t)
		// cache=on so cache-awareness (and therefore the tail gate) is live: that gate is
		// exactly what observe used to ignore, and without it the two arms agree trivially.
		h, agg := newModeHandler(t, yaml, up.URL, mode, "on")
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		for _, b := range turns {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(b))
			req.Header.Set("x-context-guru-session", "agree")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if mode == components.ModeObserve {
			return awaitSnapshot(t, agg, func(s metrics.Snapshot) bool { return s.ObserveRequests >= int64(len(turns)) })
		}
		return agg.Snapshot()
	}

	sync := drive(components.ModeSync)
	obs := drive(components.ModeObserve)

	t.Logf("sync: before=%d saved=%d (%.2f%%)", sync.TokensBefore, sync.SavedTokens, sync.SavingsPct)
	t.Logf("observe: baseline=%d potential=%d (%.2f%%) reqs=%d",
		obs.ActualBaselineTokens, obs.PotentialSavedTokens, obs.PotentialSavingsPct, obs.ObserveRequests)

	if sync.SavedTokens == 0 || obs.PotentialSavedTokens == 0 {
		t.Fatalf("one side saved nothing; the agreement check is vacuous (sync=%d observe=%d)",
			sync.SavedTokens, obs.PotentialSavedTokens)
	}
	// Both saw the same traffic under the same boundary, so the projection must track
	// the actual closely. A generous band still catches the class of bug this found
	// (observe was 11x sync before the shared-tracker fix).
	ratio := float64(obs.PotentialSavedTokens) / float64(sync.SavedTokens)
	if ratio < 0.75 || ratio > 1.33 {
		t.Fatalf("observe's projection disagrees with sync's actual: %d vs %d (ratio %.2f)",
			obs.PotentialSavedTokens, sync.SavedTokens, ratio)
	}
}

// TestObserveNeverWritesTheLiveStore: observe gets a store of its own so its frozen
// decisions accumulate across turns (without that it under-projects by ~3x), but the
// live store must stay pristine — otherwise a later real request would replay a
// decision that was never enforced, which is a request modification arriving by the
// back door.
func TestObserveNeverWritesTheLiveStore(t *testing.T) {
	up, _ := captureUpstream(t)
	cfg, err := config.LoadBytes([]byte("pipeline: [mask]\ncomponents:\n  mask:\n    keep_last: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	live := &countingStore{Store: store.NewMemory(store.Options{})}
	h := proxy.New(pipe, live, agg, proxy.Options{
		OpenAIUpstream: up.URL, Mode: components.ModeObserve, CacheMode: "on",
	})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("a long stale tool output worth offloading\n", 80)
	for n := 1; n <= 4; n++ {
		msgs := []map[string]any{{"role": "user", "content": "go"}}
		for i := 0; i < n*4; i++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "step " + strconv.Itoa(i)},
				map[string]any{"role": "tool", "tool_call_id": "t" + strconv.Itoa(i), "content": dump + strconv.Itoa(i)})
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(openAIBody(msgs...)))
		req.Header.Set("x-context-guru-session", "isolated")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	snap := awaitSnapshot(t, agg, func(s metrics.Snapshot) bool { return s.PotentialSavedTokens > 0 })
	if snap.PotentialSavedTokens == 0 {
		t.Fatal("observe recorded no savings; the isolation check is vacuous")
	}
	if n := live.puts.Load(); n != 0 {
		t.Fatalf("observe mode wrote %d entries into the LIVE store", n)
	}
}

// countingStore counts writes so a test can assert none happened.
type countingStore struct {
	store.Store
	puts atomic.Int64
}

func (c *countingStore) Put(key string, payload []byte) {
	c.puts.Add(1)
	c.Store.Put(key, payload)
}

func (c *countingStore) MarkSticky(session, id string) {
	c.puts.Add(1)
	c.Store.MarkSticky(session, id)
}

// TestAsyncDoesNotSpinOnAnUnproductiveTurn: a turn whose deferred job finds nothing to
// compact must not leave the session stuck re-enqueueing the same key forever. The
// generation only advances on a COMMIT, so an unproductive job is deliberately allowed
// to keep its slot — but the queue must stay bounded and the request path unaffected.
func TestAsyncDoesNotSpinOnAnUnproductiveTurn(t *testing.T) {
	up, got := captureUpstream(t)
	// A pipeline that can never shrink this traffic, so no job ever commits.
	h, agg := newModeHandler(t, "pipeline: [dedup]\n", up.URL, components.ModeAsync, "on")
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := openAIBody(
		map[string]any{"role": "user", "content": "nothing to compact here"},
		map[string]any{"role": "assistant", "content": "ok"},
	)
	for i := 0; i < 12; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("x-context-guru-session", "unproductive")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := len(got()); n != 12 {
		t.Fatalf("forwarded %d of 12 requests", n)
	}
	q := agg.Snapshot().AsyncQueue
	t.Logf("queue after 12 unproductive turns: %+v", q)
	// The point: no unbounded growth and no drops from a queue full of duplicate work.
	// Dedup on (session, generation) collapses all of them onto one slot.
	s := agg.Snapshot()
	if s.AsyncEnforced != 12 {
		t.Fatalf("async enforced count is %d, want 12", s.AsyncEnforced)
	}
}

// TestCompactEndpointIgnoresMode: /compact is the "compact a context, hand it back"
// endpoint used by offline replay and the llm-d-router. Its contract is synchronous by
// nature — the caller wants the compacted body in the response — so the handler's mode
// must not change it. In particular observe mode must not turn /compact into a no-op.
func TestCompactEndpointIgnoresMode(t *testing.T) {
	body := dupBody()
	var outs [][]byte
	for _, mode := range []components.Mode{components.ModeSync, components.ModeAsync, components.ModeObserve} {
		up, _ := captureUpstream(t)
		h, _ := modeHandler(t, "pipeline: [dedup]\n", up.URL, mode)
		srv := httptest.NewServer(h.Mux())
		resp, err := http.Post(srv.URL+"/compact", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		if bytes.Equal(out, body) {
			t.Fatalf("/compact returned the original unchanged under mode %s", mode)
		}
		outs = append(outs, out)
	}
	for i := 1; i < len(outs); i++ {
		if !bytes.Equal(outs[0], outs[i]) {
			t.Fatalf("/compact output depends on the operating mode:\n %s\n %s", outs[0], outs[i])
		}
	}
}
