package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// fakeCheapModel is an Anthropic-dialect endpoint for the COMPACTION model: it returns a
// usable summary and a fixed, recognisable token bill.
func fakeCheapModel(t *testing.T, in, out int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"text","text":"<summary>the span, summarized</summary>"}],
			"usage":{"input_tokens":%d,"output_tokens":%d,
			         "cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`, in, out)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// cgLLMHandler wires a recorder + a config whose summarizer uses the "config"-source
// cheap model, so the request path can be made to spend our own model money on purpose.
func cgLLMHandler(t *testing.T, upstream string, cheap components.Model) (*Handler, *dash.Recorder) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(
		"pipeline: [summarize]\ncomponents:\n  summarize:\n    keep_last: 1\n" +
			"    start_from_message: 0\n    min_tokens: 1\n    model:\n      source: config\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{
		DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: upstream,
		Dashboard:         rec,
		Prices:            fixedPricer{},
		CheapModel:        cheap,
	})
	t.Cleanup(h.Close)
	return h, rec
}

// summarizableRequest is a four-turn transcript, so that with keep_last: 1 the span
// the summarizer folds actually contains the big tool result — i.e. the LLM call
// really happens and the row really has our own model cost on it.
func summarizableRequest() string {
	b, _ := json.Marshal(map[string]any{
		"model":      "aws/claude-sonnet-5",
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "please fix the failing test"},
			map[string]any{"role": "assistant", "content": "let me run the tests"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": bigToolOutput()},
			}},
			map[string]any{"role": "assistant", "content": "working on it"},
		},
	})
	return string(b)
}

func postChat(t *testing.T, srv *httptest.Server, session string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(summarizableRequest()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", session)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
}

// Finding B. cg_llm_cost_usd was the DELTA of process-global cheap-model counters across
// a request's lifetime, so any other tenant's compaction call that happened while this
// request was in flight was billed to this request's row — and from there into
// tenant_spend, MonthToDateUSD and cg_tenant_cg_llm_cost_usd. It is also a side channel:
// a tenant can watch its own rows to infer when other tenants are compacting.
//
// The other tenant's call is made from inside the upstream handler, i.e. strictly within
// the window between newCapture and finish, which is what makes this deterministic.
func TestCGLLMCostIsNotChargedToAnotherTenantsRequest(t *testing.T) {
	cheap, cheapCalls := fakeCheapModel(t, 10_000, 2_000)
	// The other tenant's compaction client: same process, its own request context.
	other := cheapmodel.Anthropic{BaseURL: cheap.URL, Model: "m", APIKey: "unused-by-the-fake"}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// tenant-b's extract_llm call, mid-flight of tenant-a's request.
		if _, err := other.Complete(context.Background(), "summarize this"); err != nil {
			t.Errorf("the other tenant's cheap-model call failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer up.Close()

	// No cheap model of its own: tenant-a's pipeline cannot spend a cent here.
	h, rec := cgLLMHandler(t, up.URL, nil)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	before, _, _ := cheapmodel.Usage()
	postChat(t, srv, "sess-a")
	waitForRows(t, rec, 1)
	if cheapCalls.Load() != 1 {
		t.Fatalf("the other tenant made %d cheap-model calls; the test needs exactly 1", cheapCalls.Load())
	}

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("captured %d rows; want 1", page.Total)
	}
	if got := page.Requests[0].CGLLMCostUSD; got != 0 {
		t.Errorf("this request's row was charged $%v of context-guru LLM cost, but its own "+
			"pipeline had no model at all — it is being billed for another tenant's "+
			"10000-in/2000-out compaction call", got)
	}

	// /stats keeps its process-wide total: the fix scopes ATTRIBUTION, it does not stop
	// counting what this proxy spent.
	if after, _, _ := cheapmodel.Usage(); after <= before {
		t.Errorf("process-wide cheap-model call count did not move (%d -> %d); /stats and the "+
			"benchmark harness read it", before, after)
	}
}

// The other half of the same property: a request's OWN compaction spend must still land
// on its row. Without this, "always report 0" would pass the test above.
func TestCGLLMCostIsChargedToTheRequestThatSpentIt(t *testing.T) {
	cheap, cheapCalls := fakeCheapModel(t, 10_000, 2_000)
	up := fakeUpstream(t)
	defer up.Close()

	h, rec := cgLLMHandler(t, up.URL,
		cheapmodel.Anthropic{BaseURL: cheap.URL, Model: "m", APIKey: "unused-by-the-fake"})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	postChat(t, srv, "sess-own")
	waitForRows(t, rec, 1)
	if cheapCalls.Load() == 0 {
		t.Fatal("the summarizer never called the cheap model; the assertion below would be vacuous")
	}

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Requests[0].CGLLMCostUSD; got <= 0 {
		t.Errorf("cg_llm_cost_usd = %v after this request's own summarizer spent "+
			"10000-in/2000-out; our own model cost stopped being reported", got)
	}
}
