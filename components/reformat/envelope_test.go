package reformat

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// Claude Code tool results arrive wrapped by the tool runner: the payload the agent
// actually reads is a JSON document escaped inside a string field. Measured on real
// traffic, 673/673 of the large low-reduction JSON blobs carry their payload in a
// `stdout` string, and 537 of those (2,098,762 tokens, 89% of the mass) hold a
// repeated-record array in there. Before the descent, `format` re-encoded only the
// envelope (9 tokens of 6,459) and `toon` reported not_uniform_object_array without
// ever looking inside.

// records builds a pretty-printed uniform record array, the shape toon exists for.
func records(n int) string {
	rows := make([]string, n)
	for i := 0; i < n; i++ {
		rows[i] = fmt.Sprintf("    {\n      \"task_id\": %d,\n      \"client_name\": \"client-%d\",\n      \"state\": \"running\"\n    }", i, i)
	}
	return "[\n" + strings.Join(rows, ",\n") + "\n  ]"
}

// envelope wraps payload in the tool-runner envelope, JSON-escaping it into stdout.
func envelope(t *testing.T, payload string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ok": true, "exit_code": 0, "stdout": payload})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func reformatTool(t *testing.T, c components.Reformat, text string) (string, *components.Report) {
	t.Helper()
	req := &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Input:    []schemas.ChatMessage{blockMsg(schemas.ChatMessageRoleTool, text)},
	}
	rep := &components.Report{}
	if err := c.Reformat(req, rep, ctx()); err != nil {
		t.Fatalf("%s Reformat: %v", c.Name(), err)
	}
	return schema.MessageText(req.Input[0]), rep
}

