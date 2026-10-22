package record

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// sse builds an Anthropic event stream whose single text block arrives as the given deltas.
func sse(deltas ...string) string {
	var b strings.Builder
	b.WriteString("event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[]}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	for _, d := range deltas {
		b.WriteString("event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonString(d) + `}}` + "\n\n")
	}
	b.WriteString("event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

func jsonString(s string) string { return encodeLiteral(s) }

// THE PROBLEM. A model can print a secret it read from a tool result, and a stream sends it in
// pieces. Redacting each event on its own sees half a key in each, matches nothing, and keeps
// both halves -- so the raw stream on disk still carries the whole key.
func TestPerEventRedactionMissesASplitSecret(t *testing.T) {
	key := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB"
	a, b := key[:5], key[5:] // "sk-an" | "t-api03-...": neither half looks like a key on its own
	raw := sse("the key is "+a, b+" ok")

	// What redacting each event on its own does: both halves survive, so the whole key is on
	// disk in two pieces. This is the leak RedactSSE exists to stop.
	events, _ := splitSSE(raw)
	var perEvent strings.Builder
	for _, e := range events {
		perEvent.WriteString(ContentJSON(e.json))
	}
	if !strings.Contains(perEvent.String(), a) || !strings.Contains(perEvent.String(), b) {
		t.Fatal("per-event redaction caught a half key on its own; pick a split that shows the problem")
	}
	if got := RedactSSE(raw); strings.Contains(got, b) {
		t.Errorf("RedactSSE kept part of a secret split across two events:\n%s", got)
	}
}

// The stream must stay a valid stream with the same events, so a replay tool can still read it,
// and redacted text must still assemble to the redacted message.
func TestRedactSSEKeepsTheStreamShape(t *testing.T) {
	key := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB"
	raw := sse("the key is "+key[:5], key[5:]+" ok")
	got := RedactSSE(raw)
	if strings.Count(got, "event: content_block_delta") != 2 {
		t.Errorf("event count changed:\n%s", got)
	}
	agg, ok := aggregateTextForTest(got)
	if !ok {
		t.Fatalf("redacted stream no longer aggregates:\n%s", got)
	}
	if want := "the key is " + Placeholder + " ok"; agg != want {
		t.Errorf("assembled text = %q, want %q", agg, want)
	}
}

// A clean stream comes back byte for byte.
func TestRedactSSELeavesACleanStreamAlone(t *testing.T) {
	raw := sse("Hello ", "there")
	if got := RedactSSE(raw); got != raw {
		t.Errorf("a stream with nothing to redact was changed:\n got: %q\nwant: %q", got, raw)
	}
}

// Identity is removed across events too: a name can be split like a key.
func TestRedactSSEAlsoRemovesASplitName(t *testing.T) {
	raw := sse("thanks Da", "na Levi")
	got := RedactSSE(raw)
	agg, _ := aggregateTextForTest(got)
	if strings.Contains(agg, "Levi") || strings.Contains(got, "Levi") {
		t.Errorf("a name split across events survived: %q", agg)
	}
}

// Tool-call arguments stream as partial_json pieces; a secret there is split the same way.
func TestRedactSSEToolArguments(t *testing.T) {
	key := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl"
	raw := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + jsonString(`{"command":"export GITHUB_TOKEN=`+key[:15]) + `}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + jsonString(key[15:]+`"}`) + `}}` + "\n\n" +
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n"
	if got := RedactSSE(raw); strings.Contains(got, key[:15]) || strings.Contains(got, key[15:]) {
		t.Errorf("a token split across tool-argument pieces survived:\n%s", got)
	}
}

// Every way of cutting a text into two events, and many into three: the deltas must always join
// to exactly what redacting the whole text gives, and no event may hold more than a scrap of the
// secret. This is the property the whole file rests on, so it is checked exhaustively rather
// than on a few hand-picked splits.
func TestRedactSSEAnySplit(t *testing.T) {
	key := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-BBBB"
	text := "token " + key + " and thanks Dana Levi, bye"
	want := Content(text)
	check := func(cuts ...int) {
		t.Helper()
		var parts []string
		prev := 0
		for _, c := range cuts {
			parts = append(parts, text[prev:c])
			prev = c
		}
		parts = append(parts, text[prev:])
		got := RedactSSE(sse(parts...))
		joined, _ := aggregateTextForTest(got)
		if joined != want {
			t.Fatalf("cuts %v: deltas join to %q, want %q", cuts, joined, want)
		}
		events, _ := splitSSE(got)
		for _, e := range events {
			piece := gjson.Get(e.json, "delta.text").String()
			for n := 12; n <= len(key); n++ { // no 12-character run of the key in any event
				for i := 0; i+n <= len(key); i += n {
					if strings.Contains(piece, key[i:i+n]) {
						t.Fatalf("cuts %v: an event holds %q of the key: %q", cuts, key[i:i+n], piece)
					}
				}
			}
			if strings.Contains(piece, "Levi") {
				t.Fatalf("cuts %v: an event holds the surname: %q", cuts, piece)
			}
		}
	}
	for c := 1; c < len(text); c++ {
		check(c)
	}
	for c1 := 1; c1 < len(text); c1 += 3 {
		for c2 := c1 + 1; c2 < len(text); c2 += 5 {
			check(c1, c2)
		}
	}
}
