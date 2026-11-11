package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
)

// cacheinjectBody is a Claude Code shaped request with n tool turns. The client sends two marks of its
// own, one on the system block and one on the newest message, as Claude Code does.
func cacheinjectBody(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"system prompt","cache_control":{"type":"ephemeral"}}],"messages":[`)
	b.WriteString(`{"role":"user","content":[{"type":"text","text":"go"}]}`)
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` +
			strings.Repeat("output line number "+id+"\\n", 30) + `"}`)
		if i == n-1 {
			b.WriteString(`,{"type":"text","text":"next","cache_control":{"type":"ephemeral"}}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// #938. cacheinject adds cache_control marks to messages the client did not mark. The marks lengthen the
// forwarded body, and the accountant's sum check compares the bytes the reported operations explain with the
// bytes that went upstream. A mark nobody reports leaves ops=0 for bodies=+37 per mark, the check fails and the
// event books no saving.
func TestCacheinjectMarksAreExplainedByReportedOperations(t *testing.T) {
	for _, turns := range []int{2, 3, 4} {
		body := cacheinjectBody(turns)
		p, err := pipe(t, "pipeline: [cacheinject]\n").Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
			Provider: bschemas.Anthropic, Body: body,
		})
		if !res.Changed {
			t.Fatalf("%d turns: cacheinject changed nothing; the test body has no mark to add", turns)
		}
		grew := len(res.Body) - len(body)
		if grew <= 0 {
			t.Fatalf("%d turns: forwarded body grew by %d bytes, want marks added", turns, grew)
		}
		if res.Trace.OpsDropped != 0 {
			t.Fatalf("%d turns: %d operations dropped", turns, res.Trace.OpsDropped)
		}
		explained := 0
		for _, o := range res.Trace.Ops {
			if o.DryRun {
				continue
			}
			explained += o.SizeAfter.Bytes - o.SizeBefore.Bytes
		}
		if explained != grew {
			t.Errorf("%d turns: operations explain %d bytes, the forwarded body grew by %d (ops %d)",
				turns, explained, grew, len(res.Trace.Ops))
		}
		// Each reported operation must sit on a message that carries a new mark.
		for _, o := range res.Trace.Ops {
			if gjson.GetBytes(res.Body, "messages."+itoa(o.At.Index)+".content.#(cache_control)").Exists() == false &&
				!strings.Contains(gjson.GetBytes(res.Body, "messages."+itoa(o.At.Index)).Raw, "cache_control") {
				t.Errorf("%d turns: op %+v is on a message with no mark", turns, o)
			}
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}

// #938 on the message #32 names as the real target: an assistant message with a tool_use block, which
// bifrost cannot round-trip. The mark is written at its path on the original bytes and must be reported,
// and the provider fields of the block must still come through unchanged.
func TestCacheinjectMarkOnAToolUseMessageIsReported(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[
		{"role":"user","content":"run the tool"},
		{"role":"assistant","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"ls -la"}}]}
	]}`)
	p, err := pipe(t, "pipeline: [cacheinject]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body,
	})
	if !res.Changed || !gjson.GetBytes(res.Body, "messages.1.content.1.cache_control").Exists() {
		t.Fatalf("precondition: the mark must reach the wire: %s", res.Body)
	}
	if res.Trace.OpsDropped != 0 || len(res.Trace.Ops) != 1 {
		t.Fatalf("ops = %+v dropped = %d, want the one mark reported", res.Trace.Ops, res.Trace.OpsDropped)
	}
	op := res.Trace.Ops[0]
	if op.At.Index != 1 || op.SizeAfter.Bytes-op.SizeBefore.Bytes != len(res.Body)-len(body) {
		t.Fatalf("op %+v, body grew by %d bytes", op, len(res.Body)-len(body))
	}
	if blk := gjson.GetBytes(res.Body, "messages.1.content.1"); blk.Get("id").String() != "toolu_abc" ||
		blk.Get("input.command").String() != "ls -la" {
		t.Fatalf("tool_use provider fields changed: %s", blk.Raw)
	}
}
