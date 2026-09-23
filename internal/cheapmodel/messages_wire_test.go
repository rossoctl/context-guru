package cheapmodel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// THE DEFECT THIS GUARDS AGAINST
//
// CompleteMessages was first written to marshal each bschemas.ChatMessage whole, on the
// reasoning that its json tags are already OpenAI-shaped. They are — but the struct carries
// more than the wire wants, every field omitempty and therefore silently present once set.
// Marshalling one assistant message with Reasoning and a tool call produced:
//
//	{"role":"assistant","reasoning":"...","tool_calls":[{"index":0,"id":"call_1",
//	 "function":{"name":null,"arguments":""}}]}
//
// `reasoning` is not a request field (and a thinking model sets one on most turns), `index`
// is a streaming-delta field, and "name": null is invalid where a string is required. So the
// mapping is an explicit allowlist, and this test is what keeps it one.
func captureBody(t *testing.T, fn func(o OpenAI) error) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"<summary>ok</summary>"}}]}`)
	}))
	defer srv.Close()
	if err := fn(OpenAI{BaseURL: srv.URL, Model: "m", Client: srv.Client()}); err != nil {
		t.Fatalf("call: %v", err)
	}
	return got
}

func TestCompleteMessagesSendsOnlyWireFields(t *testing.T) {
	reasoning := "I should check the file first"
	name, id, typ := "grep", "call_1", "function"
	txt := "calling the tool"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{
			{
				Role:    bschemas.ChatMessageRoleAssistant,
				Content: &bschemas.ChatMessageContent{ContentStr: &txt},
				ChatAssistantMessage: &bschemas.ChatAssistantMessage{
					Reasoning: &reasoning, // must NOT reach the wire
					ToolCalls: []bschemas.ChatAssistantMessageToolCall{
						{ID: &id, Type: &typ, Function: bschemas.ChatAssistantMessageToolCallFunction{
							Name: &name, Arguments: `{"q":"x"}`}},
					},
				},
			},
		})
		return err
	})

	raw, _ := json.Marshal(body)
	if s := string(raw); strings.Contains(s, "reasoning") {
		t.Errorf("`reasoning` reached the wire — not an OpenAI request field:\n%s", s)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v", body["messages"])
	}
	m := msgs[0].(map[string]any)
	// `name` is NOT in this list: bifrost tags it "for chat completions" and it is request-legal,
	// so dropping it would make the request diverge from the parent's rendered prefix. Its
	// passthrough has its own test below.
	for _, forbidden := range []string{"reasoning", "reasoning_details", "annotations", "refusal", "audio"} {
		if _, bad := m[forbidden]; bad {
			t.Errorf("assistant message carries non-wire field %q", forbidden)
		}
	}
	if m["content"] != "calling the tool" {
		t.Errorf("content = %#v, want the original text", m["content"])
	}
	tcs, ok := m["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %#v", m["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if _, bad := tc["index"]; bad {
		t.Error("tool_calls[0] carries `index` — a streaming-delta field, not a request field")
	}
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Errorf("tool call id/type wrong: %#v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "grep" {
		t.Errorf("function.name = %#v, want grep", fn["name"])
	}
	if fn["arguments"] != `{"q":"x"}` {
		t.Errorf("function.arguments = %#v — must survive verbatim", fn["arguments"])
	}
}

// A nil function name must be OMITTED, never sent as null: null is invalid where the backend
// requires a string, and a nil name means this was a streaming fragment.
func TestCompleteMessagesOmitsNilToolCallName(t *testing.T) {
	id := "call_9"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{{
			Role: bschemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &bschemas.ChatAssistantMessage{
				ToolCalls: []bschemas.ChatAssistantMessageToolCall{{ID: &id}},
			},
		}})
		return err
	})
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), `"name":null`) {
		t.Errorf(`"name":null reached the wire:\n%s`, raw)
	}
}

// A tool result must keep its tool_call_id, or the backend rejects the whole request.
func TestCompleteMessagesKeepsToolCallID(t *testing.T) {
	tid, out := "call_1", "3 failures"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{{
			Role:            bschemas.ChatMessageRoleTool,
			Content:         &bschemas.ChatMessageContent{ContentStr: &out},
			ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &tid},
		}})
		return err
	})
	m := body["messages"].([]any)[0].(map[string]any)
	if m["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %#v, want call_1 — without it the request is rejected", m["tool_call_id"])
	}
}

// ⭐ Block content must survive as BLOCKS. Flattening to a string changes the rendered prompt
// and forfeits the prefix match, which is the entire reason this method exists.
func TestCompleteMessagesPreservesContentBlocks(t *testing.T) {
	txt := "hello"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{{
			Role: bschemas.ChatMessageRoleUser,
			Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
				{Type: bschemas.ChatContentBlockTypeText, Text: &txt},
			}},
		}})
		return err
	})
	m := body["messages"].([]any)[0].(map[string]any)
	if _, isArray := m["content"].([]any); !isArray {
		t.Errorf("block content was flattened to %T — that changes the rendered prefix", m["content"])
	}
}

// `name` is request-legal on OpenAI, so it must survive: omitting a field the parent sent changes
// the rendered prefix, which is the byte-identity this method exists for.
func TestCompleteMessagesPassesNameThrough(t *testing.T) {
	name, txt := "alice", "hello"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{{
			Role:    bschemas.ChatMessageRoleUser,
			Name:    &name,
			Content: &bschemas.ChatMessageContent{ContentStr: &txt},
		}})
		return err
	})
	m := body["messages"].([]any)[0].(map[string]any)
	if m["name"] != "alice" {
		t.Errorf("name = %#v, want alice — a dropped request-legal field breaks the prefix match", m["name"])
	}
}

// ⚠️ A tool message whose content is a BLOCK ARRAY. apply's normalize synthesises role=tool
// messages on Anthropic-shaped traffic, and this client is the only MessagesModel, so that traffic
// reaches here. The block array is passed through rather than flattened — flattening would change
// the rendered prefix — so this test records what actually goes on the wire for that case, which is
// the shape a backend expecting a string would reject.
func TestCompleteMessagesToolMessageWithBlockContent(t *testing.T) {
	tid, txt := "call_1", "3 failures"
	body := captureBody(t, func(o OpenAI) error {
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{{
			Role: bschemas.ChatMessageRoleTool,
			Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{
				{Type: bschemas.ChatContentBlockTypeText, Text: &txt},
			}},
			ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &tid},
		}})
		return err
	})
	m := body["messages"].([]any)[0].(map[string]any)
	if m["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id lost: %#v", m["tool_call_id"])
	}
	if _, isArray := m["content"].([]any); !isArray {
		t.Errorf("block content on a tool message was flattened to %T; the prefix would diverge "+
			"from the parent's", m["content"])
	}
}

// ⭐ THE BREAKPOINT IS WHAT MAKES THIS WORK ON ANTHROPIC AT ALL.
//
// Anthropic-family models have no automatic prefix caching, so the appended-instruction shape
// saves nothing without an explicit cache_control mark: measured 8.2x the warm cost on
// aws/claude-opus-4-7. The mark must land on the last block of the PREFIX — everything except
// the appended instruction — because a mark on the instruction writes a new entry every turn
// and reads none.
func TestCompleteMessagesMarksThePrefixForAnthropicModels(t *testing.T) {
	convo := strings.Repeat("the handler in src/mod/file.py returns 500 on a missing newline; "+
		"pytest tests/test_handler.py reports three failures in the dispatch path\n", 90)
	instr := "[OPERATOR INSTRUCTION] Summarize the conversation above."
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":"<summary>ok</summary>"}}]}`)
	}))
	defer srv.Close()
	o := OpenAI{BaseURL: srv.URL, Model: "aws/claude-opus-4-7", Client: srv.Client()}
	if _, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &convo}},
		{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &instr}},
	}); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}
	blocks, isArr := msgs[0].(map[string]any)["content"].([]any)
	if !isArr {
		t.Fatalf("prefix content = %T, want a block array so a breakpoint can attach", msgs[0].(map[string]any)["content"])
	}
	last := blocks[len(blocks)-1].(map[string]any)
	cc, ok := last["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("prefix's last block carries no ephemeral cache_control: %#v", last)
	}
	if last["text"] != convo {
		t.Error("the marked block's text was altered — that changes the prefix and loses the read")
	}
	// The appended instruction must NOT be marked: it is the fresh suffix, and marking it
	// writes a new entry per turn instead of reading the agent's.
	if s := string(mustJSON(t, msgs[1])); strings.Contains(s, "cache_control") {
		t.Errorf("the appended instruction was marked — that writes every turn and reads nothing: %s", s)
	}
}