// The measured case: the record array sits inside an object inside the stdout string.
// toon used to decline this entirely.
func TestToonEncodesRecordArrayInsideEnvelope(t *testing.T) {
	in := envelope(t, "{\n  \"total\": 50,\n  \"tasks\": "+records(50)+"\n}")
	out, rep := reformatTool(t, &Toon{minTokens: 50}, in)
	if rep.Skipped {
		t.Fatalf("skipped the envelope; gates=%v", rep.Gates)
	}
	if !strings.Contains(out, "[50]{client_name,state,task_id}:") {
		t.Fatalf("no TOON header in output:\n%s", out)
	}
	before, after := schema.TextTokens(in), schema.TextTokens(out)
	if after >= before {
		t.Fatalf("did not shrink: %d -> %d", before, after)
	}
	t.Logf("toon envelope: %d -> %d tokens (%.1f%% saved)", before, after, 100*float64(before-after)/float64(before))

	// The agent must still be able to parse what it receives, at both levels.
	var env struct {
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope no longer parses: %v\n%s", err, out)
	}
	if !env.OK || env.ExitCode != 0 {
		t.Errorf("sibling fields corrupted: %+v", env)
	}
	var inner struct {
		Total int    `json:"total"`
		Tasks string `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(env.Stdout), &inner); err != nil {
		t.Fatalf("stdout payload no longer parses: %v\n%s", err, env.Stdout)
	}
	if inner.Total != 50 || !strings.HasPrefix(inner.Tasks, "[50]{") {
		t.Errorf("inner payload wrong: total=%d tasks=%.40q", inner.Total, inner.Tasks)
	}
}

// The simpler shape: stdout is the record array itself.
func TestToonEncodesArrayDirectlyInEnvelopeStdout(t *testing.T) {
	in := envelope(t, records(50))
	out, rep := reformatTool(t, &Toon{minTokens: 50}, in)
	if rep.Skipped {
		t.Fatalf("skipped; gates=%v", rep.Gates)
	}
	if !strings.Contains(out, "[50]{client_name,state,task_id}:") {
		t.Fatalf("no TOON header:\n%.300s", out)
	}
	if schema.TextTokens(out) >= schema.TextTokens(in) {
		t.Fatal("did not shrink")
	}
}

// format used to compact only the envelope and save ~0.1%.
func TestFormatCompactsPayloadInsideEnvelope(t *testing.T) {
	payload := "{\n  \"total\": 50,\n  \"tasks\": " + records(50) + "\n}"
	in := envelope(t, payload)
	out, rep := reformatTool(t, &Format{minTokens: 50}, in)
	if rep.Skipped {
		t.Fatalf("skipped; gates=%v", rep.Gates)
	}
	before, after := schema.TextTokens(in), schema.TextTokens(out)
	// Envelope-only compaction is worth ~0.1%; descending must beat that by a lot.
	if float64(before-after)/float64(before) < 0.2 {
		t.Fatalf("only %d -> %d tokens: the payload was not compacted", before, after)
	}
	t.Logf("format envelope: %d -> %d tokens (%.1f%% saved)", before, after, 100*float64(before-after)/float64(before))

	// Lossless: the payload must decode to the same value it arrived as.
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope no longer parses: %v", err)
	}
	var got string
	if err := json.Unmarshal(env["stdout"], &got); err != nil {
		t.Fatalf("stdout not a string: %v", err)
	}
	var gotV, wantV any
	if err := json.Unmarshal([]byte(got), &gotV); err != nil {
		t.Fatalf("compacted payload does not parse: %v", err)
	}
	if err := json.Unmarshal([]byte(payload), &wantV); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		t.Error("compacted payload is not the value that arrived")
	}
}

// Sibling fields must round-trip byte-exact, including the awkward ones: a big integer
// (a float64 round-trip would rewrite it), HTML characters (default escaping would
// grow them), and a long string that is NOT JSON.
func TestEnvelopeDescentPreservesSiblingFields(t *testing.T) {
	siblings := map[string]json.RawMessage{
		"exit_code": json.RawMessage(`0`),
		"ok":        json.RawMessage(`true`),
		"id":        json.RawMessage(`12345678901234567890`),
		"cmd":       json.RawMessage(`"a < b && c > d"`),
		"stderr":    json.RawMessage(`"` + strings.Repeat("warning: not json at all ", 8) + `"`),
	}
	in := map[string]json.RawMessage{}
	for k, v := range siblings {
		in[k] = v
	}
	inner, err := json.Marshal("{\n  \"tasks\": " + records(50) + "\n}")
	if err != nil {
		t.Fatal(err)
	}
	in["stdout"] = inner
	// marshalJSON, not json.Marshal: the input must contain a literal `<`, so that a
	// component quietly re-escaping it shows up as a byte difference.
	raw, err := marshalJSON(in)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []components.Reformat{&Format{minTokens: 50}, &Toon{minTokens: 50}} {
		out, rep := reformatTool(t, c, string(raw))
		if rep.Skipped {
			t.Fatalf("%s: skipped; gates=%v", c.Name(), rep.Gates)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%s: result does not parse: %v", c.Name(), err)
		}
		if len(got) != len(in) {
			t.Errorf("%s: field count changed: %d -> %d", c.Name(), len(in), len(got))
		}
		for k, want := range siblings {
			if string(got[k]) != string(want) {
				t.Errorf("%s: field %q not byte-exact: %s != %s", c.Name(), k, got[k], want)
			}
		}
	}
}

// A long string field that is not JSON must not be parsed, rewritten, or counted as a
// descent — and with nothing else to do, both components leave the blob alone.
func TestEnvelopeDescentSkipsNonJSONString(t *testing.T) {
	in := `{"ok":true,"exit_code":1,"stdout":"` + strings.Repeat("plain log line, no json here. ", 20) + `"}`
	for _, c := range []components.Reformat{&Format{minTokens: 50}, &Toon{minTokens: 50}} {
		out, rep := reformatTool(t, c, in)
		if out != in {
			t.Errorf("%s: rewrote a non-JSON payload:\n%s", c.Name(), out)
		}
		if !rep.Skipped {
			t.Errorf("%s: acted on a non-JSON payload", c.Name())
		}
		if rep.Gates["envelope_no_embedded_json"] == 0 && rep.Gates["not_uniform_object_array"] == 0 {
			t.Errorf("%s: declined without an honest gate reason: %v", c.Name(), rep.Gates)
		}
	}

	// The cheap gate must also refuse a short JSON-shaped string: nothing to win.
	if _, ok := embeddedJSON(json.RawMessage(`"{\"a\":1}"`)); ok {
		t.Error("attempted a parse below the size floor")
	}
}

// If the inner transform does not shrink the payload, the whole blob must come back
// untouched — a component must not lean on the pipeline's never-worse guard.
func TestEnvelopeDescentLeavesBlobUnchangedWhenInnerDoesNotShrink(t *testing.T) {
	// Already-compact payload, and a table toon must refuse (numeric strings would
	// collapse onto number cells), so neither component can find a win.
	payload := `{"rows":[` + strings.Repeat(`{"id":"1","n":"2"},`, 40) + `{"id":"1","n":"2"}]}`
	in := envelope(t, payload)
	for _, c := range []components.Reformat{&Format{minTokens: 50}, &Toon{minTokens: 50}} {
		out, rep := reformatTool(t, c, in)
		if out != in {
			t.Errorf("%s: rewrote a blob it could not shrink:\nbefore %s\nafter  %s", c.Name(), in, out)
		}
		if !rep.Skipped {
			t.Errorf("%s: acted without shrinking", c.Name())
		}
		if len(rep.Gates) == 0 {
			t.Errorf("%s: declined with no gate reason", c.Name())
		}
	}
}

// The pathological input: escaped JSON nested many levels deep. Descent is bounded, so
// only the outermost payload is ever opened — no unbounded walker.
func TestEnvelopeDescentIsBounded(t *testing.T) {
	deep := `{"tasks":` + records(20) + `}`
	for i := 0; i < 6; i++ {
		b, err := json.Marshal(deep)
		if err != nil {
			t.Fatal(err)
		}
		deep = `{"ok":true,"stdout":` + string(b) + `}`
	}
	out, _ := reformatTool(t, &Toon{minTokens: 50}, deep)
	if strings.Contains(out, "[20]{") {
		t.Error("reached the innermost payload: descent is not bounded")
	}
}
