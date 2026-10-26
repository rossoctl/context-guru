package proxy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The VERBATIM openings of the summary requests the agents send. Kept here as fixtures
// rather than imported so a change to the detector's phrase list cannot make these tests
// pass by construction.
const (
	ccCompactPrompt = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
		"- Tool calls will be REJECTED and will waste your only turn — you will fail the task.\n\n" +
		"Your task is to create a detailed summary of the conversation so far, paying close " +
		"attention to the user's explicit requests and your previous actions.\n" +
		"3. Files and Code Sections: ... include full code snippets where applicable"
	ccPartialCompactPrompt = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
		"Your task is to create a detailed summary of the RECENT portion of the conversation"
	bobCompressPrompt = "First, reason in your scratchpad. Then, generate the <state_snapshot>."
	// Claude Code 2.1.296's second variant (#518): "of this conversation", not "of the".
	ccThisConversationPrompt = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
		"Your task is to create a detailed summary of this conversation. This summary will be " +
		"placed at the start of a continuing session"
)

// ccTokensLeft is the system message Claude Code 2.1.296 puts after the last user turn.
var ccTokensLeft = map[string]any{"role": "system", "content": "<total_tokens>15000000 tokens left</total_tokens>"}

// TestAgentCompactionIsNotBypassed: the agent's own summarization request goes through the
// pipeline like any other request. A bypass forwards the original history while the provider
// cache holds the rewritten one, so it cost a full cache write (see isAgentCompaction). Every
// case below must be compacted. wantDetected says whether the detector counted it, which is the
// only thing the detection now changes on this path.
//
// The near-miss cases are the anti-spoof half: the signal is a full agent-authored
// sentence in the LAST message, so ordinary prose about summarizing, and the same
// sentence sitting anywhere but the final turn, must not be counted.
func TestAgentCompactionIsNotBypassed(t *testing.T) {
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	for _, tc := range []struct {
		name         string
		lastMsg      map[string]any
		before       []map[string]any // extra turns spliced before the dup tool output
		after        []map[string]any // extra turns after lastMsg
		wantDetected bool
	}{
		{
			name:         "claude code auto-compact",
			lastMsg:      map[string]any{"role": "user", "content": ccCompactPrompt},
			wantDetected: true,
		},
		{
			name:         "claude code partial compact",
			lastMsg:      map[string]any{"role": "user", "content": ccPartialCompactPrompt},
			wantDetected: true,
		},
		{
			// Anthropic content-block shape: the phrase is inside a text block, not a
			// plain string.
			name: "claude code compact as content blocks",
			lastMsg: map[string]any{"role": "user", "content": []map[string]any{
				{"type": "text", "text": ccCompactPrompt},
			}},
			wantDetected: true,
		},
		{
			// Claude Code 2.1.296: the prompt is a second text block of the last user turn,
			// and a system message follows that turn.
			name: "claude code 2.1.296 compact before a trailing system message",
			lastMsg: map[string]any{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "name 1 command"},
				{"type": "text", "text": ccCompactPrompt},
			}},
			after:        []map[string]any{ccTokensLeft},
			wantDetected: true,
		},
		{
			name:         "claude code 2.1.296 this-conversation variant",
			lastMsg:      map[string]any{"role": "user", "content": ccThisConversationPrompt},
			after:        []map[string]any{ccTokensLeft},
			wantDetected: true,
		},
		{
			// The OpenAI dialect's equivalent of a trailing system message.
			name:         "compact before a trailing developer message",
			lastMsg:      map[string]any{"role": "user", "content": ccThisConversationPrompt},
			after:        []map[string]any{{"role": "developer", "content": "be brief"}},
			wantDetected: true,
		},
		{
			// Skipping trailing system messages must not reach past an assistant turn.
			name:         "agent prompt before an assistant turn and a system message",
			before:       []map[string]any{{"role": "user", "content": ccCompactPrompt}},
			lastMsg:      map[string]any{"role": "assistant", "content": "done"},
			after:        []map[string]any{ccTokensLeft},
			wantDetected: false,
		},
		{
			name:         "ordinary request before a trailing system message",
			lastMsg:      map[string]any{"role": "user", "content": "fix the failing test"},
			after:        []map[string]any{ccTokensLeft},
			wantDetected: false,
		},
		{
			name:         "bob shell compression",
			lastMsg:      map[string]any{"role": "user", "content": bobCompressPrompt},
			wantDetected: true,
		},
		{
			name:         "ordinary request",
			lastMsg:      map[string]any{"role": "user", "content": "fix the failing test"},
			wantDetected: false,
		},
		{
			// The realistic near miss: a human asking, in their own words, for the very
			// thing the agent's prompt asks for.
			name: "human asking for a summary of the conversation so far",
			lastMsg: map[string]any{"role": "user", "content": "Please write a detailed summary " +
				"of the conversation so far into NOTES.md — your task is to keep it short."},
			wantDetected: false,
		},
		{
			// The latch attempt: the exact agent prompt, but not the final turn (a real
			// transcript contains it after any earlier compaction). If this bypassed, one
			// prompt would disable compaction for every remaining turn of the session.
			name:         "agent prompt present but not the last message",
			before:       []map[string]any{{"role": "user", "content": ccCompactPrompt}},
			lastMsg:      map[string]any{"role": "user", "content": "carry on"},
			wantDetected: false,
		},
		{
			// Model-generated text is not a compaction request.
			name:         "phrase in a trailing assistant turn",
			lastMsg:      map[string]any{"role": "assistant", "content": ccCompactPrompt},
			wantDetected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var up upstreamCapture
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				up.record(r)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer upstream.Close()

			h, _ := buildHandler(t, "pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n", upstream.URL)
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			msgs := append([]map[string]any{}, tc.before...)
			msgs = append(msgs,
				map[string]any{"role": "tool", "tool_call_id": "a", "content": dump},
				map[string]any{"role": "tool", "tool_call_id": "b", "content": dump},
				tc.lastMsg,
			)
			dup := len(msgs) - 2 // the second duplicate tool output
			msgs = append(msgs, tc.after...)
			body, err := json.Marshal(map[string]any{"model": "gpt-x", "messages": msgs})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			got := up.last().body
			if bytes.Equal(got, body) {
				t.Fatalf("this request must be compacted, but was forwarded unchanged: %s", got)
			}
			// And specifically compacted, not merely re-serialized: dedup collapsed the
			// duplicate tool output.
			second := gjson.GetBytes(got, "messages."+strconv.Itoa(dup)+".content").String()
			if !strings.Contains(second, "repeat of") {
				t.Fatalf("dedup did not run: %s", got)
			}
			if n := agentCompactionCount(t, srv.URL); (n == 1) != tc.wantDetected {
				t.Fatalf("components.agent_compaction.gates.detected = %d; wantDetected = %v", n, tc.wantDetected)
			}
		})
	}
}

