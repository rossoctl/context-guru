package expand_test

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/expand"
	"github.com/tidwall/gjson"
)

// The turn a client produces after receiving our own tool_use: the assistant's expand call
// echoed back, answered by the client's own error, because no client implements the tool.
const ccError = "<tool_use_error>Error: No such tool available: context_guru_expand</tool_use_error>"

func anthropicTranscript(toolName, callID, hashID, resultText string) string {
	return `{"model":"claude","messages":[` +
		`{"role":"user","content":"look at <<cg:` + hashID + `>>"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"let me fetch that"},` +
		`{"type":"tool_use","id":"` + callID + `","name":"` + toolName + `","input":{"id":"` + hashID + `"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + callID + `",` +
		`"content":"` + resultText + `","is_error":true}]}]}`
}

func openAITranscript(toolName, callID, hashID, resultText string) string {
	return `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"look at <<cg:` + hashID + `>>"},` +
		`{"role":"assistant","tool_calls":[{"id":"` + callID + `","type":"function",` +
		`"function":{"name":"` + toolName + `","arguments":"{\"id\":\"` + hashID + `\"}"}}]},` +
		`{"role":"tool","tool_call_id":"` + callID + `","content":"` + resultText + `"}]}`
}

func resolver(have map[string]string) func(string) (string, bool) {
	return func(id string) (string, bool) {
		v, ok := have[id]
		return v, ok
	}
}

// The bug as the user saw it, from the request side: the model asked for offloaded content
// and got told the tool does not exist. It must get the content instead.
func TestRepairReplacesTheClientsErrorWithTheOriginal(t *testing.T) {
	for _, tc := range []struct {
		provider string
		body     string
		content  string
	}{
		{"anthropic", anthropicTranscript(expand.ToolName, "toolu_1", "HASH", ccError),
			"messages.2.content.0.content"},
		{"openai", openAITranscript(expand.ToolName, "call_1", "HASH", ccError),
			"messages.2.content"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			out, restored := expand.RepairToolResults(tc.provider, []byte(tc.body),
				resolver(map[string]string{"HASH": "THE ORIGINAL CONTENT"}))
			if got := gjson.GetBytes(out, tc.content).String(); got != "THE ORIGINAL CONTENT" {
				t.Fatalf("tool_result content = %q, want the original", got)
			}
			if strings.Contains(string(out), "No such tool available") {
				t.Fatalf("the client's error must not reach the model:\n%s", out)
			}
			if len(restored) != 1 || restored[0] != "THE ORIGINAL CONTENT" {
				t.Fatalf("restored = %q, want the one original (the caller marks it kept-verbatim)", restored)
			}
			// The block is no longer an error: leaving the flag set tells the model its call
			// failed while handing it that call's result.
			if gjson.GetBytes(out, "messages.2.content.0.is_error").Bool() {
				t.Fatalf("is_error must be cleared:\n%s", out)
			}
			// Byte-stable: the client keeps its own copy of the error, so the same repair is
			// needed on every later turn and must produce the same bytes or the provider's
			// prefix cache misses every turn.
			again, _ := expand.RepairToolResults(tc.provider, out, resolver(map[string]string{"HASH": "THE ORIGINAL CONTENT"}))
			if string(again) != string(out) {
				t.Fatalf("repair is not idempotent:\n first %s\n then  %s", out, again)
			}
		})
	}
}

// An id that has aged out of the store gets the same words the continuation loop uses. "The
// content expired" is a fact the model can act on; "there is no such tool" invites it to
// give up on a tool this proxy advertises on every later turn.
func TestRepairSubstitutesTheSharedPlaceholderForAnExpiredID(t *testing.T) {
	out, restored := expand.RepairToolResults("anthropic",
		[]byte(anthropicTranscript(expand.ToolName, "toolu_1", "GONE", ccError)),
		resolver(nil))
	got := gjson.GetBytes(out, "messages.2.content.0.content").String()
	if got != expand.Unavailable("GONE") {
		t.Fatalf("content = %q, want %q", got, expand.Unavailable("GONE"))
	}
	if len(restored) != 0 {
		t.Fatalf("nothing was recovered, so nothing is kept verbatim: %q", restored)
	}
}

