package proxy

import (
	"net/http/httptest"
	"testing"
)

func TestIdentifyNilRequestIsGeneric(t *testing.T) {
	if c := identify(nil); c.Label != "unknown" || c.Name != "" {
		t.Fatalf("identify(nil) = %+v", c)
	}
}

func TestClientHeaderIsNotForwarded(t *testing.T) {
	src := httptest.NewRequest("POST", "/", nil).Header
	src.Set("x-context-guru-client", "pi")
	dst := httptest.NewRequest("POST", "/", nil).Header
	copyHeaders(dst, src)
	if v := dst.Get("x-context-guru-client"); v != "" {
		t.Fatalf("x-context-guru-client was forwarded upstream: %q", v)
	}
}

func TestClientOfNamesMatchTheCompactionSwitch(t *testing.T) {
	for ua, want := range map[string]string{"claude-cli/2 (external, cli)": "claude", "bobshell/1": "bob", "codex_cli_rs/1": "codex", "opencode/1": "opencode", "": ""} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("User-Agent", ua)
		if got := identify(r).Name; got != want {
			t.Errorf("%q: Name = %q, want %q", ua, got, want)
		}
	}
}

func TestModelAgentOfKeepsThePiProviderToken(t *testing.T) {
	for _, tc := range [][3]string{{"pi", "openai", "openai"}, {"pi", "anthropic", "anthropic"}, {"pi", "bedrock", "bedrock"}, {"claude-cli", "anthropic", "claude-cli"}, {"openai", "openai", "openai"}} {
		if got := modelAgentOf(tc[0], tc[1]); got != tc[2] {
			t.Errorf("modelAgentOf(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
