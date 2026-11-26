package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/config"
)

// claudeCodeBody is a Claude-Code-shaped request: 30 tools, a big system block with a git-status tail,
// cache_control marks, and turns tool results.
func claudeCodeBody(turns int, tail string) []byte {
	var tools []any
	for i := 0; i < 30; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("tool_%02d", i),
			"description": strings.Repeat("Does a thing with files. ", 20),
			"input_schema": map[string]any{"type": "object", "$schema": "http://json-schema.org/draft-07/schema#",
				"properties":           map[string]any{"path": map[string]any{"type": "string", "description": "the path", "title": "Path"}},
				"additionalProperties": false}})
	}
	sys := []any{
		map[string]any{"type": "text", "text": "You are Claude Code."},
		map[string]any{"type": "text", "text": strings.Repeat("Follow the repo rules carefully. ", 600) +
			"\n<env>\ngitStatus: " + tail + "\n</env>", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	msgs := []any{map[string]any{"role": "user", "content": "fix the bug"}}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("tu_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "tool_01", "input": map[string]any{"path": "a.py"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": bigToolOutput()}}})
	}
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	b, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 64, "tools": tools, "system": sys, "messages": msgs})
	return b
}

// Every shipped preset is priced by accountant on a Claude-Code-shaped session: no request falls
// back to the legacy figures, and the component rows sum to the baseline delta. A new rewrite of the
// envelope that reports no operation makes the sum check fail here.
func TestEveryPresetIsPricedByAccountant(t *testing.T) {
	for _, preset := range config.PresetNames() {
		t.Run(preset, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
					`"stop_reason":"end_turn","usage":{"input_tokens":50,"output_tokens":5,`+
					`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, len(b)/8, len(b)/16)
			}))
			defer up.Close()
			h, rec, tap := accHandlerYAML(t, up.URL, "preset: "+preset+"\n")
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			const turns = 4
			for i := 1; i <= turns; i++ {
				req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
					strings.NewReader(string(claudeCodeBody(i, fmt.Sprintf("M file%d.go", i%2)))))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-context-guru-session", "s-"+preset)
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			waitForRows(t, rec, turns)
			for i, e := range tap.all() {
				r, _ := e.AccountantResult()
				if fb := e.AccountantFallback(); fb != "" {
					t.Errorf("request %d fell back: %q, checks %+v, unpriced %+v", i+1, fb, r.Checks, r.Unpriced)
					continue
				}
				var sum float64
				for _, c := range e.Components {
					sum += c.SavedUSD
				}
				if d := sum - (e.BaselineCostUSD - e.CostUSD); d > 1e-12 || d < -1e-12 {
					t.Errorf("request %d: rows sum %v, baseline delta %v", i+1, sum, e.BaselineCostUSD-e.CostUSD)
				}
			}
		})
	}
}
