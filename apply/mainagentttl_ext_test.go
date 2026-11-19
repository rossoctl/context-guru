package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// #1243, through BodyOpts: the thread decides, the ops explain the bytes.

func mainAgentBody() []byte {
	pad := strings.Repeat("context ", 30000)
	return []byte(`{"model":"aws/claude-opus-5",` +
		`"tools":[{"name":"Bash","description":"` + pad + `","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`)
}

func isSub(id string) bool { return strings.HasPrefix(id, "a:") }

func runMainAgent(t *testing.T, o apply.Opts) apply.Result {
	t.Helper()
	p, err := pipe(t, "pipeline: []\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	o.Provider, o.Body = bschemas.Anthropic, mainAgentBody()
	o.MainAgentTTL1h, o.HeadTTLMinBytes, o.IsSubagent = true, 200000, isSub
	return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), o)
}

func withThread(id string) apply.Opts {
	return apply.Opts{ThreadOf: func(string) thread.Result { return thread.Result{ID: id, Source: thread.SourceHeader} }}
}

func allOneHour(body []byte) bool { return body != nil && apply.AllBreakpointsAskExtendedTTL(body) }

func TestMainAgentTTLMainThreadGetsItSubagentDoesNot(t *testing.T) {
	main := runMainAgent(t, withThread(thread.Primary))
	if !allOneHour(main.Body) || !main.Changed {
		t.Fatalf("main thread: not every breakpoint asks 1h (changed=%v)", main.Changed)
	}
	if !main.Trace.HeadTTL1h {
		t.Error("the trace does not say the request asked for 1h")
	}
	sub := runMainAgent(t, withThread("a:agent1"))
	if sub.Body != nil && gjson.GetBytes(sub.Body, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Error("a subagent request was relabelled")
	}
}

// The ops explain every byte the relabel added to messages (the accountant's sum check), and carry
// the "headttl:" ChangeID the library finds a held entry by.
func TestMainAgentTTLReportsTheMessageRelabelAsOperations(t *testing.T) {
	res := runMainAgent(t, withThread(thread.Primary))
	grew := len(gjson.GetBytes(res.Body, "messages").Raw) - len(gjson.GetBytes(mainAgentBody(), "messages").Raw)
	explained, n := 0, 0
	for _, o := range res.Trace.Ops {
		if o.Operator != "context-guru.headttl" {
			continue
		}
		n++
		explained += o.SizeAfter.Bytes - o.SizeBefore.Bytes
		if !strings.HasPrefix(o.ChangeID, "headttl:") {
			t.Errorf("ChangeID %q lacks the headttl: prefix", o.ChangeID)
		}
	}
	if n != 2 || explained != grew {
		t.Errorf("%d headttl ops explain %d bytes; the messages grew by %d (want 2 ops)", n, explained, grew)
	}
}

// No thread resolver: every request is on the primary thread. A host that forgot IsSubagent asks
// for nothing.
func TestMainAgentTTLWithoutAThreadResolverIsTheMainThread(t *testing.T) {
	if res := runMainAgent(t, apply.Opts{}); !allOneHour(res.Body) {
		t.Error("not relabelled")
	}
	p, _ := pipe(t, "pipeline: []\n").Build(nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: mainAgentBody(), MainAgentTTL1h: true, HeadTTLMinBytes: 200000})
	if res.Body != nil && gjson.GetBytes(res.Body, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Error("relabelled without a way to tell subagents apart")
	}
}
