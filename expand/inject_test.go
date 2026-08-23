package expand

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func toolNames(t *testing.T, body []byte, provider string) []string {
	field := "function.name"
	if provider == "anthropic" {
		field = "name"
	}
	var out []string
	for _, tl := range gjson.GetBytes(body, "tools").Array() {
		out = append(out, tl.Get(field).String())
	}
	return out
}

func TestInjectOpenAIAddsToolLast(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[{"role":"user","content":"out: <<cg:k1>>"}]}`)
	out, injected := Inject("openai", InjectAuto, body, true)
	if !injected {
		t.Fatal("expected injection when tools present + store persists")
	}
	names := toolNames(t, out, "openai")
	if len(names) != 2 || names[0] != "read_file" || names[1] != ToolName {
		t.Fatalf("expand tool must be appended LAST, got %v", names)
	}
}

func TestInjectAnthropicAddsToolLast(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"name":"bash","input_schema":{}}],"messages":[{"role":"user","content":"out: <<cg:k1>>"}]}`)
	out, injected := Inject("anthropic", InjectAuto, body, true)
	if !injected {
		t.Fatal("expected injection")
	}
	names := toolNames(t, out, "anthropic")
	if len(names) != 2 || names[1] != ToolName {
		t.Fatalf("expand tool must be last, got %v", names)
	}
}

func TestInjectIdempotent(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[{"role":"user","content":"out: <<cg:k1>>"}]}`)
	once, _ := Inject("openai", InjectAuto, body, true)
	twice, injected := Inject("openai", InjectAuto, once, true)
	if injected {
		t.Fatal("second inject must be a no-op")
	}
	if !bytes.Equal(once, twice) {
		t.Fatal("idempotent inject must return byte-identical body")
	}
}

func TestInjectDeterministicBytes(t *testing.T) {
	// Byte-stable across calls (prefix-cache stability).
	base := []byte(`{"tools":[{"type":"function","function":{"name":"a"}}],"messages":[{"role":"user","content":"out: <<cg:k1>>"}]}`)
	a, _ := Inject("openai", InjectAuto, base, true)
	b, _ := Inject("openai", InjectAuto, base, true)
	if !bytes.Equal(a, b) {
		t.Fatal("injected bytes must be deterministic")
	}
	// And the tool def itself is valid JSON with a stable shape.
	if !json.Valid(ToolDefRaw("openai")) || !json.Valid(ToolDefRaw("anthropic")) {
		t.Fatal("ToolDefRaw must be valid JSON")
	}
}

func TestInjectAutoSkipsWhenNoTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	_, injected := Inject("openai", InjectAuto, body, true)
	if injected {
		t.Fatal("auto must NOT inject when the request declares no tools")
	}
}

func TestInjectAlwaysCreatesToolsArray(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, injected := Inject("openai", InjectAlways, body, true)
	if !injected {
		t.Fatal("always must inject even with no tools")
	}
	if got := toolNames(t, out, "openai"); len(got) != 1 || got[0] != ToolName {
		t.Fatalf("always must create tools=[expand], got %v", got)
	}
}

func TestInjectNeverAndNoStore(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[{"role":"user","content":"out: <<cg:k1>>"}]}`)
	if _, in := Inject("openai", InjectNever, body, true); in {
		t.Fatal("never must not inject")
	}
	if _, in := Inject("openai", InjectAuto, body, false); in {
		t.Fatal("must not inject when store cannot persist (nothing to expand)")
	}
}

