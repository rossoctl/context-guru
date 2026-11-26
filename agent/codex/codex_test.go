package codex

import (
	"encoding/json"
	"net/http"
	"testing"
)

const (
	sid = "019a7f2c-1d3e-7b4a-9c5d-2e8f6a0b1c34"
	tid = "019a7f2c-aaaa-7b4a-9c5d-2e8f6a0b1c35"
	pid = "019a7f2c-bbbb-7b4a-9c5d-2e8f6a0b1c36"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestIdentifyReadsTheIdsForCodexCallers(t *testing.T) {
	for name, tc := range map[string]struct {
		h    http.Header
		want Fields
	}{
		"cli":              {hdr("User-Agent", "codex_cli_rs/0.162.1 (Mac OS 15; arm64) iTerm", "session-id", sid), Fields{SessionID: sid}},
		"exec":             {hdr("User-Agent", "codex_exec/0.162.1", "session-id", sid), Fields{SessionID: sid}},
		"tui":              {hdr("User-Agent", "codex-tui/0.162.1", "session-id", sid), Fields{SessionID: sid}},
		"upper case":       {hdr("User-Agent", "Codex_CLI_RS/1", "session-id", " "+sid+" "), Fields{SessionID: sid}},
		"originator only":  {hdr("User-Agent", "MyIDE/1.0", "Originator", "codex_vscode", "session-id", sid), Fields{SessionID: sid}},
		"main thread":      {hdr("User-Agent", "codex_cli_rs/1", "session-id", sid, "thread-id", tid), Fields{SessionID: sid, AgentID: tid}},
		"subagent":         {hdr("User-Agent", "codex_cli_rs/1", "session-id", sid, "thread-id", tid, "x-codex-parent-thread-id", pid), Fields{sid, tid, pid}},
		"codex, no ids":    {hdr("User-Agent", "codex_cli_rs/1"), Fields{}},
		"codex, blank sid": {hdr("User-Agent", "codex_cli_rs/1", "session-id", "  "), Fields{}},
	} {
		got, ok := Identify(tc.h)
		if !ok || got != tc.want {
			t.Errorf("%s: Identify = %+v, %v; want %+v, true", name, got, ok, tc.want)
		}
	}
}

func TestIdentifyRefusesOtherCallers(t *testing.T) {
	for name, h := range map[string]http.Header{
		"nil":             nil,
		"empty":           {},
		"other agent":     hdr("User-Agent", "python-requests/2", "session-id", sid, "thread-id", tid),
		"no user agent":   hdr("session-id", sid),
		"claude":          hdr("User-Agent", "claude-cli/2.1 (external, cli)", "session-id", sid),
		"codex mid-text":  hdr("User-Agent", "my-codex-tool/1", "session-id", sid),
		"short user agnt": hdr("User-Agent", "cod", "session-id", sid),
	} {
		if got, ok := Identify(h); ok || got != (Fields{}) {
			t.Errorf("%s: Identify = %+v, %v; want zero, false", name, got, ok)
		}
	}
}

func TestIsCompaction(t *testing.T) {
	meta := func(kind string) string { return `{"request_kind":"` + kind + `","turn_id":"t"}` }
	body := func(kind string) []byte {
		b, _ := json.Marshal(map[string]any{"input": []any{}, "client_metadata": map[string]string{"x-codex-turn-metadata": meta(kind)}})
		return b
	}
	hdr := func(v string) http.Header { h := codexHeader(); h.Set("x-codex-turn-metadata", v); return h }
	for _, tc := range []struct {
		name string
		h    http.Header
		body []byte
		want bool
	}{
		{"header", hdr(meta("compaction")), []byte(`{}`), true},
		{"header, turn", hdr(meta("turn")), []byte(`{}`), false},
		{"header, not json", hdr("nope"), []byte(`{}`), false},
		{"body client_metadata", codexHeader(), body("compaction"), true},
		{"body client_metadata, turn", codexHeader(), body("turn"), false},
		{"nothing", codexHeader(), []byte(`{"input":[]}`), false},
		{"nil header", nil, body("compaction"), false},
		{"not Codex, header", func() http.Header { h := hdr(meta("compaction")); h.Set("User-Agent", "curl/8"); return h }(), []byte(`{}`), false},
		{"originator Codex only", func() http.Header {
			h := http.Header{}
			h.Set("Originator", "codex_vscode")
			h.Set("x-codex-turn-metadata", meta("compaction"))
			return h
		}(), []byte(`{}`), true},
	} {
		if got := IsCompaction(tc.h, tc.body); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A Codex caller: its User-Agent starts with its originator.
func codexHeader() http.Header {
	h := http.Header{}
	h.Set("User-Agent", "codex_cli_rs/0.172.0 (Mac OS)")
	return h
}
