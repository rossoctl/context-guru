package reformat

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
)

func TestCompactToolSchemas(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // expected `tools` array after the strip; "" = body unchanged
	}{{
		name: "claude code shape: $schema is the only annotation on real traffic",
		body: `{"messages":[],"tools":[{"name":"Read","description":"Reads a file.","input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"file_path":{"type":"string","description":"absolute path"}},"required":["file_path"],"additionalProperties":false}}]}`,
		want: `[{"description":"Reads a file.","input_schema":{"additionalProperties":false,"properties":{"file_path":{"description":"absolute path","type":"string"}},"required":["file_path"],"type":"object"},"name":"Read"}]`,
	}, {
		name: "every annotation keyword, at the root and nested",
		body: `{"tools":[{"name":"T","input_schema":{"$id":"urn:x","$schema":"s","$comment":"c","title":"Root","description":"keep me","deprecated":false,"examples":[1],"example":1,"markdownDescription":"**m**","readOnly":true,"writeOnly":false,"type":"object","properties":{"a":{"type":"string","title":"A","readOnly":true,"description":"keep"}}}}]}`,
		want: `[{"input_schema":{"description":"keep me","properties":{"a":{"description":"keep","type":"string"}},"type":"object"},"name":"T"}]`,
	}, {
		// THE TRAP. `title`, `examples` and `readOnly` here are PROPERTY NAMES, not
		// keywords. Stripping them silently deletes real parameters of the tool.
		name: "properties named title/examples/readOnly survive",
		body: `{"tools":[{"name":"CreateIssue","input_schema":{"type":"object","title":"drop this one","properties":{"title":{"type":"string","title":"drop this one too","description":"the issue title"},"examples":{"type":"array","items":{"type":"string","title":"nope"}},"readOnly":{"type":"boolean","default":false},"writeOnly":{"type":"boolean"},"$schema":{"type":"string","description":"a property genuinely called $schema"}},"required":["title","examples","readOnly"]}}]}`,
		want: `[{"input_schema":{"properties":{"$schema":{"description":"a property genuinely called $schema","type":"string"},"examples":{"items":{"type":"string"},"type":"array"},"readOnly":{"default":false,"type":"boolean"},"title":{"description":"the issue title","type":"string"},"writeOnly":{"type":"boolean"}},"required":["title","examples","readOnly"],"type":"object"},"name":"CreateIssue"}]`,
	}, {
		// const/default/enum hold INSTANCE DATA. A `title` inside them is a value.
		name: "instance data under const/default/enum is never touched",
		body: `{"tools":[{"name":"T","input_schema":{"type":"object","title":"gone","properties":{"page":{"type":"object","default":{"title":"Untitled","readOnly":true},"const":{"title":"fixed"},"enum":[{"title":"a"},{"examples":["b"]}]}}}}]}`,
		want: `[{"input_schema":{"properties":{"page":{"const":{"title":"fixed"},"default":{"readOnly":true,"title":"Untitled"},"enum":[{"title":"a"},{"examples":["b"]}],"type":"object"}},"type":"object"},"name":"T"}]`,
	}, {
		name: "recurses through anyOf/allOf/items/propertyNames/$defs",
		body: `{"tools":[{"name":"T","input_schema":{"$defs":{"leaf":{"type":"string","title":"gone"}},"anyOf":[{"title":"gone","type":"object"}],"allOf":[{"readOnly":true,"type":"object"}],"items":{"title":"gone","type":"string"},"propertyNames":{"title":"gone","pattern":"^x"},"additionalProperties":{"title":"gone","type":"string"}}}]}`,
		want: `[{"input_schema":{"$defs":{"leaf":{"type":"string"}},"additionalProperties":{"type":"string"},"allOf":[{"type":"object"}],"anyOf":[{"type":"object"}],"items":{"type":"string"},"propertyNames":{"pattern":"^x"}},"name":"T"}]`,
	}, {
		name: "draft-07 tuple form of items",
		body: `{"tools":[{"name":"T","input_schema":{"type":"array","items":[{"type":"string","title":"gone"},{"type":"number","examples":[1]}]}}]}`,
		want: `[{"input_schema":{"items":[{"type":"string"},{"type":"number"}],"type":"array"},"name":"T"}]`,
	}, {
		// $id/$schema are base-URI and dialect declarations, not annotations: a schema
		// that RESOLVES A REFERENCE may need them. Keep them there, strip the rest.
		name: "$id and $schema kept when the schema uses $ref",
		body: `{"tools":[{"name":"T","input_schema":{"$id":"urn:t","$schema":"https://json-schema.org/draft/2020-12/schema","title":"gone","$defs":{"n":{"type":"string","title":"gone"}},"properties":{"a":{"$ref":"#/$defs/n"}},"type":"object"}}]}`,
		want: `[{"input_schema":{"$defs":{"n":{"type":"string"}},"$id":"urn:t","$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"a":{"$ref":"#/$defs/n"}},"type":"object"},"name":"T"}]`,
	}, {
		name: "openai function shape",
		body: `{"tools":[{"type":"function","function":{"name":"T","parameters":{"type":"object","title":"gone","properties":{"a":{"type":"string","title":"gone"}}}}}]}`,
		want: `[{"function":{"name":"T","parameters":{"properties":{"a":{"type":"string"}},"type":"object"}},"type":"function"}]`,
	}, {
		name: "schema-less anthropic server tool is left alone",
		body: `{"tools":[{"type":"bash_20250124","name":"bash"}]}`,
		want: ``,
	}, {
		name: "nothing to strip: body untouched byte for byte",
		body: `{"tools":[{"name":"T","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`,
		want: ``,
	}, {
		name: "no tools array",
		body: `{"messages":[{"role":"user","content":"hi"}]}`,
		want: ``,
	}, {
		name: "empty tools array",
		body: `{"tools":[]}`,
		want: ``,
	}, {
		name: "unparseable tools: fail open",
		body: `{"tools":[{"name":}]}`,
		want: ``,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toolCache.Clear()
			out, changed := CompactToolSchemas([]byte(tc.body))
			if tc.want == "" {
				if changed {
					t.Fatalf("expected no change, got %s", out)
				}
				if string(out) != tc.body {
					t.Fatalf("body mutated on a no-op:\n got %s\nwant %s", out, tc.body)
				}
				return
			}
			if !changed {
				t.Fatalf("expected a change, got none")
			}
			got := gjson.GetBytes(out, "tools").Raw
			if got != tc.want {
				t.Errorf("tools mismatch:\n got %s\nwant %s", got, tc.want)
			}
			// Every field outside `tools` must survive verbatim.
			for _, k := range []string{"messages", "model"} {
				if a, b := gjson.Get(tc.body, k).Raw, gjson.GetBytes(out, k).Raw; a != b {
					t.Errorf("field %q changed: %q -> %q", k, a, b)
				}
			}
		})
	}
}

