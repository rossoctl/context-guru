package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
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

// cacheinjectConversation is a Claude Code shaped body with n tool turns. The client marks the system
// block and its newest message and nothing else, so cacheinject has marks of its own to add.
func cacheinjectConversation(n int) []byte {
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "fix it"}}}}
	for k := 0; k < n; k++ {
		id := "tu_" + string(rune('a'+k))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "bash", "input": map[string]any{"command": "ls"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": bigToolOutput()}}})
	}
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	last[0].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral"}
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64, "messages": msgs,
		"system": []any{map[string]any{"type": "text", "text": "s1", "cache_control": map[string]any{"type": "ephemeral"}}}})
	return b
}

// cacheinject adds cache_control marks to messages the client did not mark. The marks lengthen the forwarded
// body. The metadata-only write path put them on the wire without recording that the message was written, so
// the operation that carries the mark was dropped: ops=0 for bodies=+37 bytes, the sum check failed on every
// request, and the event booked no saving (#938).
func TestCacheinjectMarksAreReportedAndPriced(t *testing.T) {
	for name, yaml := range map[string]string{
		"cacheinject alone":           "pipeline: [cacheinject]\n",
		"without the tail breakpoint": "pipeline: [cacheinject]\ncache:\n  tail_breakpoint: false\n",
		"with cachesplit":             "pipeline: [cachesplit, cacheinject]\n",
		// A text rewrite and a mark can land on one message: the two operations share its wire bytes.
		"with a text rewrite": "pipeline: [mask, cacheinject]\ncomponents:\n  mask:\n    keep_recent: 1\n    min_tokens: 50\n",
	} {
		t.Run(name, func(t *testing.T) {
			up := billedUpstream(t)
			defer up.Close()
			h, rec, tap := accHandlerYAML(t, up.URL, yaml)
			for n := 2; n <= 4; n++ {
				accPostBody(t, h, "sess-ci", cacheinjectConversation(n))
			}
			waitForRows(t, rec, 3)
			evs := tap.all()
			for i, e := range evs {
				if res, ok := e.AccountantResult(); ok && i > 0 {
					for _, c := range res.Checks {
						t.Errorf("request %d: check failed: %s %s", i+1, c.Check, c.Detail)
					}
				}
				if i == 0 {
					continue // the first request of a thread is held out as an estimate
				}
				if fb := e.AccountantFallback(); fb != "" {
					t.Errorf("request %d fell back: %s", i+1, fb)
				}
			}
		})
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

// A bypassed request (x-context-guru-bypass) runs no pipeline, so it leaves TokensBefore at 0, and the proxy logs it
// as an acc_fallback with size_unknown. But the client asked for no context-guru: its $0 saving is a measured $0. So
// it stores no reason, and the Overview keeps its cost in the saving share (#1004).
func TestBypassedRequestStoresNoFallback(t *testing.T) {
	up := billedUpstream(t)
	defer up.Close()
	h, rec, tap := accHandlerYAML(t, up.URL, "preset: codesafe\n")
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-bypass")
	req.Header.Set("x-context-guru-bypass", "true")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)
	evs := tap.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	e := evs[0]
	if !e.Bypassed || e.AccountantFallback() != dash.FallbackSizeUnknown {
		t.Fatalf("setup: bypassed %v, fallback %q; want true, %q", e.Bypassed, e.AccountantFallback(), dash.FallbackSizeUnknown)
	}
	if e.AccFallback != "" {
		t.Fatalf("stored acc_fallback %q on a bypassed request, want none", e.AccFallback)
	}
}

// openCodeConversation is shaped like an OpenCode request through the Vercel AI SDK Anthropic provider (OpenCode
// v1.19.5 provider/transform.ts applyCaching; @ai-sdk/anthropic convert-to-anthropic-prompt.ts): two system blocks
// with a mark each, a mark on the last block of each of the last two messages, and no mark on a tool declaration.
func openCodeConversation(turns int) []byte {
	mark := func() map[string]any { return map[string]any{"type": "ephemeral"} }
	var tools []any
	for _, n := range []string{"bash", "read", "edit", "glob", "grep", "task", "todowrite"} {
		tools = append(tools, map[string]any{"name": n, "description": strings.Repeat("Runs "+n+". ", 30),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}},
				"required": []any{"x"}, "additionalProperties": false}})
	}
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "fix the test"}}}}
	for k := 0; k < turns; k++ {
		id := "toolu_" + string(rune('a'+k))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Looking."},
				map[string]any{"type": "tool_use", "id": id, "name": "bash", "input": map[string]any{"x": "ls"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": bigToolOutput()}}})
	}
	for _, m := range msgs[max(0, len(msgs)-2):] {
		c := m.(map[string]any)["content"].([]any)
		c[len(c)-1].(map[string]any)["cache_control"] = mark()
	}
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"system": []any{
			map[string]any{"type": "text", "text": strings.Repeat("You are opencode. ", 200), "cache_control": mark()},
			map[string]any{"type": "text", "text": "<env>dir: /x</env>" + strings.Repeat(" rules.", 100), "cache_control": mark()},
		},
		"messages": msgs, "tools": tools, "tool_choice": map[string]any{"type": "auto"}})
	return b
}

