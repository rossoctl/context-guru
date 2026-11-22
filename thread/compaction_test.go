package thread

import "testing"

// The direct cases of IsAgentCompaction. The proxy's end-to-end tests (proxy/agentcompaction_test.go)
// cover the same rule on the wire.
func TestIsAgentCompaction(t *testing.T) {
	const cc = "Your task is to create a detailed summary of the conversation so far."
	const bob = "First, reason in your scratchpad. Then, generate the <state_snapshot>."
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
		{"no message list", `{"input":[{"role":"user","content":"` + cc + `"}]}`, false},
		{"not json", `nope`, false},
	} {
		if got := IsAgentCompaction([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: IsAgentCompaction = %v, want %v", tc.name, got, tc.want)
		}
	}
}