// TestCompactToolSchemasByteStable is the cache-safety test. A transformation that
// is not byte-identical for identical input re-anchors the prompt-cache prefix on
// EVERY request and is a pure loss (see the CompactToolSchemas doc). Go map ranging
// is randomized, so a rebuild that ranged instead of sorting would fail this.
func TestCompactToolSchemasByteStable(t *testing.T) {
	body := []byte(`{"tools":[{"name":"B","input_schema":{"$schema":"s","title":"x","type":"object","properties":{"z":{"type":"string","title":"a"},"y":{"type":"number","readOnly":true},"title":{"type":"string"}},"required":["y","z"]}},{"name":"A","input_schema":{"$comment":"c","type":"object","properties":{"q":{"anyOf":[{"type":"string","title":"t"},{"type":"null"}]}}}}]}`)

	// Repeated calls, cache cleared each time so the walk actually re-runs.
	first, _ := CompactToolSchemas(body)
	for i := 0; i < 200; i++ {
		toolCache.Clear()
		got, _ := CompactToolSchemas(body)
		if string(got) != string(first) {
			t.Fatalf("iteration %d differs:\n got %s\nwant %s", i, got, first)
		}
	}
	// And with the cache warm, which is the path production takes.
	for i := 0; i < 200; i++ {
		got, _ := CompactToolSchemas(body)
		if string(got) != string(first) {
			t.Fatalf("cached iteration %d differs", i)
		}
	}

	// Across PROCESSES: map iteration order and the maphash seed are re-randomized
	// per process, so same-process repetition cannot prove this on its own.
	if os.Getenv("CG_TOOLSCHEMA_CHILD") == "1" {
		out, _ := CompactToolSchemas(body)
		os.Stdout.WriteString(string(out) + "\n")
		return
	}
	for i := 0; i < 5; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestCompactToolSchemasByteStable$", "-test.v=false")
		cmd.Env = append(os.Environ(), "CG_TOOLSCHEMA_CHILD=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child run %d: %v", i, err)
		}
		// The child prints the body then testing prints PASS; take the JSON prefix.
		got := string(out)
		if i := strings.IndexByte(got, '\n'); i >= 0 {
			got = got[:i] // the child prints the body then testing prints PASS
		}
		if got != string(first) {
			t.Fatalf("child process %d produced different bytes:\n got %s\nwant %s", i, got, first)
		}
	}
}

