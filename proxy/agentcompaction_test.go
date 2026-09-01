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
)

// TestAgentCompactionIsBypassed: the agent's own summarization request must reach the
// upstream BYTE-IDENTICAL, while ordinary traffic is still compacted. Compacting a
// summary request replaces content with <<cg:…>> markers in the one request that is
// asking for that content verbatim, and the loss is baked into the summary — no
// expansion can recover it.
//
// The near-miss cases are the anti-spoof half: the signal is a full agent-authored
// sentence in the LAST message, so ordinary prose about summarizing, and the same
// sentence sitting anywhere but the final turn, must both still be compacted.
func TestAgentCompactionIsBypassed(t *testing.T) {
	dump := strings.Repeat("a verbose repeated tool output line\n", 60)
	for _, tc := range []struct {
		name       string
		lastMsg    map[string]any
		before     []map[string]any // extra turns spliced before the dup tool output
		wantBypass bool
	}{
		{
			name:       "claude code auto-compact",
			lastMsg:    map[string]any{"role": "user", "content": ccCompactPrompt},
			wantBypass: true,
		},
		{
			name:       "claude code partial compact",
			lastMsg:    map[string]any{"role": "user", "content": ccPartialCompactPrompt},
			wantBypass: true,
		},
		{
			// Anthropic content-block shape: the phrase is inside a text block, not a
			// plain string.
			name: "claude code compact as content blocks",
			lastMsg: map[string]any{"role": "user", "content": []map[string]any{
				{"type": "text", "text": ccCompactPrompt},
			}},
			wantBypass: true,
		},
		{
			name:       "bob shell compression",
			lastMsg:    map[string]any{"role": "user", "content": bobCompressPrompt},
			wantBypass: true,
		},
		{
			name:       "ordinary request",
			lastMsg:    map[string]any{"role": "user", "content": "fix the failing test"},
			wantBypass: false,
		},
		{
			// The realistic near miss: a human asking, in their own words, for the very
			// thing the agent's prompt asks for.
			name: "human asking for a summary of the conversation so far",
			lastMsg: map[string]any{"role": "user", "content": "Please write a detailed summary " +
				"of the conversation so far into NOTES.md — your task is to keep it short."},
			wantBypass: false,
		},
		{
			// The latch attempt: the exact agent prompt, but not the final turn (a real
			// transcript contains it after any earlier compaction). If this bypassed, one
			// prompt would disable compaction for every remaining turn of the session.
			name:       "agent prompt present but not the last message",
			before:     []map[string]any{{"role": "user", "content": ccCompactPrompt}},
			lastMsg:    map[string]any{"role": "user", "content": "carry on"},
			wantBypass: false,
		},
		{
			// Model-generated text is not a compaction request.
			name:       "phrase in a trailing assistant turn",
			lastMsg:    map[string]any{"role": "assistant", "content": ccCompactPrompt},
			wantBypass: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.ReadAll(r.Body)
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
			body, err := json.Marshal(map[string]any{"model": "gpt-x", "messages": msgs})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if tc.wantBypass {
				if !bytes.Equal(got, body) {
					t.Fatalf("a compaction request must be forwarded byte-identical.\n sent: %s\n  got: %s", body, got)
				}
				return
			}
			if bytes.Equal(got, body) {
				t.Fatalf("this request must still be compacted, but was forwarded unchanged: %s", got)
			}
			// And specifically compacted, not merely re-serialized: dedup collapsed the
			// duplicate tool output.
			second := gjson.GetBytes(got, "messages."+strconv.Itoa(len(msgs)-2)+".content").String()
			if !strings.Contains(second, "identical to an earlier") {
				t.Fatalf("dedup did not run: %s", got)
			}
		})
	}
}

// TestAgentCompactionBypassIsCounted: the bypass must be visible at /stats. A silent
// one is indistinguishable from a pipeline that has quietly stopped working, which is
// the failure mode the gate histogram exists for.
func TestAgentCompactionBypassIsCounted(t *testing.T) {
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

	st, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Body.Close()
	stats, _ := io.ReadAll(st.Body)
	if n := gjson.GetBytes(stats, "components.bypass.gates.agent_compaction").Int(); n != 2 {
		t.Fatalf("components.bypass.gates.agent_compaction = %d; want 2 (the two agent "+
			"compaction requests, not the ordinary turn): %s", n, stats)
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
// The bypass case below is the one that must NOT change, and it costs no cache to keep: a
// compaction request's system prompt and message set differ from the conversation's, so it
// hashes to its own prefix and shares no entry with the turns around it.
//
// TestMarkerFreeSSEStreamsThrough is the other half of the pair: advertising on every turn
// means every Anthropic stream is inspected, and it pins that inspection still streams.
func TestExpandToolAdvertisedOnEveryTurnButNeverOnACompaction(t *testing.T) {
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
		// The compaction request carries the whole transcript, markers included — it must
		// STILL not get the tool, because it is bypassed.
		{"agent compaction with markers in the transcript",
			[]map[string]any{marked, {"role": "user", "content": ccCompactPrompt}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ = io.ReadAll(r.Body)
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

			advertised := strings.Contains(string(got), "context_guru_expand")
			if advertised != tc.want {
				t.Fatalf("expand tool advertised=%v, want %v: %s", advertised, tc.want, got)
			}
		})
	}
}
