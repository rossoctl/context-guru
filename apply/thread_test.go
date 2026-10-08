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
