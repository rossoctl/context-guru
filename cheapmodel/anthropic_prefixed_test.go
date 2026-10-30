package cheapmodel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
)

// THE DEFECT: Claude Code sends max_tokens=32000 with thinking.budget_tokens=31999, and
// CompletePrefixed used to overwrite max_tokens with the fixed PrefixAskMaxTokens (16000)
// without looking at the budget already on the body — 16000 is not greater than 31999, so
// every prefix ask on such a session 400'd with "max_tokens must be greater than
// thinking.budget_tokens" (the live failure this test reproduces offline). See
// thinkingAdjustedMaxTokens's doc comment for why the fix reads the budget rather than
// stripping or resizing the thinking block.
func TestCompletePrefixedRaisesMaxTokensAboveTheThinkingBudget(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],`+
			`"usage":{"input_tokens":40,"output_tokens":10,"cache_read_input_tokens":19595}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":32000,` +
		`"thinking":{"type":"enabled","budget_tokens":31999},` +
		`"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	reply, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge")
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if reply != `{"verdicts":[]}` {
		t.Fatalf("reply = %q", reply)
	}

	// THE INVARIANT ITSELF: whatever max_tokens went out, it must clear the budget already on
	// the wire, or this test is reproducing nothing and the live 400 would still happen.
	outMax := gjson.GetBytes(got, "max_tokens").Int()
	outBudget := gjson.GetBytes(got, "thinking.budget_tokens").Int()
	if outMax <= outBudget {
		t.Fatalf("sent max_tokens=%d, budget_tokens=%d: still invalid, the provider would 400 "+
			"on this exact body", outMax, outBudget)
	}

	// THE THINKING BLOCK ITSELF MUST BE BYTE-IDENTICAL. A changed thinking.type or
	// thinking.budget_tokens invalidates the cached prefix (see thinkingAdjustedMaxTokens's doc
	// comment), which defeats the entire reason CompletePrefixed exists.
	if got2 := gjson.GetBytes(got, "thinking").String(); got2 != `{"type":"enabled","budget_tokens":31999}` {
		t.Fatalf("the thinking block was changed: %s", got2)
	}

	// EVERYTHING ELSE besides max_tokens, the appended message, and `stream` is untouched.
	if gjson.GetBytes(got, "model").String() != "claude-sonnet-5" {
		t.Fatalf("model changed: %s", got)
	}
	if n := gjson.GetBytes(got, "messages").Array(); len(n) != 2 ||
		n[0].Get("content").String() != "carry on" || n[1].Get("content").String() != "judge" {
		t.Fatalf("messages array was not preserved plus one appended turn: %s", got)
	}
	if gjson.GetBytes(got, "stream").Exists() {
		t.Fatalf("stream was not deleted: %s", got)
	}
}

// THE CEILING BUG, found live on PR #406's review: thinkingAdjustedMaxTokens's raw
// budget_tokens + reply has no upper bound, so a large thinking budget can push max_tokens past
// the MODEL's own output cap -- a new 400 with a different message ("max_tokens: 79999 > 64000,
// which is the maximum allowed number of output tokens") in place of the one this fix exists to
// avoid. Claude Code with CLAUDE_CODE_MAX_OUTPUT_TOKENS=64000 and MAX_THINKING_TOKENS=63999 on
// claude-haiku-4-5 is exactly this shape, and 64000/63999 was measured live: OK, reply "YES",
// cache_read=16230, cache_write=0. See thinkingAdjustedMaxTokens's doc comment for the full
// trade-off this ceiling accepts.
func TestCompletePrefixedCapsAtTheBodysOwnMaxTokensWhenWantWouldExceedTheModelsCap(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],"usage":{}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-haiku-4-5","max_tokens":64000,` +
		`"thinking":{"type":"enabled","budget_tokens":63999},` +
		`"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-haiku-4-5"}
	if _, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge"); err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	outMax := gjson.GetBytes(got, "max_tokens").Int()
	// The reviewer's exact ask: a result <= 64000 (the model's cap, read via the body's own
	// max_tokens) and > 63999 (still clears the thinking budget).
	if outMax > 64000 || outMax <= 63999 {
		t.Fatalf("max_tokens = %d, want in (63999, 64000]", outMax)
	}
	if got2 := gjson.GetBytes(got, "thinking").String(); got2 != `{"type":"enabled","budget_tokens":63999}` {
		t.Fatalf("the thinking block was changed: %s", got2)
	}
}

// thinking.type "adaptive" carries no budget_tokens at all, and must pass through exactly as
// PrefixAskMaxTokens always has -- this pins that the new budget-reading logic does not touch
// the ordinary case.
func TestCompletePrefixedLeavesAdaptiveThinkingUnaffected(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],"usage":{}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":4096,` +
		`"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	if _, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge"); err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if got2 := gjson.GetBytes(got, "max_tokens").Int(); got2 != PrefixAskMaxTokens {
		t.Fatalf("max_tokens = %d, want the unadjusted %d (adaptive carries no budget)",
			got2, PrefixAskMaxTokens)
	}
	if got2 := gjson.GetBytes(got, "thinking").String(); got2 != `{"type":"adaptive"}` {
		t.Fatalf("the thinking block was changed: %s", got2)
	}
}

// No thinking block at all -- the ordinary non-Claude-Code case -- must also be unaffected.
func TestCompletePrefixedLeavesNoThinkingUnaffected(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],"usage":{}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":4096,` +
		`"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	if _, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge"); err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if got2 := gjson.GetBytes(got, "max_tokens").Int(); got2 != PrefixAskMaxTokens {
		t.Fatalf("max_tokens = %d, want the unadjusted %d", got2, PrefixAskMaxTokens)
	}
	if gjson.GetBytes(got, "thinking").Exists() {
		t.Fatalf("a thinking block was invented where the body had none: %s", got)
	}
}

// A CUT-OFF REPLY (stop_reason: "max_tokens") IS RETURNED AS TEXT, NOT AS AN ERROR. The sweep
// (extract_sweep.go) already detects this itself for free -- extract.ParseVerdicts fails on it
// and reports ReplyWasTruncated=true, so the sweep declines under sweep_reply_truncated with no
// extra call. Turning it into an error here instead would route it through sweep_ask_failed,
// which runs a second, full-price fallback by default -- a cost regression a prior version of
// this fix introduced and review caught.
func TestCompletePrefixedReturnsATruncatedReplyRatherThanAnError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"[{\"i\":0"}],`+
			`"stop_reason":"max_tokens","usage":{"input_tokens":40,"output_tokens":16000}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-haiku-4-5","max_tokens":32000,` +
		`"thinking":{"type":"enabled","budget_tokens":31999},` +
		`"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-haiku-4-5"}
	reply, usage, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge")
	if err != nil {
		t.Fatalf("a truncated reply was turned into an error: %v", err)
	}
	if reply != `[{"i":0` {
		t.Fatalf("reply = %q, want the clipped text passed through unchanged", reply)
	}
	// Usage is still billed: the tokens were spent whether or not the reply parses.
	if usage.Output != 16000 {
		t.Fatalf("usage.Output = %d, want 16000", usage.Output)
	}
}

// thinkingAdjustedMaxTokens unit-level: pins the arithmetic directly, independent of the HTTP
// plumbing above.
func TestThinkingAdjustedMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		want       int
		wantCapped bool
	}{
		{"enabled with a large budget, no orig max_tokens on the body",
			`{"thinking":{"type":"enabled","budget_tokens":31999}}`, 31999 + PrefixAskMaxTokens, false},
		{"enabled with a small budget", `{"thinking":{"type":"enabled","budget_tokens":10}}`,
			10 + PrefixAskMaxTokens, false},
		{"adaptive", `{"thinking":{"type":"adaptive"}}`, PrefixAskMaxTokens, false},
		{"disabled", `{"thinking":{"type":"disabled"}}`, PrefixAskMaxTokens, false},
		{"absent", `{}`, PrefixAskMaxTokens, false},
		// Reviewer's minor note: "enabled" with budget_tokens missing is 0 + reply. The API would
		// reject such a body anyway (thinking.enabled requires a budget), so this is not a shape
		// CompletePrefixed must defend against — just pinned so the fall-through reads as a
		// choice, not an oversight.
		{"enabled with budget_tokens missing", `{"thinking":{"type":"enabled"}}`, PrefixAskMaxTokens, false},
		// The reviewer's live-measured 64000/63999 shape: the raw want (63999+16000=79999)
		// exceeds the model's cap, so the body's own max_tokens (64000, measured live as OK --
		// cache_read=16230, cache_write=0) is used instead.
		{"orig max_tokens caps a want that would exceed the model's output limit",
			`{"thinking":{"type":"enabled","budget_tokens":63999},"max_tokens":64000}`, 64000, true},
		// orig == budget is NOT a valid ceiling (it would equal the budget, violating the
		// provider's strict ">" requirement), so the strict "orig > budget" guard must reject it
		// and fall through to the computed want.
		{"orig max_tokens equal to the budget is not used as a ceiling",
			`{"thinking":{"type":"enabled","budget_tokens":31999},"max_tokens":31999}`,
			31999 + PrefixAskMaxTokens, false},
		// orig below the computed want but ALSO below the budget: not a valid ceiling either
		// (an orig that doesn't even clear the budget proves nothing), so it must not be used.
		{"orig max_tokens below the budget is not used as a ceiling",
			`{"thinking":{"type":"enabled","budget_tokens":31999},"max_tokens":100}`,
			31999 + PrefixAskMaxTokens, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, capped := thinkingAdjustedMaxTokens([]byte(tc.body), PrefixAskMaxTokens)
			if got != tc.want {
				t.Errorf("thinkingAdjustedMaxTokens = %d, want %d", got, tc.want)
			}
			if capped != tc.wantCapped {
				t.Errorf("capped = %v, want %v", capped, tc.wantCapped)
			}
		})
	}
}

// THE DEFECT, live on claude-sonnet-5[1m] (6/6 failures in one capture): Claude Code sends a
// mid-conversation `role: system` reminder as the LAST message of the stored request on
// effectively every turn. Appending the ask directly after that message is a guaranteed 400
// ("role 'system' must precede an 'assistant' message or end the array"). An earlier version of
// this fix inserted the ask BEFORE the system message instead, which cleared that rule but moved
// bytes in front of the cache_control breakpoint Claude Code places on exactly that message —
// measured live, that version turned the cache READ this method exists for into a full WRITE
// (read=0, write=21,652). This version leaves the system message exactly where it is and appends
// a short synthetic assistant turn ("Understood.") plus the ask after it instead — measured live,
// read=21,644, write=0. See insertAskMessage's doc comment for the full comparison.
func TestCompletePrefixedAppendsAfterATrailingSystemMessage(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],`+
			`"usage":{"input_tokens":40,"output_tokens":10,"cache_read_input_tokens":19595}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":16000,` +
		`"messages":[` +
		`{"role":"user","content":"first turn"},` +
		`{"role":"assistant","content":"reply"},` +
		`{"role":"user","content":"second turn"},` +
		`{"role":"system","content":"<system-reminder>be careful</system-reminder>"}` +
		`]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	reply, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge")
	if err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	if reply != `{"verdicts":[]}` {
		t.Fatalf("reply = %q", reply)
	}

	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 6 {
		t.Fatalf("got %d messages, want 6 (4 original + the synthetic turn + the ask): %s",
			len(msgs), got)
	}
	// EVERY ORIGINAL MESSAGE IS BYTE-IDENTICAL AND UNMOVED — this is what keeps the cache: nothing
	// before (or at) the system message's own position may change by even a byte.
	want := []struct{ role, content string }{
		{"user", "first turn"}, {"assistant", "reply"}, {"user", "second turn"},
		{"system", "<system-reminder>be careful</system-reminder>"},
		{"assistant", "Understood."},
		{"user", "judge"},
	}
	for i, w := range want {
		if msgs[i].Get("role").String() != w.role || msgs[i].Get("content").String() != w.content {
			t.Errorf("messages[%d] = %s, want role=%q content=%q", i, msgs[i].Raw, w.role, w.content)
		}
	}
	// THE ASK IS ALWAYS LAST, which is what keeps this route's own prefill rule satisfied.
	if last := msgs[len(msgs)-1]; last.Get("role").String() != "user" || last.Get("content").String() != "judge" {
		t.Fatalf("the ask is not the final message: %s", got)
	}
}

// MULTIPLE TRAILING SYSTEM MESSAGES all stay exactly where they are; the synthetic turn and the
// ask land after the whole run, not between any of them.
func TestCompletePrefixedAppendsAfterMultipleTrailingSystemMessages(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],"usage":{}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":16000,` +
		`"messages":[` +
		`{"role":"user","content":"turn one"},` +
		`{"role":"system","content":"reminder one"},` +
		`{"role":"system","content":"reminder two"}` +
		`]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	if _, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge"); err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %s", len(msgs), got)
	}
	if msgs[1].Get("content").String() != "reminder one" || msgs[2].Get("content").String() != "reminder two" {
		t.Fatalf("the trailing system run was reordered: %s", got)
	}
	if msgs[3].Get("role").String() != "assistant" || msgs[3].Get("content").String() != "Understood." {
		t.Fatalf("the synthetic turn did not land right after the system run: %s", got)
	}
	if msgs[4].Get("role").String() != "user" || msgs[4].Get("content").String() != "judge" {
		t.Fatalf("the ask did not land last: %s", got)
	}
}

// THE ORDINARY CASE, with no trailing system message, is unaffected: the ask still lands at the
// absolute end, exactly as CompletePrefixed always behaved before this fix.
func TestCompletePrefixedAppendsAtTheEndWithNoTrailingSystemMessage(t *testing.T) {
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"verdicts\":[]}"}],"usage":{}}`)
	}))
	defer up.Close()

	prefixBody := []byte(`{"model":"claude-sonnet-5","max_tokens":16000,` +
		`"messages":[{"role":"user","content":"carry on"}]}`)
	d, ref := dispatchtest.HTTP(up.URL, nil)
	cli := Anthropic{D: d, Ref: ref, Model: "claude-sonnet-5"}
	if _, _, err := cli.CompletePrefixed(context.Background(), prefixBody, "judge"); err != nil {
		t.Fatalf("CompletePrefixed: %v", err)
	}
	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %s", len(msgs), got)
	}
	if msgs[0].Get("content").String() != "carry on" || msgs[1].Get("content").String() != "judge" {
		t.Fatalf("the ordinary append-at-the-end shape changed: %s", got)
	}
}

