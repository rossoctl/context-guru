package apply_test

import (
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

type responseReducer struct{}

type cachePhaseProbe struct{ seen components.Ctx }

func (*cachePhaseProbe) Name() string                 { return "cache-phase-probe" }
func (*cachePhaseProbe) Enabled(*components.Ctx) bool { return true }
func (p *cachePhaseProbe) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	p.seen = *c
	return nil
}

func (responseReducer) Name() string                 { return "responses-test" }
func (responseReducer) Enabled(*components.Ctx) bool { return true }
func (responseReducer) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleTool {
			schema.SetMessageText(&req.Input[i], "reduced")
		}
	}
	return nil
}

func TestResponsesRewritesToolOutputWithoutChangingEnvelope(t *testing.T) {
	body := []byte(` { "model":"gpt-5.6", "instructions":"keep me", "prompt_cache_options":{"ttl":"30m"}, "input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},` +
		`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"` + strings.Repeat("large output ", 20) + `"}` +
		`], "stream":true } `)
	p := components.NewPipeline([]components.Component{responseReducer{}}, nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	if !res.Changed {
		t.Fatal("expected compatible tool output to be rewritten")
	}
	if got := gjson.GetBytes(res.Body, "input.3.output").String(); got != "reduced" {
		t.Fatalf("output = %q", got)
	}
	for _, path := range []string{"model", "instructions", "prompt_cache_options", "input.0", "input.1", "input.2", "stream"} {
		if a, b := gjson.GetBytes(res.Body, path).Raw, gjson.GetBytes(body, path).Raw; a != b {
			t.Errorf("untouched %s changed:\nwant %s\n got %s", path, b, a)
		}
	}
}

func TestResponsesRewritesCodexCustomToolOutputBlocks(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[` +
		`{"type":"custom_tool_call","call_id":"call_1","name":"exec_command","input":"{}"},` +
		`{"type":"custom_tool_call_output","call_id":"call_1","output":[` +
		`{"type":"input_text","text":"metadata"},` +
		`{"type":"input_text","text":"` + strings.Repeat("large output ", 20) + `"},` +
		`{"type":"input_image","image_url":"opaque"}]}` +
		`]}`)
	p := components.NewPipeline([]components.Component{responseReducer{}}, nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	if !res.Changed {
		t.Fatal("expected custom tool output blocks to be rewritten")
	}
	for _, path := range []string{"input.1.output.0.text", "input.1.output.1.text"} {
		if got := gjson.GetBytes(res.Body, path).String(); got != "reduced" {
			t.Errorf("%s = %q", path, got)
		}
	}
	for _, path := range []string{"input.0", "input.1.output.2"} {
		if a, b := gjson.GetBytes(res.Body, path).Raw, gjson.GetBytes(body, path).Raw; a != b {
			t.Errorf("untouched %s changed:\nwant %s\n got %s", path, b, a)
		}
	}
}

func TestResponsesUnsupportedInputPassesThroughByteForByte(t *testing.T) {
	body := []byte(" { \"model\": \"gpt-5\", \"input\": [{\"type\":\"reasoning\",\"id\":\"r\"}] } \n")
	p := components.NewPipeline([]components.Component{responseReducer{}}, nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	if res.Changed || string(res.Body) != string(body) {
		t.Fatalf("passthrough changed bytes:\nwant %q\n got %q", body, res.Body)
	}
}

func TestOpenAIAutoCachePhaseUsesThirtyMinuteMinimum(t *testing.T) {
	for _, api := range []string{"", "responses"} {
		t.Run(api, func(t *testing.T) {
			probe := &cachePhaseProbe{}
			p := components.NewPipeline([]components.Component{probe}, nil)
			st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
			base := time.Unix(1_700_000_000, 0)
			var first, second, third []byte
			if api == "responses" {
				first = []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"}]}`)
				second = []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"}]}`)
				third = []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"},{"type":"message","role":"user","content":"three"}]}`)
			} else {
				first = []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"one"}]}`)
				second = []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`)
				third = []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"}]}`)
			}
			for i, turn := range []struct {
				body []byte
				at   time.Time
				want components.CachePhase
			}{
				{first, base, components.CachePhaseUnknown},
				{second, base.Add(29*time.Minute + 30*time.Second), components.CachePhaseWarm},
				{third, base.Add(60*time.Minute + 30*time.Second), components.CachePhaseUnknown},
			} {
				res := apply.BodyOpts(context.Background(), p, st, apply.Opts{
					Provider: bschemas.OpenAI, API: api, Body: turn.body,
					Session: "cache-test", Tracker: tr, Now: turn.at,
				})
				if !res.CacheAware || !probe.seen.CacheTTLMinimum || probe.seen.CacheTTLMs != (30*time.Minute).Milliseconds() {
					t.Fatalf("turn %d: cache facts = aware=%v minimum=%v ttl=%d", i, res.CacheAware, probe.seen.CacheTTLMinimum, probe.seen.CacheTTLMs)
				}
				if got := probe.seen.CachePhase(time.Minute); got != turn.want {
					t.Errorf("turn %d: phase = %s, want %s", i, got, turn.want)
				}
				if i > 0 && res.MaxCachedIdx < 0 {
					t.Errorf("turn %d: cached prefix was not tracked", i)
				}
			}
		})
	}
}

func TestResponsesKeepAliveTouchRefreshesTheCacheClock(t *testing.T) {
	probe := &cachePhaseProbe{}
	p := components.NewPipeline([]components.Component{probe}, nil)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	base := time.Unix(1_700_000_000, 0)
	first := []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"}]}`)
	second := []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"}]}`)
	third := []byte(`{"model":"gpt-5.6","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"},{"type":"message","role":"user","content":"three"}]}`)
	for _, turn := range []struct {
		body []byte
		at   time.Time
	}{{first, base}, {second, base.Add(time.Minute)}} {
		apply.BodyOpts(context.Background(), p, st, apply.Opts{
			Provider: bschemas.OpenAI, API: "responses", Body: turn.body,
			Session: "cache-touch", Tracker: tr, Now: turn.at,
		})
	}
	apply.RecordCacheTouch(st, "", second, bschemas.OpenAI, base.Add(28*time.Minute).UnixMilli())
	apply.BodyOpts(context.Background(), p, st, apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: third,
		Session: "cache-touch", Tracker: tr, Now: base.Add(40 * time.Minute),
	})
	if got := probe.seen.CachePhase(time.Minute); got != components.CachePhaseWarm {
		t.Fatalf("phase after a confirmed keep-alive read = %s, want warm", got)
	}
}
