package expand

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponseCallsOpenAI(t *testing.T) {
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH1\"}"}}
	]}}]}`
	calls, other := ResponseCalls("openai", []byte(resp))
	if other || len(calls) != 1 || calls[0].CallID != "call_1" || calls[0].HashID != "HASH1" {
		t.Fatalf("bad parse: %+v other=%v", calls, other)
	}
}

func TestResponseCallsOtherToolBails(t *testing.T) {
	resp := `{"choices":[{"message":{"tool_calls":[
		{"id":"c1","function":{"name":"context_guru_expand","arguments":"{\"id\":\"H\"}"}},
		{"id":"c2","function":{"name":"do_something_else","arguments":"{}"}}
	]}}]}`
	calls, other := ResponseCalls("openai", []byte(resp))
	if !other || len(calls) != 1 {
		t.Fatalf("expected otherTools=true with one expand call, got %+v other=%v", calls, other)
	}
}

func TestResponseCallsAnthropic(t *testing.T) {
	resp := `{"role":"assistant","content":[
		{"type":"text","text":"let me look"},
		{"type":"tool_use","id":"toolu_1","name":"context_guru_expand","input":{"id":"HASH2"}}
	]}`
	calls, other := ResponseCalls("anthropic", []byte(resp))
	if other || len(calls) != 1 || calls[0].CallID != "toolu_1" || calls[0].HashID != "HASH2" {
		t.Fatalf("bad anthropic parse: %+v other=%v", calls, other)
	}
}

func TestContinuationOpenAIAppendsTurns(t *testing.T) {
	req := `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","function":{"name":"context_guru_expand","arguments":"{\"id\":\"H\"}"}}]}}]}`
	out, ok := Continuation("openai", []byte(req), []byte(resp), map[string]string{"call_1": "THE ORIGINAL"})
	if !ok {
		t.Fatal("continuation failed")
	}
	msgs := gjson.GetBytes(out, "messages")
	if msgs.Get("#").Int() != 3 {
		t.Fatalf("expected 3 messages (user, assistant, tool), got %d: %s", msgs.Get("#").Int(), out)
	}
	if msgs.Get("2.role").String() != "tool" || msgs.Get("2.tool_call_id").String() != "call_1" ||
		!strings.Contains(msgs.Get("2.content").String(), "THE ORIGINAL") {
		t.Fatalf("tool result turn wrong: %s", out)
	}
}

func TestContinuationAnthropicAppendsTurns(t *testing.T) {
	req := `{"messages":[{"role":"user","content":"hi"}]}`
	resp := `{"content":[{"type":"tool_use","id":"toolu_1","name":"context_guru_expand","input":{"id":"H"}}]}`
	out, ok := Continuation("anthropic", []byte(req), []byte(resp), map[string]string{"toolu_1": "ORIG"})
	if !ok {
		t.Fatal("continuation failed")
	}
	msgs := gjson.GetBytes(out, "messages")
	if msgs.Get("#").Int() != 3 || msgs.Get("1.role").String() != "assistant" || msgs.Get("2.role").String() != "user" {
		t.Fatalf("expected user,assistant,user turns: %s", out)
	}
	if msgs.Get("2.content.0.tool_use_id").String() != "toolu_1" || !strings.Contains(msgs.Get("2.content.0.content").String(), "ORIG") {
		t.Fatalf("tool_result wrong: %s", out)
	}
}

// Restoration is APPENDED, never spliced back over the marker. This is a cache property
// before it is anything else: `messages` is hashed in order, so rewriting a message in the
// middle of a transcript invalidates the cached prefix from that point on, while appending
// two turns at the end leaves every cached byte before them intact. The marker therefore
// stays exactly where the offloader put it, hash id and all, and the recovered original
// arrives at the end as a tool_result.
//
// Asserted rather than assumed because nothing in Continuation's signature stops a future
// version from being "helpful" and putting the content back where it came from — which
// would read as a tidier transcript and cost the whole prefix on every expand call.
func TestContinuationAppendsAndNeverRewritesAnEarlierMessage(t *testing.T) {
	const original = "the full original content that was offloaded"
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			req := []byte(`{"messages":[` +
				`{"role":"user","content":"do the thing"},` +
				`{"role":"user","content":"tool output <<cg:k1>>"},` +
				`{"role":"assistant","content":"working on it"}` +
				`]}`)
			var resp []byte
			if provider == "anthropic" {
				resp = []byte(`{"content":[{"type":"tool_use","id":"c1","name":"` + ToolName +
					`","input":{"id":"k1"}}]}`)
			} else {
				resp = []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
					`{"id":"c1","type":"function","function":{"name":"` + ToolName +
					`","arguments":"{\"id\":\"k1\"}"}}]}}]}`)
			}
			before := gjson.GetBytes(req, "messages").Array()
			out, ok := Continuation(provider, req, resp, map[string]string{"c1": original})
			if !ok {
				t.Fatal("Continuation refused a well-formed request/response pair")
			}
			after := gjson.GetBytes(out, "messages").Array()
			if len(after) != len(before)+2 {
				t.Fatalf("messages went %d -> %d; the continuation must APPEND the "+
					"assistant tool-call turn and the tool_result turn and nothing else",
					len(before), len(after))
			}
			// Every byte of the original transcript, unchanged and in place.
			for i, msg := range before {
				if after[i].Raw != msg.Raw {
					t.Errorf("message %d was rewritten, so the cached prefix is lost from "+
						"there on\n got %s\nwant %s", i, after[i].Raw, msg.Raw)
				}
			}
			// The marker specifically: still there, still carrying its hash id. If the
			// original had been spliced back over it, this is what would have vanished.
			if !strings.Contains(after[1].Get("content").String(), Marker("k1")) {
				t.Errorf("the marker was replaced by the restored content at its original "+
					"index; it must stay put and the content must arrive at the end: %s",
					after[1].Raw)
			}
			// And the recovered original is at the END, not in the middle.
			if !strings.Contains(after[len(after)-1].Raw, original) {
				t.Errorf("the restored original is not in the last message: %s",
					after[len(after)-1].Raw)
			}
			for i := 0; i < len(before); i++ {
				if strings.Contains(after[i].Raw, original) {
					t.Errorf("the restored original leaked into message %d, which is inside "+
						"the cached prefix", i)
				}
			}
		})
	}
}
