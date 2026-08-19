package apply_test

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// The tool-schema strip is body-level and gated on the `toolschema` name, so the only
// thing that can go wrong in the wiring is the gate and the ordering: it must rewrite
// `tools` and leave `messages` byte-identical, and it must run BEFORE the writeback
// takes byte offsets into the body (`tools` can be serialized either side of
// `messages`, and on captured Claude Code traffic it comes after).
func TestToolSchemaStripThroughApply(t *testing.T) {
	const body = `{"model":"claude-opus-5","tools":[{"name":"Read","input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","title":"Read","properties":{"title":{"type":"string","title":"drop"}}}}],"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"{\n  \"a\": 1,\n  \"b\": 2\n}"}]}]}`

	for _, tc := range []struct {
		name, pipeline string
		want           bool
	}{
		{"gated off without the name", "pipeline: [format]\n", false},
		{"fires with the name", "pipeline: [format, toolschema]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := pipe(t, tc.pipeline).Build(nil)
			out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
				bschemas.Anthropic, []byte(body), "s1", false)

			schema := gjson.GetBytes(out, "tools.0.input_schema")
			if got := schema.Get("$schema").Exists() || schema.Get("title").Exists(); got != !tc.want {
				t.Errorf("annotations present = %v, want %v", got, !tc.want)
			}
			// The property literally named `title` must survive either way.
			if !schema.Get("properties.title.type").Exists() {
				t.Error("the property named `title` was deleted")
			}
			// Whatever else the pipeline did, the message array must still parse and keep
			// its shape — this is where a mis-ordered rewrite corrupts the body.
			msgs := gjson.GetBytes(out, "messages")
			if !msgs.IsArray() || len(msgs.Array()) != 2 {
				t.Fatalf("messages corrupted: %s", msgs.Raw)
			}
			if got := msgs.Array()[1].Get("content.0.tool_use_id").String(); got != "t1" {
				t.Errorf("tool_result block corrupted: tool_use_id = %q", got)
			}
		})
	}
}