// agentCompactionCount reads the detection counter from /stats.
func agentCompactionCount(t *testing.T, base string) int64 {
	t.Helper()
	st, err := http.Get(base + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Body.Close()
	stats, _ := io.ReadAll(st.Body)
	return gjson.GetBytes(stats, "components.agent_compaction.gates.detected").Int()
}

// TestAgentCompactionIsCounted: the detection must be visible at /stats, and it must NOT
// appear as a bypass. components.bypass.gates.agent_compaction no longer exists.
func TestAgentCompactionIsCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	h, _ := buildHandler(t, "pipeline: [dedup]\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	for _, prompt := range []string{ccCompactPrompt, bobCompressPrompt, "an ordinary turn"} {
		body := openAIBody(map[string]any{"role": "user", "content": prompt})
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if n := agentCompactionCount(t, srv.URL); n != 2 {
		t.Fatalf("components.agent_compaction.gates.detected = %d; want 2 (the two agent "+
			"compaction requests, not the ordinary turn)", n)
	}
	st, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Body.Close()
	stats, _ := io.ReadAll(st.Body)
	if gjson.GetBytes(stats, "components.bypass").Exists() {
		t.Fatalf("an agent compaction must not be reported as a bypass: %s", stats)
	}
}

// TestAgentCompactionWithTheHeaderIsStillBypassed: the explicit header is the only bypass.
// The same compaction-shaped request, with the header, is forwarded byte for byte and is
// not counted as a detection.
func TestAgentCompactionWithTheHeaderIsStillBypassed(t *testing.T) {
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, "pipeline: [dedup]\ncomponents:\n  dedup: {min_tokens: 20}\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	body := openAIBody(
		map[string]any{"role": "tool", "tool_call_id": "a", "content": dump},
		map[string]any{"role": "tool", "tool_call_id": "b", "content": dump},
		map[string]any{"role": "user", "content": ccCompactPrompt},
	)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-context-guru-bypass", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := up.last().body; !bytes.Equal(got, body) {
		t.Fatalf("a request with the bypass header must be forwarded byte-identical.\n sent: %s\n  got: %s", body, got)
	}
}

