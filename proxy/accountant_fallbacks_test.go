package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tooldefer"
)

// accPostBody sends body to the handler under session and returns the response status.
func accPostBody(t *testing.T, h *Handler, session string, body []byte) {
	t.Helper()
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", session)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// accNoSumFailure fails the test when an event's Result carries a failed check, or the event fell
// back for a reason that means the operation list is wrong.
func accNoSumFailure(t *testing.T, e dash.Event) {
	t.Helper()
	res, ok := e.AccountantResult()
	if !ok {
		t.Fatalf("no accountant result (fallback %q)", e.AccountantFallback())
	}
	for _, c := range res.Checks {
		t.Errorf("check failed: %s %s", c.Check, c.Detail)
	}
	if fb := e.AccountantFallback(); fb != "" {
		t.Errorf("the override must apply, fell back: %s", fb)
	}
}

// A Claude Code style request ends in a long `system` message after the client's last mark. The
// default tail breakpoint (#795) marks that message, which makes its wire bytes longer. The
// mark is an operation of its own: without it the sum check failed on every request and no saving
// was booked (#906).
func TestTailBreakpointReportsItsBytes(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	h, rec, tap := accHandlerYAML(t, up.URL, "preset: codesafe\ncache:\n  tail_breakpoint: true\n")
	build := func(n int) []byte {
		msgs := []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "reminder"},
			map[string]any{"type": "text", "text": "fix it", "cache_control": map[string]any{"type": "ephemeral"}},
		}}}
		for k := 0; k < n; k++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "tu_" + string(rune('a'+k)), "name": "bash", "input": map[string]any{"command": "ls"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu_" + string(rune('a'+k)), "content": bigToolOutput()}}})
		}
		// A string content, so the mark also turns it into one text block.
		msgs = append(msgs, map[string]any{"role": "system", "content": strings.Repeat("env line. ", 700)})
		b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64, "messages": msgs,
			"system": []any{map[string]any{"type": "text", "text": "s1", "cache_control": map[string]any{"type": "ephemeral"}}}})
		return b
	}
	accPostBody(t, h, "sess-tail", build(1))
	accPostBody(t, h, "sess-tail", build(2))
	waitForRows(t, rec, 2)
	evs := tap.all()
	for _, e := range evs {
		accNoSumFailure(t, e)
	}
	if got := evs[1].AccountantFallback(); got != "" {
		t.Fatalf("second request fell back: %s", got)
	}
}

// A small conversation that drops a large tool list: the removed declarations are many times the
// size of the messages. The plausibility bound reads message tokens, so it dropped the whole result
// with implausible_size (#907).
func TestRemovedToolDeclarationsDoNotTripThePlausibilityBound(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	h, rec, tap := accHandlerYAML(t, up.URL,
		"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    remove: [big_one, big_two]\n")
	decl := func(name string, n int) map[string]any {
		return map[string]any{"name": name, "description": strings.Repeat("Does a thing. ", n),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}
	}
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"tools":    []any{decl("bash", 3), decl("big_one", 400), decl("big_two", 400)},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	accPostBody(t, h, "sess-tools", b)
	accPostBody(t, h, "sess-tools", b)
	waitForRows(t, rec, 2)
	for i, e := range tap.all() {
		accNoSumFailure(t, e)
		if e.TokensBefore >= 100 {
			t.Fatalf("precondition: messages must be small, TokensBefore = %d", e.TokensBefore)
		}
		if i == 1 && e.BaselineCostUSD <= e.CostUSD {
			t.Errorf("no saving booked for the removed declarations: baseline %v cost %v", e.BaselineCostUSD, e.CostUSD)
		}
	}
}

// Tool deferral (`defer: auto`) marks declarations defer_loading, adds the search tool and puts a
// names list at messages[1], all after the pipeline reported its operations. The tools half is the
// envelope reporter's; the names list is its own operation. Without it every request of a deferred
// thread failed check_sum and conservation (#908).
func TestToolDeferReportsItsNamesList(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	for _, tc := range []struct {
		name string
		msgs []any
	}{
		// Claude Code's shape: a system message already at messages[1], the list is appended to it.
		{"appended", []any{map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "# Environment\nwd: /w"}}},
		// No message at index 1: the list is a new system message.
		{"inserted", []any{map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "hello"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rec, tap := accHandlerOpts(t, up.URL,
				"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n    always_defer: [mcp__big]\n",
				func(o *Options) { o.ToolDeferPath = filepath.Join(t.TempDir(), tooldefer.FileName) })
			// The provider check of deferral runs on the first request; seed it as passed.
			h.toolDefer().prober.Seed(tooldefer.ProbeKey{Upstream: up.URL, Model: "claude-sonnet-5-5", Route: "/v1/messages"},
				tooldefer.Pass, time.Now())
			decl := func(name string, n int) map[string]any {
				return map[string]any{"name": name, "description": strings.Repeat("Does a thing. ", n),
					"input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}
			}
			b, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 64,
				"tools":    []any{decl("bash", 3), decl("mcp__big", 300)},
				"messages": tc.msgs})
			accPostBody(t, h, "sess-defer", b)
			accPostBody(t, h, "sess-defer", b)
			waitForRows(t, rec, 2)
			for _, e := range tap.all() {
				accNoSumFailure(t, e)
			}
		})
	}
}