func TestInjectRespectsForcingToolChoice(t *testing.T) {
	// OpenAI required, and a specific forced function, and none: all skip.
	for _, tc := range []string{`"required"`, `"none"`, `{"type":"function","function":{"name":"x"}}`} {
		body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[{"role":"user","content":"<<cg:k1>>"}],"tool_choice":` + tc + `}`)
		if _, in := Inject("openai", InjectAuto, body, true); in {
			t.Fatalf("must skip injection under forcing tool_choice %s", tc)
		}
	}
	// auto is fine.
	body := []byte(`{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[{"role":"user","content":"<<cg:k1>>"}],"tool_choice":"auto"}`)
	if _, in := Inject("openai", InjectAuto, body, true); !in {
		t.Fatal("tool_choice auto should allow injection")
	}
	// Anthropic {"type":"any"} is forcing; {"type":"auto"} is fine.
	if _, in := Inject("anthropic", InjectAuto, []byte(`{"tools":[{"name":"x"}],"messages":[{"role":"user","content":"<<cg:k1>>"}],"tool_choice":{"type":"any"}}`), true); in {
		t.Fatal("anthropic tool_choice any must skip")
	}
	if _, in := Inject("anthropic", InjectAuto, []byte(`{"tools":[{"name":"x"}],"messages":[{"role":"user","content":"<<cg:k1>>"}],"tool_choice":{"type":"auto"}}`), true); !in {
		t.Fatal("anthropic tool_choice auto should allow injection")
	}
}

// The tools array a session sends must be byte-identical on every request in it. `tools`
// sits ahead of `system` and `messages` in the provider's cache hash, so any change to it
// invalidates the ENTIRE cached prefix — and an advertise condition that reads the TURN
// makes the array a per-turn value.
//
// This replaces TestInjectAutoRequiresMarkers, which asserted the opposite: that "auto"
// advertises only on a marker-bearing request. That condition existed to keep an
// unresolvable expand call from reaching a client, which is now handled where it belongs,
// at the resolution (proxy.serve continues on a placeholder tool_result instead of
// replaying the model's raw tool_use). The old condition bought that safety with the
// prefix: the array grew on the first offloading turn and shrank again on the next turn
// that carried no marker, and each NEW variant re-created the whole prefix at the write
// rate. (Measured caveat, because the stronger version of this claim is wrong: alternating
// back to a previous array READS, since the provider keeps both lineages alive within the
// TTL. The cost is one full write per new variant, not one per flip.)
func TestInjectAutoIsByteStableAcrossTurnsWithAndWithoutMarkers(t *testing.T) {
	const tools = `"tools":[{"function":{"name":"x"}}]`
	turns := []struct {
		name string
		body string
	}{
		{"early turn, nothing offloaded yet", `{` + tools + `,"messages":[{"role":"user","content":"plain turn"}]}`},
		{"first offload, marker present", `{` + tools + `,"messages":[{"role":"user","content":"out <<cg:k1>>"}]}`},
		// The spelling markers actually arrive in: encoding/json HTML-escapes "<".
		{"escaped marker", `{` + tools + `,"messages":[{"role":"user","content":"out \u003c\u003ccg:k1\u003e\u003e"}]}`},
		{"later turn, markers gone again", `{` + tools + `,"messages":[{"role":"user","content":"expanded, nothing left to recover"}]}`},
	}
	var want string
	for _, tc := range turns {
		out, injected := Inject("openai", InjectAuto, []byte(tc.body), true)
		if !injected {
			t.Fatalf("%s: not advertised. Every turn in a session must carry the same tools "+
				"array; a turn that omits it pays a whole-prefix cache miss, and so does the "+
				"next turn that puts it back", tc.name)
		}
		got := gjson.GetBytes(out, "tools").Raw
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Errorf("%s: tools array differs from the first turn's, so the cached prefix is "+
				"lost\n got %s\nwant %s", tc.name, got, want)
		}
	}
}

// The one case "auto" still declines, and the reason is unchanged: a request that uses no
// tools at all is the riskiest one to perturb, because a model that never saw a tool may
// penalize an unexpected one. There is no cache argument against declining it either — a
// client that sends no tools sends none on every turn, so the array is stable at absent.
func TestInjectAutoStillLeavesAToollessRequestAlone(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"out <<cg:k1>>"}]}`)
	if _, injected := Inject("openai", InjectAuto, body, true); injected {
		t.Error("auto injected into a request that declares no tools")
	}
	// And "always" is the mode that exists for the opposite choice.
	if _, injected := Inject("openai", InjectAlways, body, true); !injected {
		t.Error(`"always" must inject even with no tools array; that is the only thing that ` +
			"distinguishes it from auto now")
	}
}
