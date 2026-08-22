package apply

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// A Claude Code-shaped request: two `system` breakpoints and one on the last content block,
// which is the (2,0,0,1) layout carrying 86.4% of production spend and 100% of the
// addressable TTL misses. Padded past the 50,000-token size gate.
func headTTLBody() []byte {
	pad := strings.Repeat("context ", 30000) // ~240 KB, comfortably over 50k estimated tokens
	return []byte(`{"model":"aws/claude-opus-5",` +
		`"tools":[{"name":"Bash","description":"` + pad + `","input_schema":{"type":"object"},` +
		`"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"Current branch: main","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi",` +
		`"cache_control":{"type":"ephemeral"}}]}]}`)
}

// The ordering rule: "1-hour entries must appear BEFORE 5-minute ones". The head does by
// construction, so the upgrade must label the head and leave every message breakpoint alone —
// labelling a message breakpoint 1h would produce the illegal order AND be the blanket policy
// that loses money.
func TestHeadTTLUpgradesTheHeadAndNotTheTail(t *testing.T) {
	body := headTTLBody()
	out, up, head := upgradeHeadTTL(body, bschemas.Anthropic, 50000)
	if !up {
		t.Fatal("upgrade did not fire on a Claude Code-shaped body over the size gate")
	}
	for _, path := range []string{"tools.0.cache_control.ttl", "system.0.cache_control.ttl",
		"system.1.cache_control.ttl"} {
		if got := gjson.GetBytes(out, path).String(); got != "1h" {
			t.Errorf("%s = %q, want \"1h\"", path, got)
		}
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl"); got.Exists() {
		t.Errorf("the trailing message breakpoint was labelled %q; the tail must stay on the "+
			"5-minute default, or the wire carries 1h AFTER 5m and the premium lands on every write",
			got.String())
	}
	if head <= 0 {
		t.Error("head tokens not measured; the whole mixed-TTL economics is linear in that share")
	}
}

// The breakpoint budget. The provider's cap is four and 16.1% of production requests are
// already at it, where one more is a 400 rather than a worse price. Re-labelling must never
// add one, and must never write a mark where there was none.
func TestHeadTTLAddsNoBreakpoint(t *testing.T) {
	body := headTTLBody()
	before := CountBreakpoints(body)
	out, _, _ := upgradeHeadTTL(body, bschemas.Anthropic, 50000)
	after := CountBreakpoints(out)
	if before != after {
		t.Fatalf("breakpoints changed: %+v -> %+v", before, after)
	}
	if before.Total() != 4 {
		t.Fatalf("fixture should carry 4 breakpoints, got %d", before.Total())
	}

	// An unmarked head block must stay unmarked: writing a cache_control here is what turns a
	// request at the cap into a 400.
	noMark := []byte(`{"model":"m","system":[{"type":"text","text":"` +
		strings.Repeat("x", 300000) + `"}],"messages":[]}`)
	out2, up2, _ := upgradeHeadTTL(noMark, bschemas.Anthropic, 50000)
	if up2 {
		t.Error("reported an upgrade on a head with no breakpoint to re-label")
	}
	if strings.Contains(string(out2), "cache_control") {
		t.Error("invented a breakpoint on an unmarked head block")
	}
}

// The size gate is a DOLLAR filter: it exists to exclude the small-prefix requests that pay
// the 2.0x premium and can never produce a large miss. Gating on a probability instead leaves
// the net unchanged, which is why this is the gate.
func TestHeadTTLSizeGate(t *testing.T) {
	small := []byte(`{"model":"m","system":[{"type":"text","text":"short",` +
		`"cache_control":{"type":"ephemeral"}}],"messages":[]}`)
	if _, up, _ := upgradeHeadTTL(small, bschemas.Anthropic, 50000); up {
		t.Error("upgraded a request far below the size gate")
	}
	if _, up, _ := upgradeHeadTTL(headTTLBody(), bschemas.Anthropic, 1<<30); up {
		t.Error("upgraded a request below an enormous gate")
	}
}

// Only providers that honour explicit `cache_control` have a TTL to state. On an
// implicit-longest-prefix backend the field is meaningless and must not be written.
func TestHeadTTLOnlyForExplicitBreakpointProviders(t *testing.T) {
	for _, p := range []bschemas.ModelProvider{bschemas.OpenAI, bschemas.ModelProvider("gemini")} {
		if out, up, _ := upgradeHeadTTL(headTTLBody(), p, 50000); up || strings.Contains(string(out), `"ttl"`) {
			t.Errorf("provider %s: wrote a ttl on a backend with no explicit breakpoints", p)
		}
	}
}

// Idempotent: a body that already asks for 1h is reported as upgraded (so its head is
// measured) and comes back unchanged rather than doubly labelled.
func TestHeadTTLIdempotent(t *testing.T) {
	once, _, head1 := upgradeHeadTTL(headTTLBody(), bschemas.Anthropic, 50000)
	twice, up, head2 := upgradeHeadTTL(once, bschemas.Anthropic, 50000)
	if !up {
		t.Error("a body already on 1h reported no upgrade, so its head would go unmeasured")
	}
	if string(once) != string(twice) {
		t.Error("second pass changed the body")
	}
	if head1 != head2 {
		t.Errorf("head tokens differ across passes: %d vs %d", head1, head2)
	}
}

// bodyAsksExtendedTTL is what the cold-cache logic reads to decide how long a prefix may be
// assumed to live, so the upgrade has to be visible to it. If it were not, a request that
// asked for 1h would still be treated as a 5-minute prefix.
func TestHeadTTLIsVisibleToTheColdCacheLogic(t *testing.T) {
	body := headTTLBody()
	if bodyAsksExtendedTTL(body) {
		t.Fatal("fixture already asks for the extended TTL")
	}
	out, _, _ := upgradeHeadTTL(body, bschemas.Anthropic, 50000)
	if !bodyAsksExtendedTTL(out) {
		t.Error("the upgraded body does not read as asking for the extended TTL")
	}
}

// Fail open: a body that is not an object, or whose head arrays are not arrays, comes back
// untouched rather than mangled or panicking.
func TestHeadTTLFailsOpen(t *testing.T) {
	for _, in := range []string{
		`not json at all`,
		`{"system":"a string system prompt","messages":[]}`,
		`{"tools":42,"system":null}`,
		`{}`,
	} {
		big := in + strings.Repeat(" ", 300000) // over the size gate, so the gate is not what refuses
		out, up, _ := upgradeHeadTTL([]byte(big), bschemas.Anthropic, 50000)
		if up {
			t.Errorf("reported an upgrade for %q", in)
		}
		if string(out) != big {
			t.Errorf("body changed for %q", in)
		}
	}
}
