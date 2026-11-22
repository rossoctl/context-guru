package apply

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// #1243: RelabelAllTTL1h puts every existing breakpoint on the one-hour tier. The 1-hour-on-wait
// strategy applies it to the ping body.
func relabelBody() []byte {
	return []byte(`{"model":"m",` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("ok ", 10) + `"}]},` +
		`{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
}

func TestRelabelAllTTL1hRelabelsEveryBreakpointAndAddsNone(t *testing.T) {
	body := relabelBody()
	out, up := RelabelAllTTL1h(body, bschemas.Anthropic)
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
		t.Error("a breakpoint is left at 5 minutes")
	}
	// Bytes differ from the input, so a gateway response cache that replays a byte-identical body
	// cannot answer the ping (see the keeper's comment).
	if string(out) == string(body) {
		t.Error("the relabelled body equals the input")
	}
	again, up2 := RelabelAllTTL1h(out, bschemas.Anthropic)
	if !up2 || string(again) != string(out) {
		t.Error("a second relabel changed the body")
	}
}

func TestRelabelAllTTL1hProviderAndNoBreakpoint(t *testing.T) {
	if _, up := RelabelAllTTL1h(relabelBody(), bschemas.OpenAI); up {
		t.Error("relabelled for a provider with no explicit breakpoints")
	}
	bare := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if out, up := RelabelAllTTL1h(bare, bschemas.Anthropic); up || string(out) != string(bare) {
		t.Error("a request with no breakpoint was changed")
	}
	if out, up := RelabelAllTTL1h([]byte(`not json`), bschemas.Anthropic); up || string(out) != `not json` {
		t.Error("garbage was changed")
	}
}
