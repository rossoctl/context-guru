package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// sseUpstream streams one assistant turn as events: a server-side search that loads Monitor, then
// text. It answers a probe as JSON.
type sseUpstream struct{}

func (sseUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	if probe := gjson.GetBytes(b, "tools.1.name").String(); strings.HasPrefix(probe, "cg_probe_") {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"tool_search_tool_result","tool_use_id":"s","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":%q}]}},`+
			`{"type":"tool_use","id":"t","name":%q,"input":{"code":7}}],"usage":{"input_tokens":10,"output_tokens":5}}`, probe, probe)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(typ, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"Monitor\",\"limit\":1}"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"Monitor"}]}}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"ok"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":2}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`)
	ev("message_stop", `{"type":"message_stop"}`)
}

// #871 defect 2. A streamed turn that takes the SSE splice path (the request advertises the
// expand tool, as ultrahigh's does) counts its search in the ledger, and so does the
// stream-through path (a request without the proxy's own tools).
func TestToolDeferLedgerCountsSearchesOnEveryPath(t *testing.T) {
	us := httptest.NewServer(sseUpstream{})
	defer us.Close()
	for _, tc := range []struct {
		name, doc, inject string
	}{
		{"splice", "preset: ultrahigh\n", ""},
		// No offloader and no sweep: the request carries no proxy tool, so the stream goes through.
		{"stream-through", "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n", "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := config.LoadBytes([]byte(tc.doc))
			agg := metrics.NewAggregator()
			pipe, _ := cfg.Build(agg)
			h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: us.URL,
				ToolDeferPath: filepath.Join(t.TempDir(), tooldefer.FileName), Prices: tdPrices{}, InjectExpand: tc.inject})
			defer h.Close()
			ds := h.toolDefer()
			for i, s := range []string{"p1", "p2", "p3"} {
				ds.hist.Observe("", s, "/w", tdBody(), int64(i+1))
			}
			ds.prober.Seed(tooldefer.ProbeKey{Upstream: us.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"}, tooldefer.Pass, time.Now())
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			b := bytes.Replace(tdBody(), []byte(`"max_tokens":64,`), []byte(`"max_tokens":64,"stream":true,`), 1)
			resp, out := tdPost(t, srv, b)
			if resp.StatusCode != http.StatusOK || !strings.Contains(out, "tool_search_tool_result") {
				t.Fatalf("status %d: %s", resp.StatusCode, out)
			}
			st := ds.ledger.Snapshot()
			if st.Requests != 1 || st.Searches != 1 {
				t.Fatalf("ledger = %+v, want 1 request with 1 search", st)
			}
		})
	}
}
