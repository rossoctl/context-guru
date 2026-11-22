package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
	"github.com/tidwall/gjson"
)

// #1261, through BodyOpts: the host decides per thread, the relabel reaches every breakpoint, and the operations
// explain every byte it added to messages.

func firstBody() []byte {
	return []byte(`{"model":"aws/claude-opus-5",` +
		`"tools":[{"name":"Bash","description":"` + strings.Repeat("x", 100) + `","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`)
}

type hookCall struct {
	session, thread, model string
	tools                  bool
}

func runFirst(t *testing.T, body []byte, threadID string, answer bool, seen *[]hookCall) apply.Result {
	t.Helper()
	p, err := pipe(t, "pipeline: []\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "sess-x",
		ThreadOf: func(string) thread.Result { return thread.Result{ID: threadID, Source: thread.SourceHeader} },
		OneHourFirst: func(session, th, model string, tools bool) bool {
			if seen != nil {
				*seen = append(*seen, hookCall{session, th, model, tools})
			}
			return answer
		},
	})
}

func TestOneHourFirstRelabelsEveryBreakpointWhenTheHostSaysSo(t *testing.T) {
	var seen []hookCall
	res := runFirst(t, firstBody(), thread.Primary, true, &seen)
	if res.Body == nil || !apply.AllBreakpointsAskExtendedTTL(res.Body) || !res.Changed {
		t.Fatalf("not every breakpoint asks 1h (changed=%v)", res.Changed)
	}
	for _, path := range []string{"tools.0.cache_control.ttl", "system.0.cache_control.ttl",
		"messages.0.content.0.cache_control.ttl", "messages.2.content.0.cache_control.ttl"} {
		if got := gjson.GetBytes(res.Body, path).String(); got != "1h" {
			t.Errorf("%s = %q, want 1h", path, got)
		}
	}
	if before, after := apply.CountBreakpoints(firstBody()), apply.CountBreakpoints(res.Body); before != after {
		t.Errorf("breakpoints changed: %+v -> %+v", before, after)
	}
	if !res.Trace.HeadTTL1h {
		t.Error("the trace does not say the request asked for 1h")
	}
	// The hook gets the resolved session, the thread, and whether the body declares tools.
	if len(seen) != 1 || seen[0].thread != "" || !seen[0].tools || seen[0].session == "" || seen[0].model != "aws/claude-opus-5" {
		t.Errorf("hook calls = %+v, want one: a session, the primary thread, the final body's model, tools declared", seen)
	}
}

func TestOneHourFirstLeavesARequestAloneWhenTheHostSaysNo(t *testing.T) {
	res := runFirst(t, firstBody(), "a:agent1", false, nil)
	if res.Body != nil && gjson.GetBytes(res.Body, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Error("a request the host declined was relabelled")
	}
}

func TestOneHourFirstSaysWhenNoToolsAreDeclared(t *testing.T) {
	var seen []hookCall
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"title?","cache_control":{"type":"ephemeral"}}]}]}`)
	runFirst(t, body, thread.Primary, false, &seen)
	if len(seen) != 1 || seen[0].tools {
		t.Errorf("hook calls = %+v, want tools=false", seen)
	}
}

// The operations explain every byte the relabel added to messages (the accountant's sum check), with the
// headttl: ChangeID the library finds a held entry by.
func TestOneHourFirstReportsTheMessageRelabelAsOperations(t *testing.T) {
	res := runFirst(t, firstBody(), thread.Primary, true, nil)
	grew := len(gjson.GetBytes(res.Body, "messages").Raw) - len(gjson.GetBytes(firstBody(), "messages").Raw)
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

// No host hook: nothing changes. The default for every account.
func TestOneHourFirstIsOffWithoutAHook(t *testing.T) {
	p, _ := pipe(t, "pipeline: []\n").Build(nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{Provider: bschemas.Anthropic, Body: firstBody()})
	if res.Body != nil && gjson.GetBytes(res.Body, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Error("a request was relabelled with no hook")
	}
}

// The relabel runs AFTER the tail mark, so the mark markTail ADDS leaves at 1 hour too, whatever value markTail
// gave it. This test runs the tail mark for real: a pipeline with a component turns it on, the client marked an
// early message, and more than tailMinBytes (4096) of content follows that mark. The mark is ADDED (the count
// grows), and every breakpoint, the added one included, asks 1 hour.
//
// What it pins, and what it does not: markTail copies the client's last mark, so the added mark would be 1 hour
// in either defer order today. The test fails when the relabel runs BEFORE the tail mark AND markTail stops
// copying the mark's value (checked by mutation: both together fail it, the new order with that mutation passes).
func TestOneHourFirstRelabelsTheTailMarkToo(t *testing.T) {
	filler := strings.Repeat("x", 6000)
	body := []byte(`{"model":"aws/claude-opus-5","tools":[{"name":"Bash","input_schema":{"type":"object"}}],` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + filler + `"}]},` +
		`{"role":"user","content":[{"type":"text","text":"c"}]}]}`)
	p, err := pipe(t, "pipeline: [dedup]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "sess-x",
		ThreadOf:     func(string) thread.Result { return thread.Result{ID: thread.Primary, Source: thread.SourceHeader} },
		OneHourFirst: func(_, _, _ string, _ bool) bool { return true },
	})
	if res.Body == nil {
		t.Fatal("no body")
	}
	before, after := apply.CountBreakpoints(body), apply.CountBreakpoints(res.Body)
	if after.Total() <= before.Total() {
		t.Fatalf("the tail mark was not added (breakpoints %d -> %d): the test does not exercise it", before.Total(), after.Total())
	}
	if !apply.AllBreakpointsAskExtendedTTL(res.Body) {
		t.Fatalf("a breakpoint is left below 1 hour, the added tail mark included:\n%s", res.Body)
	}
}