// It must touch nothing else. A tool_result answering a tool the CLIENT owns is the client's
// business, whatever it says — including a genuine `No such tool available` for a tool the
// model hallucinated.
func TestRepairLeavesEveryOtherToolResultAlone(t *testing.T) {
	for _, body := range []string{
		anthropicTranscript("Bash", "toolu_1", "HASH", ccError),
		openAITranscript("Bash", "call_1", "HASH", ccError),
		`{"messages":[{"role":"user","content":"plain text turn"}]}`,
		`{"messages":[]}`,
		`{"model":"claude"}`,
		`not json at all`,
	} {
		provider := "anthropic"
		if strings.Contains(body, "gpt-x") {
			provider = "openai"
		}
		out, restored := expand.RepairToolResults(provider, []byte(body),
			resolver(map[string]string{"HASH": "THE ORIGINAL CONTENT"}))
		if string(out) != body {
			t.Fatalf("body was rewritten:\n want %s\n got  %s", body, out)
		}
		if len(restored) != 0 {
			t.Fatalf("nothing to restore, got %q", restored)
		}
	}
}

// Two calls in one turn, one resolvable and one not: every answer is repaired, and the
// batch does not stop at the first miss.
func TestRepairHandlesEveryCallInATurn(t *testing.T) {
	body := `{"model":"claude","messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"t1","name":"` + expand.ToolName + `","input":{"id":"GOOD"}},` +
		`{"type":"tool_use","id":"t2","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"t3","name":"` + expand.ToolName + `","input":{"id":"GONE"}}]},` +
		`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":"` + ccError + `","is_error":true},` +
		`{"type":"tool_result","tool_use_id":"t2","content":"file1 file2"},` +
		`{"type":"tool_result","tool_use_id":"t3","content":"` + ccError + `","is_error":true}]}]}`
	out, restored := expand.RepairToolResults("anthropic", []byte(body),
		resolver(map[string]string{"GOOD": "RECOVERED"}))
	if got := gjson.GetBytes(out, "messages.1.content.0.content").String(); got != "RECOVERED" {
		t.Fatalf("first call: %q", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.1.content").String(); got != "file1 file2" {
		t.Fatalf("the client's own tool result was touched: %q", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.2.content").String(); got != expand.Unavailable("GONE") {
		t.Fatalf("second call: %q", got)
	}
	if len(restored) != 1 {
		t.Fatalf("restored = %q, want only the resolvable one", restored)
	}
	if strings.Contains(string(out), "No such tool available") {
		t.Fatalf("an error for OUR tool survived:\n%s", out)
	}
}

// F3: an id-less tool_use for our tool must not make "" a live key. A tool_result carrying no
// tool_use_id is somebody else's block, and overwriting its content is data loss even when the
// value written is only a placeholder.
func TestRepairNeverMatchesAnIDLessBlock(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude","messages":[` +
			`{"role":"assistant","content":[{"type":"tool_use","name":"` + expand.ToolName + `","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","content":"SOMEONE ELSE'S RESULT"}]}]}`,
		`{"model":"gpt-x","messages":[` +
			`{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"` + expand.ToolName + `","arguments":"{}"}}]},` +
			`{"role":"tool","content":"SOMEONE ELSE'S RESULT"}]}`,
	} {
		provider := "anthropic"
		if strings.Contains(body, "gpt-x") {
			provider = "openai"
		}
		out, restored := expand.RepairToolResults(provider, []byte(body), resolver(nil))
		if string(out) != body {
			t.Fatalf("%s: an id-less pair was rewritten:\n want %s\n got  %s", provider, body, out)
		}
		if len(restored) != 0 {
			t.Fatalf("%s: restored %q", provider, restored)
		}
	}
}