// A session that runs on under another pipeline after a restart (#1169). The saved thread state holds the expand
// declaration that the first pipeline added, a change with no bytes before. The second pipeline does not add it. With
// accountant before v0.4.0 the change stayed active, so every later request failed the sum check with ops=-340 (the
// declaration's size) and bodies=0, and booked no saving. accountant#93 ends such a change when the host stops adding it.
func TestNoSumFailureAfterAPresetSwitchAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	up := billedUpstream(t)
	defer up.Close()
	run := func(yaml string, from, to int) []dash.Event {
		h, rec, tap := accHandlerOpts(t, up.URL, yaml, func(o *Options) { o.AccountantState = path })
		return postOpenCodeTurns(t, h, rec, tap, from, to)
	}
	run("preset: conservative\n", 1, 2)
	pricedAfterRestart(t, run("preset: off\n", 3, 4))
}

// The same through an adapt change: the proxy starts with conservative, the adapt apply swaps in off, and the
// restart runs off from the saved override, with the thread state saved under conservative.
func TestNoSumFailureAfterAnAdaptChangeAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	override := filepath.Join(t.TempDir(), "d.db.adapt.json")
	const base = "preset: conservative\n"
	up := billedUpstream(t)
	defer up.Close()
	opts := func(o *Options) {
		o.AccountantState, o.AdaptOverride, o.BaseConfig = path, override, base
		o.BuildConfig = func(doc []byte, e components.Emitter) (BuiltConfig, error) {
			cfg, err := config.LoadBytes(doc)
			if err != nil {
				return BuiltConfig{}, err
			}
			p, err := cfg.Build(e)
			return BuiltConfig{Pipe: p, Preset: cfg.Preset}, err
		}
	}
	h, rec, tap := accHandlerOpts(t, up.URL, base, opts)
	postOpenCodeTurns(t, h, rec, tap, 1, 2) // the expand declaration is an active change now
	if _, _, err := h.adaptWriteLocal("compaction", "test", func(string) (string, error) { return "preset: off\n", nil }); err != nil {
		t.Fatal(err)
	}
	h.Close() // saves the state and keeps the override for the next start
	h, rec, tap = accHandlerOpts(t, up.URL, base, opts)
	if got := strings.Join(h.static.Load().Pipe.Names(), ","); got != "" {
		t.Fatalf("setup: the restart runs %q, want the override's empty pipeline", got)
	}
	pricedAfterRestart(t, postOpenCodeTurns(t, h, rec, tap, 3, 4))
}

// postOpenCodeTurns sends the OpenCode conversation with from to to tool turns, waits for the rows, closes the
// handler (which saves the state) and returns the events.
func postOpenCodeTurns(t *testing.T, h *Handler, rec *dash.Recorder, tap *accTap, from, to int) []dash.Event {
	t.Helper()
	for k := from; k <= to; k++ {
		accPostBody(t, h, "ses_opencode", openCodeConversation(k))
	}
	waitForRows(t, rec, int64(to-from+1))
	h.Close()
	return tap.all()
}

// pricedAfterRestart fails when a request after the restart has no accountant result, failed a check, or fell
// back with check_sum.
func pricedAfterRestart(t *testing.T, evs []dash.Event) {
	t.Helper()
	for i, e := range evs {
		res, ok := e.AccountantResult()
		if !ok {
			t.Errorf("request %d after the restart has no accountant result (fallback %q)", i+1, e.AccountantFallback())
			continue
		}
		for _, c := range res.Checks {
			t.Errorf("request %d after the restart: check failed: %s %s", i+1, c.Check, c.Detail)
		}
		if fb := e.AccountantFallback(); fb == dash.FallbackCheckSum {
			t.Errorf("request %d after the restart fell back: %s", i+1, fb)
		}
	}
}
