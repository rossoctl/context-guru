package apply

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// #1243: the `1-hour-main-agent` strategy relabels EVERY existing breakpoint of a main-thread
// request to 1h, history included, and adds none.

// historyBody is a Claude Code shaped request with a breakpoint on the head AND on the history:
// tools, one system block, a mark on an earlier message and a mark on the last one.
func historyBody() []byte {
	pad := strings.Repeat("context ", 30000)
	return []byte(`{"model":"aws/claude-opus-5",` +
		`"tools":[{"name":"Bash","description":"` + pad + `","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`)
}

func TestMainAgentTTLRelabelsEveryBreakpointAndAddsNone(t *testing.T) {
	body := historyBody()
	out, up := upgradeMainAgentTTL(body, bschemas.Anthropic, 200000)
	if !up {
		t.Fatal("the relabel did not fire")
	}
	for _, path := range []string{"tools.0.cache_control.ttl", "system.0.cache_control.ttl",
		"messages.0.content.0.cache_control.ttl", "messages.2.content.0.cache_control.ttl"} {
		if got := gjson.GetBytes(out, path).String(); got != "1h" {
			t.Errorf("%s = %q, want 1h", path, got)
		}
	}
	if before, after := CountBreakpoints(body), CountBreakpoints(out); before != after {
		t.Errorf("breakpoints changed: %+v -> %+v", before, after)
	}
	if !AllBreakpointsAskExtendedTTL(out) {
		t.Error("a breakpoint is left at 5 minutes: the keeper would not treat the session as 1-hour")
	}
	again, up2 := upgradeMainAgentTTL(out, bschemas.Anthropic, 200000)
	if !up2 || string(again) != string(out) {
		t.Error("a second relabel changed the body")
	}
}

func TestMainAgentTTLGateProviderAndNoBreakpoint(t *testing.T) {
	body := historyBody()
	if _, up := upgradeMainAgentTTL(body, bschemas.Anthropic, len(body)+1); up {
		t.Error("relabelled a body under the size gate")
	}
	if _, up := upgradeMainAgentTTL(body, bschemas.Anthropic, 0); up {
		t.Error("a gate of 0 must ask for nothing")
	}
	if _, up := upgradeMainAgentTTL(body, bschemas.OpenAI, 1); up {
		t.Error("relabelled for a provider with no explicit breakpoints")
	}
	bare := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 300000) + `"}]}`)
	if out, up := upgradeMainAgentTTL(bare, bschemas.Anthropic, 1); up || string(out) != string(bare) {
		t.Error("a request with no breakpoint was changed")
	}
}