// A real OpenAI endpoint caches automatically AND rejects unknown fields inside a content part,
// so the mark must never be sent there.
func TestCompleteMessagesSendsNoCacheControlToOpenAIModels(t *testing.T) {
	convo := strings.Repeat("stack frame in src/mod/file.py line 42 raised on a missing newline\n", 120)
	instr := "Summarize the conversation above."
	for _, model := range []string{"gpt-4o-mini", "azure/gpt-5-mini", "Qwen/Qwen3-32B"} {
		body := captureBody(t, func(o OpenAI) error {
			o.Model = model
			_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{
				{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &convo}},
				{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &instr}},
			})
			return err
		})
		if s := string(mustJSON(t, body)); strings.Contains(s, "cache_control") {
			t.Errorf("model %q: cache_control reached a non-Anthropic endpoint", model)
		}
	}
}

// Below the model's floor the provider ignores the mark, so sending one buys a write premium for
// an entry nothing can read. CacheablePrefix owns the table; this pins that we consult it.
func TestCompleteMessagesSkipsTheMarkBelowTheModelMinimum(t *testing.T) {
	short := "one short turn"
	instr := "Summarize the conversation above."
	body := captureBody(t, func(o OpenAI) error {
		o.Model = "aws/claude-opus-4-7"
		_, err := o.CompleteMessages(context.Background(), "", []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &short}},
			{Role: bschemas.ChatMessageRoleUser, Content: &bschemas.ChatMessageContent{ContentStr: &instr}},
		})
		return err
	})
	if s := string(mustJSON(t, body)); strings.Contains(s, "cache_control") {
		t.Errorf("marked a prefix below the model minimum — the premium buys an unreadable entry: %s", s)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