// TestCompactToolSchemasValidInputsUnchanged is the losslessness claim as an
// executable check rather than a citation: for a schema built out of every keyword
// that DOES constrain instances, the stripped form must accept and reject exactly
// the same inputs. Checked structurally — the constraint keywords must survive
// byte-identically — because the repo carries no JSON Schema validator and adding a
// dependency to assert what the spec already states would be the expensive way.
func TestCompactToolSchemasValidInputsUnchanged(t *testing.T) {
	const constraining = `{"type":"object","properties":{"a":{"type":"string","minLength":1,"maxLength":9,"pattern":"^x","format":"uri","enum":["x1","x2"]},"b":{"type":"integer","minimum":0,"maximum":10,"exclusiveMinimum":0,"multipleOf":2},"c":{"type":"array","items":{"const":3},"minItems":1,"maxItems":4,"uniqueItems":true},"d":{"oneOf":[{"type":"null"},{"type":"boolean"}]}},"required":["a","b"],"additionalProperties":false,"propertyNames":{"pattern":"^[a-d]$"},"dependentRequired":{"a":["b"]},"default":{"a":"x1"}}`
	body := []byte(`{"tools":[{"name":"T","input_schema":` + constraining + `}]}`)
	// Add annotations around it; the constraining core must come out identical.
	annotated := []byte(strings.Replace(string(body), `{"type":"object","properties"`,
		`{"title":"T","$comment":"c","readOnly":false,"examples":[{}],"type":"object","properties"`, 1))

	if _, changed := CompactToolSchemas(body); changed {
		t.Error("a schema of pure constraints has nothing to strip, yet it was rewritten")
	}
	toolCache.Clear()
	got, changed := CompactToolSchemas(annotated)
	if !changed {
		t.Fatal("annotated body should have changed")
	}
	// Both sides through the same encoder, so only CONTENT differences can show up
	// (the rewrite normalizes key order; JSON object order is not semantic).
	want, ok := stripToolAnnotations([]byte(gjson.GetBytes(body, "tools").Raw))
	if !ok {
		t.Fatal("could not canonicalize the control schema")
	}
	if a, b := string(want), gjson.GetBytes(got, "tools").Raw; a != b {
		t.Errorf("annotations changed the constraining core:\n got %s\nwant %s", b, a)
	}
}