// insertAskMessage unit-level: pins the insertion point directly, independent of the HTTP
// plumbing above.
func TestInsertAskMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantRole []string
	}{
		{"no trailing system: ask appended alone", `{"messages":[{"role":"user","content":"a"}]}`,
			[]string{"user", "user"}},
		{"one trailing system: synthetic turn then ask, both after it",
			`{"messages":[{"role":"user","content":"a"},{"role":"system","content":"r"}]}`,
			[]string{"user", "system", "assistant", "user"}},
		{"two trailing system: the whole run is untouched, synthetic turn and ask follow it",
			`{"messages":[{"role":"user","content":"a"},` +
				`{"role":"system","content":"r1"},{"role":"system","content":"r2"}]}`,
			[]string{"user", "system", "system", "assistant", "user"}},
		{"empty messages", `{"messages":[]}`, []string{"user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := insertAskMessage([]byte(tc.body), "ask")
			if err != nil {
				t.Fatalf("insertAskMessage: %v", err)
			}
			msgs := gjson.GetBytes(out, "messages").Array()
			if len(msgs) != len(tc.wantRole) {
				t.Fatalf("got %d messages, want %d: %s", len(msgs), len(tc.wantRole), out)
			}
			for i, role := range tc.wantRole {
				if got := msgs[i].Get("role").String(); got != role {
					t.Errorf("messages[%d].role = %q, want %q (%s)", i, got, role, out)
				}
			}
		})
	}
}

