package thread

import (
	"net/http"
	"testing"
)

// The direct cases of IsAgentCompaction. The proxy's end-to-end tests (proxy/agentcompaction_test.go)
// cover the same rule on the wire.
func TestIsAgentCompaction(t *testing.T) {
	const cc = "Your task is to create a detailed summary of the conversation so far."
	const bob = "First, reason in your scratchpad. Then, generate the <state_snapshot>."
	const codex = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary."
	const ocFirst = "You MUST summarize the conversation above into a structured summary that will be given to another agent to resume the work."
	const ocUpdate = "Update the existing checkpoint in the conversation above into one consolidated summary."
	const piFirst = "The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use."
	const piUpdate = "The messages above are NEW conversation messages to incorporate into the existing summary provided in "
	const piPrefix = "The messages above are earlier context from an ongoing conversation. Later messages are stored separately and"
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"string content", `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"` + cc + `"}]}`, true},
		{"content blocks", `{"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"` + cc + `"}]}]}`, true},
		{"trailing system message", `{"messages":[{"role":"user","content":"` + cc + `"},{"role":"system","content":"<total_tokens>1</total_tokens>"}]}`, true},
		{"bob shell", `{"messages":[{"role":"user","content":"` + bob + `"}]}`, true},
		{"phrase in an earlier turn only", `{"messages":[{"role":"user","content":"` + cc + `"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`, false},
		{"assistant last", `{"messages":[{"role":"assistant","content":"` + cc + `"}]}`, false},
		{"ordinary prose", `{"messages":[{"role":"user","content":"please summarize the file"}]}`, false},
		{"no message list", `{"foo":[{"role":"user","content":"` + cc + `"}]}`, false},
		{"responses input items", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codex + `"}]}]}`, true},
		{"responses input, phrase in an earlier item only", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codex + `"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, false},
		{"responses input, last item has no role", `{"input":[{"type":"function_call_output","call_id":"c","output":"` + codex + `"}]}`, false},
		{"responses input string", `{"input":"` + codex + `"}`, true},
		{"opencode first summary", `{"messages":[{"role":"user","content":"` + ocFirst + `\n\nSummarize only"}]}`, true},
		{"opencode update", `{"messages":[{"role":"user","content":"` + ocUpdate + `\n\nNewer"}]}`, true},
		{"pi summary after the conversation", `{"messages":[{"role":"user","content":[{"type":"text","text":"<conversation>\n[User]: hi\n</conversation>\n\n` + piFirst + `\n\nUse this EXACT format"}]}]}`, true},
		{"pi update", `{"messages":[{"role":"user","content":[{"type":"text","text":"x ` + piUpdate + `<previous-summary>"}]}]}`, true},
		{"pi turn prefix", `{"messages":[{"role":"user","content":"` + piPrefix + ` ."}]}`, true},
		{"not json", `nope`, false},
	} {
		if got := IsAgentCompaction([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: IsAgentCompaction = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The signals that need no request text: the plugin header and Codex's own turn metadata.
func TestIsAgentCompactionRequest(t *testing.T) {
	plain := `{"messages":[{"role":"user","content":"hi"}]}`
	meta := func(kind string) string { return `{"request_kind":"` + kind + `","turn_id":"t"}` }
	for _, tc := range []struct {
		name   string
		header map[string]string
		body   string
		want   bool
	}{
		{"plugin header", map[string]string{HeaderRequestKind: "compaction"}, plain, true},
		{"plugin header, other kind", map[string]string{HeaderRequestKind: "title"}, plain, false},
		{"codex header", map[string]string{"x-codex-turn-metadata": meta("compaction")}, `{"input":[]}`, true},
		{"codex header, turn", map[string]string{"x-codex-turn-metadata": meta("turn")}, `{"input":[]}`, false},
		{"codex header, not json", map[string]string{"x-codex-turn-metadata": "nope"}, plain, false},
		{"codex client_metadata", nil, `{"input":[],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}`, true},
		{"codex client_metadata, turn", nil, `{"input":[],"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"turn\"}"}}`, false},
		{"phrase fallback", nil, `{"input":"You are performing a CONTEXT CHECKPOINT COMPACTION. x"}`, true},
		{"nothing", nil, plain, false},
	} {
		h := http.Header{}
		for k, v := range tc.header {
			h.Set(k, v)
		}
		if got := IsAgentCompactionRequest(h, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if IsAgentCompactionRequest(nil, []byte(plain)) {
		t.Error("nil header, plain body: want false")
	}
}