// TestCompactToolSchemasRealCaptures measures the component on captured Claude Code
// request bodies with the repo's own tokenizer, and asserts the two things a
// regression would break: nothing is dropped from the CONSTRAINT vocabulary, and the
// tool count is unchanged. The numbers it prints are the ones in
// docs/components/toolschema.md.
func TestCompactToolSchemasRealCaptures(t *testing.T) {
	for _, path := range captureCorpus(t) {
		t.Run(strings.NewReplacer("/", "_").Replace(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Skipf("no capture at %s", path)
			}
			defer f.Close()
			dec := json.NewDecoder(f)
			var reqs, toolsSeen, savedTot, beforeTot int
			// Capped: BPE over every request of a 1,795-line capture is minutes of CPU
			// for a figure that has already converged (the tools array is byte-identical
			// across a session's turns, so every request after the first re-measures the
			// same schemas).
			const maxReqs = 25
			for dec.More() && reqs < maxReqs {
				var line struct{ Body json.RawMessage }
				if err := dec.Decode(&line); err != nil {
					break
				}
				body := []byte(line.Body)
				pre := gjson.GetBytes(body, "tools")
				if !pre.Exists() || len(pre.Array()) == 0 {
					continue
				}
				out, changed := CompactToolSchemas(body)
				if !changed {
					continue
				}
				post := gjson.GetBytes(out, "tools")
				if a, b := len(pre.Array()), len(post.Array()); a != b {
					t.Fatalf("tool count changed %d -> %d", a, b)
				}
				for i, tool := range pre.Array() {
					name := tool.Get("name").String()
					got := post.Array()[i].Get("name").String()
					if name != got {
						t.Fatalf("tool %d renamed %q -> %q", i, name, got)
					}
					// Property NAMES are the trap; assert the full set survives.
					want := propNames(tool.Get("input_schema"))
					have := propNames(post.Array()[i].Get("input_schema"))
					for _, p := range want {
						if !contains(have, p) {
							t.Fatalf("tool %q lost property %q", name, p)
						}
					}
				}
				reqs++
				toolsSeen += len(pre.Array())
				beforeTot += tokens.Count(string(body))
				savedTot += tokens.Count(pre.Raw) - tokens.Count(post.Raw)
			}
			if reqs == 0 {
				t.Skip("no tool-bearing requests in this capture")
			}
			t.Logf("%s: %d requests, %d tools/request, %d tokens saved/request "+
				"(%.3f%% of %d input tokens), break-even on a warm prefix at %d requests",
				path, reqs, toolsSeen/reqs, savedTot/reqs,
				100*float64(savedTot/reqs)/float64(beforeTot/reqs), beforeTot/reqs,
				breakEven(beforeTot/reqs, savedTot/reqs))
		})
	}
}

// breakEven is the arithmetic from the CompactToolSchemas doc: how many requests a
// WARM session needs before the one-time re-anchor pays for itself.
// n = ((W−R)·P − W·s) / (R·s), with W = 1.25 (5m cache write), R = 0.1 (cache read).
func breakEven(prefix, saved int) int {
	if saved <= 0 {
		return -1
	}
	return int(((1.25-0.1)*float64(prefix) - 1.25*float64(saved)) / (0.1 * float64(saved)))
}

func captureCorpus(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range []string{
		"/home/vpcuser/cg-research/bench/long.jsonl",
		"/home/vpcuser/cg-research/bench/mixed.jsonl",
		"/home/vpcuser/cg-research/bench/short.jsonl",
		"/home/vpcuser/cg-research/bench/cold.jsonl",
		"/tmp/cg-runs/capture-swebench.jsonl",
	} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skip("no captures on this machine")
	}
	return out
}

func propNames(schema gjson.Result) []string {
	var out []string
	schema.Get("properties").ForEach(func(k, v gjson.Result) bool {
		out = append(out, k.String())
		out = append(out, propNames(v)...)
		return true
	})
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