// #721, #776: the last message's breakpoint is copied onto what is appended, and stays on the last
// message too. Nothing before the appended messages changes.
func TestInsertAskMessageCopiesTheTrailingBreakpoint(t *testing.T) {
	const early = `{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`
	count := func(b []byte) int { return len(gjson.GetBytes(b, "messages.#.cache_control").Array()) + bpInBlocks(b) }
	for _, tc := range []struct {
		name, last, cc string
	}{
		{"system, string content", `{"role":"system","content":"r","cache_control":{"type":"ephemeral"}}`, `{"type":"ephemeral"}`},
		{"system, block content", `{"role":"system","content":[{"type":"text","text":"r","cache_control":{"type":"ephemeral"}}]}`, `{"type":"ephemeral"}`},
		{"system, 1h ttl", `{"role":"system","content":"r","cache_control":{"type":"ephemeral","ttl":"1h"}}`, `{"type":"ephemeral","ttl":"1h"}`},
		{"user, block content", `{"role":"user","content":[{"type":"text","text":"u","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, `{"type":"ephemeral","ttl":"1h"}`},
		{"user, string content", `{"role":"user","content":"u","cache_control":{"type":"ephemeral"}}`, `{"type":"ephemeral"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","messages":[` + early + `,` + tc.last + `]}`)
			out, err := insertAskMessage(in, "ask")
			if err != nil {
				t.Fatal(err)
			}
			if count(out) != count(in)+1 {
				t.Errorf("breakpoint count %d -> %d, want one more (the bridge's copy): %s", count(in), count(out), out)
			}
			msgs := gjson.GetBytes(out, "messages").Array()
			if msgs[0].Raw != early {
				t.Errorf("an earlier message changed: %s", msgs[0].Raw)
			}
			if msgs[1].Raw != tc.last {
				t.Errorf("the old last message changed; it must keep its breakpoint (#776): %s", msgs[1].Raw)
			}
			carrier := msgs[len(msgs)-2]
			if got := carrier.Get("content.0.cache_control").Raw; got != tc.cc {
				t.Errorf("carrier cache_control = %q, want %q: %s", got, tc.cc, out)
			}
			if carrier.Get("content.0.text").String() != "Understood." || carrier.Get("role").String() != "assistant" {
				t.Errorf("breakpoint not on the assistant bridge: %s", out)
			}
			if last := msgs[len(msgs)-1]; last.Get("content").String() != "ask" || strings.Contains(last.Raw, "cache_control") {
				t.Errorf("the ask is not a plain string without a breakpoint (#725): %s", last.Raw)
			}
		})
	}
}