// compactionTurnPair sends turn N (an ordinary turn with a duplicated tool output, so the
// pipeline rewrites it) and then turn N+1, the same history plus a compaction prompt, on one
// session. It returns the two forwarded bodies and the two bodies as sent.
func compactionTurnPair(t *testing.T, header http.Header) (fwdN, fwdC, sentN, sentC []byte) {
	t.Helper()
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude",` +
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":5,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, "pipeline: [dedup, linecap]\ncomponents:\n  dedup: {min_tokens: 20}\n", upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	toolUse := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{}}}}
	}
	toolResult := func(id string) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": id, "content": dump}}}
	}
	history := []map[string]any{
		{"role": "user", "content": "read it twice"},
		toolUse("t1"), toolResult("t1"),
		toolUse("t2"), toolResult("t2"),
		{"role": "assistant", "content": "done"},
	}
	post := func(msgs ...map[string]any) (sent []byte) {
		sent = toolsRequest(t, msgs...)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(sent))
		req.Header.Set("x-context-guru-session", "compaction-prefix")
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return sent
	}
	turnN := append(append([]map[string]any{}, history...), map[string]any{"role": "user", "content": "thanks"})
	sentN = post(turnN...)
	compaction := append(append([]map[string]any{}, history...), map[string]any{"role": "user", "content": ccCompactPrompt})
	sentC = post(compaction...)
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2", up.hits())
	}
	return up.round(1).body, up.round(2).body, sentN, sentC
}

// TestAgentCompactionKeepsThePrefixOfThePreviousTurn is the property this behavior exists for:
// the tools array and the earlier messages that turn N+1 (the compaction) forwards are
// byte-identical to what turn N forwarded. A bypass forwarded the original history there, so the
// provider cache, which held the rewritten history, missed from an early point on.
func TestAgentCompactionKeepsThePrefixOfThePreviousTurn(t *testing.T) {
	fwdN, fwdC, sentN, _ := compactionTurnPair(t, nil)

	// Positive outcomes first, so a pass proves the pipeline was reached.
	if bytes.Equal(fwdN, sentN) {
		t.Fatalf("turn N was forwarded unchanged; the fixture does not exercise the rewrite: %s", fwdN)
	}
	if !strings.Contains(gjson.GetBytes(fwdN, "messages.4.content.0.content").String(), "repeat of") {
		t.Fatalf("dedup did not rewrite the duplicate tool output on turn N: %s", fwdN)
	}
	if !strings.Contains(gjson.GetBytes(fwdC, "messages.4.content.0.content").String(), "repeat of") {
		t.Fatalf("the compaction request was not rewritten (it was bypassed): %s", fwdC)
	}
	if !strings.Contains(gjson.GetBytes(fwdC, "messages.6.content").String(), "create a detailed summary of") {
		t.Fatalf("the compaction prompt is missing or moved: %s", fwdC)
	}
	// The cache property: tools, then the six earlier messages, byte for byte.
	if tn, tc := gjson.GetBytes(fwdN, "tools").Raw, gjson.GetBytes(fwdC, "tools").Raw; tn != tc {
		t.Fatalf("the forwarded tool list changed on the compaction turn.\n N: %s\n C: %s", tn, tc)
	}
	if !strings.Contains(gjson.GetBytes(fwdC, "tools").Raw, "context_guru_expand") {
		t.Fatalf("the expand tool is missing from the compaction request: %s", gjson.GetBytes(fwdC, "tools").Raw)
	}
	for i := 0; i < 6; i++ {
		path := "messages." + strconv.Itoa(i)
		if n, c := gjson.GetBytes(fwdN, path).Raw, gjson.GetBytes(fwdC, path).Raw; n != c {
			t.Fatalf("message %d of the forwarded prefix differs between turn N and the compaction.\n N: %s\n C: %s", i, n, c)
		}
	}
}

// With the header, the same pair breaks that property on purpose: the compaction turn is
// forwarded as sent. This pins that the test above can fail.
func TestAgentCompactionWithTheHeaderForwardsTheOriginalHistory(t *testing.T) {
	_, fwdC, _, sentC := compactionTurnPair(t, http.Header{"X-Context-Guru-Bypass": {"true"}})
	if !bytes.Equal(fwdC, sentC) {
		t.Fatalf("a request with the bypass header must be forwarded byte-identical: %s", fwdC)
	}
}

