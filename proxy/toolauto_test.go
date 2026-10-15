package proxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
)

func fatTool(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"%s job. %s","input_schema":{"type":"object"}}`,
		name, name, strings.Repeat("Detailed usage notes. ", 80))
}

// The model calls the restore stub on turn 1; the proxy answers in band, re-declares the tool on the
// continuation, and the client sees only the finished turn.
func TestToolfilterAutoRestoreInBand(t *testing.T) {
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := up.record(r)
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude","content":[` +
				`{"type":"tool_use","id":"r1","name":"context_guru_tools","input":{"names":["hidden_b"]}}],` +
				`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","model":"claude","content":[` +
			`{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	cfg, err := config.LoadBytes([]byte("pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    auto_remove: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.New(pipe, store.NewMemory(store.Options{}), agg, proxy.Options{
		AnthropicUpstream: upstream.URL,
		AutoToolEligible:  func(string) []string { return []string{"hidden_a", "hidden_b"} },
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := `{"model":"claude-sonnet-5","max_tokens":64,"tools":[` + strings.Join([]string{
		fatTool("keep_me"), fatTool("hidden_a"), fatTool("hidden_b")}, ",") +
		`],"messages":[{"role":"user","content":"go"}]}`
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if up.hits() != 2 {
		t.Fatalf("want 2 upstream rounds, got %d; client got %s", up.hits(), got)
	}
	names := func(b []byte) string {
		var n []string
		gjson.GetBytes(b, "tools.#.name").ForEach(func(_, v gjson.Result) bool { n = append(n, v.String()); return true })
		return strings.Join(n, ",")
	}
	if g := names(up.body(1)); !strings.HasPrefix(g, "keep_me,context_guru_tools") || strings.Contains(g, "hidden_") {
		t.Errorf("round 1 tools: %s", g)
	}
	if g := names(up.body(2)); !strings.HasPrefix(g, "keep_me,hidden_b,context_guru_tools") || strings.Contains(g, "hidden_a") {
		t.Errorf("continuation tools: %s", g)
	}
	if !strings.Contains(string(up.body(2)), "Restored: hidden_b") {
		t.Errorf("stub call not answered upstream: %s", up.body(2))
	}
	if strings.Contains(string(got), "context_guru_tools") || !strings.Contains(string(got), "done") {
		t.Errorf("client should see only the finished turn: %s", got)
	}
}