// #776: the question must leave every byte of the turn it reads as it was sent, the last message's
// breakpoint included. That breakpoint names the turn's cache entry; a question that moves it away
// refreshes only its own new entry, and the plain ping after it finds the turn's entry expired.
// Live: Sonnet 5.5, the ping after the question read 13,487 and wrote 18,053 with the breakpoint
// moved, and read 31,542 and wrote 0 with it kept.
func TestInsertAskMessageLeavesThePrefixByteIdentical(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]},` +
		`{"role":"system","content":[{"type":"text","text":"reminder","cache_control":{"type":"ephemeral"}}]}]}`)
	out, err := insertAskMessage(body, "ask")
	if err != nil {
		t.Fatal(err)
	}
	in := gjson.GetBytes(body, "messages").Array()
	got := gjson.GetBytes(out, "messages").Array()
	if len(got) != len(in)+2 {
		t.Fatalf("got %d messages, want %d: %s", len(got), len(in)+2, out)
	}
	for i := range in {
		if got[i].Raw != in[i].Raw {
			t.Errorf("messages[%d] changed:\n was %s\n now %s", i, in[i].Raw, got[i].Raw)
		}
	}
	if gjson.GetBytes(out, "system").Raw != gjson.GetBytes(body, "system").Raw {
		t.Errorf("system changed: %s", out)
	}
}

// #776: the provider refuses more than 4 breakpoints. At the cap the bridge gets no copy, and the
// last message's breakpoint (which keeps the turn's entry alive) stays.
func TestInsertAskMessageNeverExceedsTheBreakpointCap(t *testing.T) {
	sys := `"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]`
	mark := `{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}`
	for _, tc := range []struct {
		name   string
		body   string
		in     int
		bridge bool
	}{
		{"3 marks: the bridge gets a copy",
			`{` + sys + `,"messages":[{"role":"user","content":"go"},{"role":"system","content":[` + mark + `]}]}`, 3, true},
		{"4 marks: no copy, the last keeps its own",
			`{` + sys + `,"messages":[{"role":"user","content":[` + mark + `]},{"role":"system","content":[` + mark + `]}]}`, 4, false},
		{"4 marks with one on tools: no copy",
			`{"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],` + sys + `,"messages":[{"role":"user","content":"go"},{"role":"system","content":"r","cache_control":{"type":"ephemeral"}}]}`, 4, false},
		{"4 marks on a trailing user message: no copy",
			`{` + sys + `,"messages":[{"role":"assistant","content":[` + mark + `]},{"role":"user","content":[` + mark + `]}]}`, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n := countBreakpoints([]byte(tc.body)); n != tc.in {
				t.Fatalf("fixture has %d breakpoints, want %d", n, tc.in)
			}
			out, err := insertAskMessage([]byte(tc.body), "ask")
			if err != nil {
				t.Fatal(err)
			}
			n := countBreakpoints(out)
			if n > maxBreakpoints {
				t.Errorf("%d breakpoints, over the provider's cap of %d: %s", n, maxBreakpoints, out)
			}
			msgs := gjson.GetBytes(out, "messages").Array()
			bridge := msgs[len(msgs)-2]
			if bridge.Get("role").String() != "assistant" || !strings.Contains(bridge.Raw, "Understood.") {
				t.Fatalf("no bridge: %s", out)
			}
			if has := strings.Contains(bridge.Raw, "cache_control"); has != tc.bridge {
				t.Errorf("bridge breakpoint = %v, want %v: %s", has, tc.bridge, out)
			}
			if want := tc.in + map[bool]int{true: 1, false: 0}[tc.bridge]; n != want {
				t.Errorf("%d breakpoints, want %d: %s", n, want, out)
			}
			if !strings.Contains(msgs[len(msgs)-3].Raw, "cache_control") {
				t.Errorf("the last message lost its breakpoint: %s", out)
			}
		})
	}
}

