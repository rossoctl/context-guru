package expand

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/sjson"
)

func TestParseMarkersDistinctInOrder(t *testing.T) {
	got := ParseMarkers("a <<cg:K1>> b <<cg:K2>> c <<cg:K1>>")
	if len(got) != 2 || got[0] != "K1" || got[1] != "K2" {
		t.Fatalf("ParseMarkers=%v want [K1 K2] first-seen, deduped", got)
	}
	if ParseMarkers("no markers here") != nil {
		t.Fatal("text without markers must return nil")
	}
}

// escLT / escGT are the JSON \uXXXX escapes Go's encoders emit for "<" and ">",
// obtained by ASKING encoding/json rather than hand-writing them, so a fixture can
// never claim an escape form the encoder does not actually produce.
var escLT, escGT = jsonEscape('<'), jsonEscape('>')

// jsonEscape marshals a single rune with HTML escaping on and returns the escape
// sequence encoding/json chose for it (e.g. "<" for '<').
func jsonEscape(r rune) string {
	b, err := json.Marshal(string(r))
	if err != nil {
		panic(err)
	}
	s := strings.Trim(string(b), `"`)
	if !strings.HasPrefix(s, `\u`) {
		panic("encoding/json no longer escapes " + string(r) + " (got " + s + "); " +
			"the raw-body marker matcher's escaped-form handling needs revisiting")
	}
	return s
}

// upperHex uppercases the hex digits of a \uXXXX escape but not the "u" ("<"
// -> "<"), matching what a non-Go JSON encoder may legitimately emit.
func upperHex(esc string) string {
	return `\u` + strings.ToUpper(strings.TrimPrefix(esc, `\u`))
}

// TestHasMarkersInMessagesIgnoresOwnInjectedTool is the regression guard for the
// tautology of issue #26: the injected expand tool's own description quotes the
// marker syntax, so a whole-body marker check was always true and every streaming
// response got buffered. The check must look only at model-visible content.
func TestHasMarkersInMessagesIgnoresOwnInjectedTool(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		body := []byte(`{"messages":[{"role":"user","content":"hello"}],"tools":[]}`)
		injected, ok := Inject(provider, InjectAlways, body, true)
		if !ok {
			t.Fatalf("%s: Inject should have fired", provider)
		}
		// The escaped marker shape IS in the injected bytes — that is exactly what used
		// to make the old whole-body check unconditionally true.
		if !strings.Contains(string(injected), escLT+escLT+"cg:") {
			t.Fatalf("%s: expected the tool description to carry an escaped marker: %s", provider, injected)
		}
		if HasMarkersInMessages(injected) {
			t.Fatalf("%s: our own injected tool must not count as a marker: %s", provider, injected)
		}
	}
}

// TestHasMarkersInMessagesEscapedForm pins the load-bearing accident: markers reach
// the wire HTML-escaped whenever the value they were appended to contains a newline
// (sjson/encoding/json escape "<"). Both spellings must be found.
func TestHasMarkersInMessagesEscapedForm(t *testing.T) {
	escMarker := escLT + escLT + "cg:ABC" + escGT + escGT
	cases := map[string]string{
		"plain":                   `{"messages":[{"role":"user","content":"see <<cg:ABC>>"}]}`,
		"escaped":                 `{"messages":[{"role":"user","content":"see ` + escMarker + `"}]}`,
		"escaped after a newline": `{"messages":[{"role":"user","content":"line1\nline2 ` + escMarker + `"}]}`,
		"summary sentinel":        `{"messages":[{"role":"user","content":"` + SummaryMarker + ` compacted"}]}`,
		"in the system prompt":    `{"system":"context: ` + escMarker + `","messages":[]}`,
		"escaped in tool_result":  `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"out\n` + escMarker + `"}]}]}`,
		// < is as valid an escape as <. Missing it would be a false negative:
		// a real expand call streamed past uninspected.
		"uppercase hex escape": `{"messages":[{"role":"user","content":"see ` +
			upperHex(escLT) + upperHex(escLT) + `cg:ABC` +
			upperHex(escGT) + upperHex(escGT) + `"}]}`,
	}
	for name, body := range cases {
		if !HasMarkersInMessages([]byte(body)) {
			t.Errorf("%s: marker not found in %s", name, body)
		}
	}
	// Real sjson output, not a hand-written string: prove the escaping happens exactly
	// as offload triggers it (marker appended after a newline).
	sjsonBody, _ := sjson.SetBytes([]byte(`{"messages":[{"role":"user","content":""}]}`),
		"messages.0.content", "line1\nline2 "+Marker("XYZ"))
	if !strings.Contains(string(sjsonBody), escLT+escLT+"cg:XYZ") {
		t.Fatalf("expected sjson to escape the marker after a newline: %s", sjsonBody)
	}
	if !HasMarkersInMessages(sjsonBody) {
		t.Fatalf("sjson-escaped marker must be found: %s", sjsonBody)
	}

	for name, body := range map[string]string{
		"no markers":                   `{"messages":[{"role":"user","content":"nothing here"}]}`,
		"prefix only but not a marker": `{"messages":[{"role":"user","content":"cg: and ` + escLT + `cg:"}]}`,
	} {
		if HasMarkersInMessages([]byte(body)) {
			t.Errorf("%s: false positive on %s", name, body)
		}
	}
}

func TestResolve(t *testing.T) {
	st := store.NewMemory(store.Options{})
	if _, ok := Resolve(st, "absent"); ok {
		t.Fatal("absent key must miss")
	}
	st.Put("k", []byte("orig"))
	if v, ok := Resolve(st, "k"); !ok || v != "orig" {
		t.Fatalf("Resolve=%q ok=%v", v, ok)
	}
}

func TestContinuationFailsOpenOnBadShape(t *testing.T) {
	if _, ok := Continuation("anthropic", []byte(`{"messages":[]}`), []byte(`{}`), nil); ok {
		t.Fatal("anthropic response with no content must fail open (ok=false)")
	}
	if _, ok := Continuation("openai", []byte(`{"messages":[]}`), []byte(`{}`), nil); ok {
		t.Fatal("openai response with no message must fail open (ok=false)")
	}
}

func TestResponseCallsNoExpandCall(t *testing.T) {
	// A plain assistant answer with no tool calls => no expand calls, no other tools.
	calls, other := ResponseCalls("openai", []byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	if len(calls) != 0 || other {
		t.Fatalf("plain answer => no calls; got calls=%v other=%v", calls, other)
	}
}
