// Claude-Code-shaped request fixtures, shared by the proxy's wire-level tests.
//
// They live in their own file because several test groups need them and each was inventing its own
// body, which is how a fixture ends up not resembling what the client actually sends.
package proxy_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// cachePipeline is a cachesplit-only pipeline: the shape a cache-focused deployment runs, and the
// one whose promise is that nothing else touches the request.
const cachePipeline = "pipeline: [cachesplit]\n"

// attributionText is the block Claude Code prepends as the first system block.
const attributionText = "You are Claude Code, Anthropic's official CLI for Claude."

// jsonStr quotes a Go string as a JSON string.
func jsonStr(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// volatileSystemText is a system block over cachesplit's 1024-token floor (~4 chars/token) that
// ends in the environment snapshot the split exists to move out of the hashed prefix.
func volatileSystemText() string {
	return strings.Repeat("You are a coding agent. Follow the instructions carefully.\n", 120) +
		"\nCurrent branch: main\nRecent commits:\n0898367954 SWE-bench\n"
}

// claudeCodeBody is a Claude-Code-shaped Anthropic request: a small attribution block as the
// FIRST system block, then a large one ending in the volatile environment snapshot, with the
// cache breakpoint at its end.
//
// The JSON is assembled as TEXT rather than marshalled from a map, and that is not fussiness.
// Go's json.Marshal sorts map keys, so a map-built body arrives pre-sorted — and a proxy bug
// that re-encodes blocks instead of passing their original bytes through would then produce
// byte-identical output and no test would notice. Claude Code sends `{"type":...,"text":...}`,
// unsorted, and the API's positional strip of the attribution block depends on those bytes
// arriving unchanged. So the fixture has to carry the real order.
func claudeCodeBody(t *testing.T, stream bool) []byte {
	t.Helper()
	return claudeCodeBodyWithFirst(t, stream, attributionText)
}

func claudeCodeBodyWithFirst(t *testing.T, stream bool, first string) []byte {
	t.Helper()
	body := `{"model":"claude-sonnet-5","max_tokens":64,"stream":` +
		map[bool]string{true: "true", false: "false"}[stream] +
		`,"system":[` +
		`{"type":"text","text":` + jsonStr(first) + `},` +
		`{"type":"text","text":` + jsonStr(volatileSystemText()) + `,"cache_control":{"type":"ephemeral"}}` +
		`],"messages":[{"role":"user","content":"hello"}]}`
	if !json.Valid([]byte(body)) {
		t.Fatalf("test fixture is not valid JSON: %s", body)
	}
	return []byte(body)
}
