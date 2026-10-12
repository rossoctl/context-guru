package apply

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/store"
)

type threadSpy struct{ seen *[]string }

func (threadSpy) Name() string                 { return "threadspy" }
func (threadSpy) Enabled(*components.Ctx) bool { return true }
func (s threadSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = append(*s.seen, c.Thread)
	return nil
}

// Opts.ThreadOf runs once, with the resolved session id, BEFORE the pipeline: components see the
// thread on Ctx.Thread (cache_aware_summarizer keys its keep-alive candidate by it, #423), and the
// result rides on the trace. Both wire envelopes.
func TestThreadOfRunsBeforeThePipelineOnBothEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name, api string
		body      string
	}{
		{"messages", "", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"fix it"}]}`},
		{"responses", "responses", `{"model":"gpt-5","input":[{"role":"user","content":"fix it"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			pipe := components.NewPipeline([]components.Component{threadSpy{&seen}}, nil)
			var calls int
			var gotSession string
			res := BodyOpts(context.Background(), pipe, store.NewMemory(store.Options{}), Opts{
				Provider: bschemas.Anthropic, API: tc.api, Body: []byte(tc.body), Session: "sess-1",
				ThreadOf: func(s string) thread.Result {
					calls++
					gotSession = s
					return thread.Result{ID: "a:S", Source: thread.SourceHeader}
				},
			})
			if calls != 1 || gotSession != res.Session || res.Session == "" {
				t.Fatalf("ThreadOf called %d times with %q; trace session %q", calls, gotSession, res.Session)
			}
			if !res.ThreadResolved || res.Thread.ID != "a:S" {
				t.Errorf("trace thread = %+v (resolved %v)", res.Thread, res.ThreadResolved)
			}
			if len(seen) != 1 || seen[0] != "a:S" {
				t.Errorf("the pipeline saw Ctx.Thread %q, want [a:S]", seen)
			}
		})
	}
}

// A panic in the hook fails open to the primary thread; the request still runs.
func TestThreadOfPanicFailsOpen(t *testing.T) {
	var seen []string
	pipe := components.NewPipeline([]components.Component{threadSpy{&seen}}, nil)
	res := BodyOpts(context.Background(), pipe, store.NewMemory(store.Options{}), Opts{
		Provider: bschemas.Anthropic, Session: "s",
		Body:     []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"x"}]}`),
		ThreadOf: func(string) thread.Result { panic("boom") },
	})
	if res.Thread.ID != thread.Primary || len(seen) != 1 || seen[0] != thread.Primary {
		t.Fatalf("thread %+v, pipeline saw %q", res.Thread, seen)
	}
}

type parentSpy struct{ seen *[]components.Ctx }

func (parentSpy) Name() string                 { return "parentspy" }
func (parentSpy) Enabled(*components.Ctx) bool { return true }
func (s parentSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = append(*s.seen, *c)
	return nil
}

// Ctx.ThreadParent and Ctx.ThreadInherits carry thread.Result.InheritsFrom to components, on both
// wire envelopes: set on the FIRST request of a side thread (parent = the primary thread, ""),
// and clear on the main thread's own requests.
func TestCtxCarriesTheParentThreadOnBothEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name, api, field, model string
	}{
		{"messages", "", "messages", "claude-sonnet-5"},
		{"responses", "responses", "input", "gpt-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []components.Ctx
			pipe := components.NewPipeline([]components.Component{parentSpy{&seen}}, nil)
			st, threads := store.NewMemory(store.Options{}), thread.New()
			const tools = `,"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}]`
			for _, body := range []string{
				`{"model":"` + tc.model + `","` + tc.field + `":[{"role":"user","content":"fix it"}]` + tools + `}`,
				`{"model":"` + tc.model + `","` + tc.field + `":[{"role":"user","content":"fix it"},{"role":"user","content":"title?"}]}`,
			} {
				b := []byte(body)
				BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, API: tc.api, Body: b, Session: "s",
					ThreadOf: func(s string) thread.Result { return threads.Resolve(s, "", b, false) }})
			}
			if len(seen) != 2 {
				t.Fatalf("pipeline ran %d times, want 2", len(seen))
			}
			if seen[0].ThreadInherits || seen[0].ThreadParent != "" {
				t.Errorf("main thread: ThreadInherits=%v ThreadParent=%q, want false, \"\"", seen[0].ThreadInherits, seen[0].ThreadParent)
			}
			if side := seen[1]; side.Thread == thread.Primary || !side.ThreadInherits || side.ThreadParent != thread.Primary {
				t.Errorf("side request: Thread=%q ThreadInherits=%v ThreadParent=%q, want its own thread inheriting from the primary",
					side.Thread, side.ThreadInherits, side.ThreadParent)
			}
		})
	}
}