// The short form Claude Code also sends: one user message and no tools. It must still be
// forwarded and answered.
func TestShortFormAgentCompactionStillWorks(t *testing.T) {
	var up upstreamCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"summary"}}]}`))
	}))
	defer upstream.Close()
	h, _ := buildHandler(t, offloadCapablePipeline, upstream.URL)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := openAIBody(map[string]any{"role": "user", "content": ccCompactPrompt})
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(answer), "summary") {
		t.Fatalf("status %d, body %s; want 200 with the upstream answer", resp.StatusCode, answer)
	}
	got := up.last().body
	if gjson.GetBytes(got, "messages.0.content").String() != ccCompactPrompt || gjson.GetBytes(got, "messages.#").Int() != 1 {
		t.Fatalf("the short-form request was changed: %s", got)
	}
	if gjson.GetBytes(got, "tools").Exists() {
		t.Fatalf("a tool-less request gained tools: %s", got)
	}
	if n := agentCompactionCount(t, srv.URL); n != 1 {
		t.Fatalf("detected = %d, want 1", n)
	}
}

// The condition to ADVERTISE context_guru_expand and the condition to INTERCEPT its use
// are one condition — the tool is on the outgoing request — and that condition is now a
// property of the SESSION rather than of the turn. `tools` sits ahead of `system` and
// `messages` in the provider's cache hash, so an advertise test that reads the turn makes
// the tools array a per-turn value and every change to it discards the entire cached
// prefix. It previously required a marker, so the array grew on the first offloading turn
// and shrank again on the next turn without one.
//
// What made that narrowing look necessary was the compaction case: nothing resolves, the
// model's raw tool_use is replayed to a client that implements no such tool, and Claude
// Code counts three of those as reason to disable auto-compact. That is fixed where it
// belongs — proxy.serve continues with a placeholder tool_result when nothing resolves —
// so advertising no longer has to pay for it.
//
// The compaction case below carries the tool too. A compaction request is not bypassed, so
// leaving the tool out would change the tools array against the thread's earlier turns and
// miss the cache.
//
// TestMarkerFreeSSEStreamsThrough is the other half of the pair: advertising on every turn
// means every Anthropic stream is inspected, and it pins that inspection still streams.
func TestExpandToolAdvertisedOnEveryTurnIncludingACompaction(t *testing.T) {
	marked := map[string]any{"role": "user", "content": "earlier output <<cg:HASH>>"}
	for _, tc := range []struct {
		name string
		msgs []map[string]any
		want bool
	}{
		// Advertised even with nothing to expand. The tools array must be byte-identical on
		// every request in a session or each change to it costs the whole cached prefix, so
		// the advertise test may not read the turn. An expand call that resolves nothing is
		// handled at the RESOLUTION now — proxy.serve continues with a placeholder
		// tool_result — which is what used to make this case unsafe.
		{"no markers yet", []map[string]any{{"role": "user", "content": "go"}}, true},
		{"marker present", []map[string]any{marked, {"role": "user", "content": "go on"}}, true},
		// The compaction request carries the whole transcript, markers included. It is not
		// bypassed, so it gets the tool like every other turn of its thread.
		{"agent compaction with markers in the transcript",
			[]map[string]any{marked, {"role": "user", "content": ccCompactPrompt}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var up upstreamCapture
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				up.record(r)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer upstream.Close()

			// A pipeline that CAN offload: advertising is gated on that, because a pipeline
			// which mints no markers would advertise a tool whose every call must fail. The
			// premise of this test is that an offload happened (hence the store seed below),
			// so the fixture has to be a pipeline where that is possible. `linecap` does not
			// act on these short bodies, so the forwarded bytes are unchanged.
			h, st := buildHandler(t, "pipeline: [linecap]\n", upstream.URL)
			st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			body, err := json.Marshal(map[string]any{
				"model":    "gpt-x",
				"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
				"messages": tc.msgs,
			})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			advertised := strings.Contains(string(up.last().body), "context_guru_expand")
			if advertised != tc.want {
				t.Fatalf("expand tool advertised=%v, want %v: %s", advertised, tc.want, up.last().body)
			}
		})
	}
}