func bpInBlocks(b []byte) int {
	n := 0
	for _, m := range gjson.GetBytes(b, "messages").Array() {
		for _, bl := range m.Get("content").Array() {
			if bl.Get("cache_control").Exists() {
				n++
			}
		}
	}
	return n
}

func TestInsertAskMessageAddsNoBreakpointWhenTheLastHasNone(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"system","content":"r"}]}`)
	out, err := insertAskMessage(in, "ask")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("a breakpoint appeared: %s", out)
	}
	if gjson.GetBytes(out, "messages.2.content").String() != "Understood." {
		t.Errorf("bridge content is not a plain string: %s", out)
	}
}

// #725 review: a trailing user message WITHOUT a breakpoint needs no bridge; a trailing assistant
// message keeps its breakpoint where it is and the ask follows it directly.
func TestInsertAskMessageBridgeOnlyWhereABreakpointIsCopied(t *testing.T) {
	out, err := insertAskMessage([]byte(`{"messages":[{"role":"user","content":"a"}]}`), "ask")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(gjson.GetBytes(out, "messages").Array()); n != 2 {
		t.Errorf("a plain trailing user message got a bridge: %s", out)
	}
	asst := `{"role":"assistant","content":[{"type":"text","text":"done","cache_control":{"type":"ephemeral"}}]}`
	out, err = insertAskMessage([]byte(`{"messages":[{"role":"user","content":"a"},`+asst+`]}`), "ask")
	if err != nil {
		t.Fatal(err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 || msgs[1].Raw != asst || strings.Contains(msgs[2].Raw, "cache_control") {
		t.Errorf("assistant-last shape changed: %s", out)
	}
}

// countBreakpoints counts what the provider's cap counts: both mark spellings, on tools, system,
// messages and content blocks, the same as apply.CountBreakpoints.
func TestCountBreakpointsMatchesTheCapRule(t *testing.T) {
	body := []byte(`{"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"s","cachePoint":{"type":"default"}}],` +
		`"messages":[{"role":"user","content":"u","cache_control":{"type":"ephemeral"}},` +
		`{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}},{"type":"text","text":"y"}]}]}`)
	if n := countBreakpoints(body); n != 4 {
		t.Errorf("countBreakpoints = %d, want 4", n)
	}
	if n := countBreakpoints([]byte(`{"messages":[{"role":"user","content":"cache_control"}]}`)); n != 0 {
		t.Errorf("text that only names the field counted as a mark: %d", n)
	}
}
