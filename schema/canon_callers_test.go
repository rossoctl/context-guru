package schema_test

import (
	"strconv"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
)

// One table of wire variations for every place that compares a message across requests (#783).
// A pair in sameMessage is one message as a client sends it on two requests; every caller must
// see the same message. A pair in otherMessage is two messages; every caller must tell them apart.

type pair struct {
	name string
	a, b string
	// orderSensitive marks a pair that differs only in key order. internal/thread hashes keys in
	// wire order on purpose (one streaming pass, and a client re-sends its history verbatim), so
	// it is not held to those pairs. See callers.
	orderSensitive bool
}

var sameMessage = []pair{
	{name: "marker moved off a block",
		a: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}`,
		b: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`},
	{name: "marker moved off the message",
		a: `{"role":"user","content":"hi","cache_control":{"type":"ephemeral"}}`,
		b: `{"role":"user","content":"hi"}`},
	{name: "string vs one text block",
		a: `{"role":"user","content":"fix the bug"}`,
		b: `{"role":"user","content":[{"type":"text","text":"fix the bug"}]}`},
	{name: "string vs one text block with a marker (Claude Code 2.1.312)",
		a: `{"role":"user","content":"fix the bug"}`,
		b: `{"role":"user","content":[{"type":"text","text":"fix the bug","cache_control":{"type":"ephemeral"}}]}`},
	{name: "assistant string vs one text block",
		a: `{"role":"assistant","content":"done"}`,
		b: `{"role":"assistant","content":[{"type":"text","text":"done"}]}`},
	{name: "escaped text, string vs block",
		a: `{"role":"user","content":"a \"q\" é\nb"}`,
		b: `{"role":"user","content":[{"type":"text","text":"a \"q\" é\nb"}]}`},
	{name: "key order", orderSensitive: true,
		a: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
		b: `{"content":[{"text":"a","type":"text"},{"type":"text","text":"b"}],"role":"user"}`},
	{name: "key order, string vs block", orderSensitive: true,
		a: `{"content":"hi","role":"user"}`,
		b: `{"role":"user","content":[{"text":"hi","type":"text"}]}`},
	{name: "integer above 2^53, marker moved",
		a: `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"n":9007199254740993},"cache_control":{"type":"ephemeral"}}]}`,
		b: `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"n":9007199254740993}}]}`},
	{name: "nested marker in tool_result content",
		a: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out","cache_control":{"type":"ephemeral"}}]}]}`,
		b: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out"}]}]}`},
	{name: "tool_result content, string vs one text block",
		a: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}`,
		b: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out","cache_control":{"type":"ephemeral"}}]}]}`},
}

var otherMessage = []pair{
	{name: "different text",
		a: `{"role":"user","content":"hi"}`,
		b: `{"role":"user","content":"ho"}`},
	{name: "different text, string vs block",
		a: `{"role":"user","content":"hi"}`,
		b: `{"role":"user","content":[{"type":"text","text":"ho"}]}`},
	{name: "two blocks vs the joined string",
		a: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
		b: `{"role":"user","content":"ab"}`},
	{name: "two blocks vs one block of the joined text",
		a: `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
		b: `{"role":"user","content":[{"type":"text","text":"ab"}]}`},
	{name: "different role",
		a: `{"role":"user","content":"hi"}`,
		b: `{"role":"assistant","content":"hi"}`},
	{name: "integers above 2^53 that differ by 1",
		a: `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"n":9007199254740993}}]}`,
		b: `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"n":9007199254740992}}]}`},
}

type caller struct {
	name string
	// hash returns the caller's identity of one wire message.
	hash func(t *testing.T, msg string) string
	// orderSensitive: see pair.orderSensitive.
	orderSensitive bool
}

func body(msg string) []byte { return []byte(`{"messages":[` + msg + `]}`) }

var callers = []caller{
	{name: "schema.CanonicalWireMessage", hash: func(t *testing.T, msg string) string {
		b, ok := schema.CanonicalWireMessage(msg)
		if !ok {
			t.Fatalf("not canonicalized: %s", msg)
		}
		return string(b)
	}},
	{name: "expand.Fingerprint", hash: func(t *testing.T, msg string) string {
		fp := expand.Fingerprint(gjson.Parse(msg))
		if fp == "" {
			t.Fatalf("no fingerprint: %s", msg)
		}
		return fp
	}},
	{name: "components.SpanHash over apply.NormalizeMessages", hash: func(t *testing.T, msg string) string {
		norm := apply.NormalizeMessages(bschemas.Anthropic, body(msg))
		if len(norm) == 0 {
			t.Fatalf("nothing normalized: %s", msg)
		}
		return components.SpanHash(norm)
	}},
	{name: "thread.Fingerprint", orderSensitive: true, hash: func(t *testing.T, msg string) string {
		sums, ok := thread.Fingerprint(body(msg))
		if !ok {
			t.Fatalf("no thread fingerprint: %s", msg)
		}
		return strconv.FormatUint(sums[0], 16)
	}},
}

func TestEveryCallerSeesTheSameMessage(t *testing.T) {
	for _, c := range callers {
		for _, p := range sameMessage {
			if p.orderSensitive && c.orderSensitive {
				continue
			}
			if c.hash(t, p.a) != c.hash(t, p.b) {
				t.Errorf("%s: %s: the same message read as two\n a: %s\n b: %s", c.name, p.name, p.a, p.b)
			}
		}
	}
}

func TestEveryCallerTellsTwoMessagesApart(t *testing.T) {
	for _, c := range callers {
		for _, p := range otherMessage {
			if c.hash(t, p.a) == c.hash(t, p.b) {
				t.Errorf("%s: %s: two messages read as one\n a: %s\n b: %s", c.name, p.name, p.a, p.b)
			}
		}
	}
}
