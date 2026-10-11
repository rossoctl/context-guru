package apply_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/skills"
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

func TestResponsesSessionIDForMatchesBodyOpts(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","instructions":"stable","input":[{"role":"user","content":"task"}]}`)
	res := apply.BodyOpts(context.Background(), nil, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Tenant: "tenant", Body: body,
	})
	if got := apply.SessionIDFor("tenant", "", bschemas.OpenAI, body); got == "" || got != res.Session {
		t.Fatalf("SessionIDFor = %q, BodyOpts = %q", got, res.Session)
	}
}

func TestResponsesEnvelopeToolTransforms(t *testing.T) {
	cfg := pipe(t, "pipeline: [toolschema, toolfilter]\ncomponents:\n  toolfilter: {remove: [unused, Keep]}\n")
	p, _ := cfg.Build(nil)
	body := []byte(`{"model":"gpt-5.6","instructions":"You may use Keep when needed.",` +
		`"input":[{"role":"user","content":"task"}],"tools":[` +
		`{"type":"function","name":"Keep","description":"keep","parameters":{"type":"object","title":"drop","properties":{"x":{"type":"string","title":"drop"}}}},` +
		`{"type":"function","name":"unused","parameters":{"type":"object","title":"drop"}},` +
		`{"type":"custom","name":"freeform","format":{"type":"text"}}]}`)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	if !res.Changed || res.FilteredDecls != 1 || res.FilteredDeclTokens == 0 {
		t.Fatalf("transforms did not act: changed=%v decls=%d tokens=%d", res.Changed, res.FilteredDecls, res.FilteredDeclTokens)
	}
	if got := gjson.GetBytes(res.Body, "tools.#").Int(); got != 2 {
		t.Fatalf("tool count = %d: %s", got, res.Body)
	}
	if gjson.GetBytes(res.Body, "tools.0.parameters.title").Exists() || gjson.GetBytes(res.Body, "tools.0.parameters.properties.x.title").Exists() {
		t.Fatalf("Responses parameter annotations survived: %s", res.Body)
	}
	if got := gjson.GetBytes(res.Body, "tools.0.name").String(); got != "Keep" {
		t.Fatalf("prose-described tool was removed: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "input").Raw != gjson.GetBytes(body, "input").Raw ||
		gjson.GetBytes(res.Body, "instructions").Raw != gjson.GetBytes(body, "instructions").Raw {
		t.Fatal("envelope transforms changed transcript fields")
	}
	second := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	if string(res.Body) != string(second.Body) {
		t.Fatal("Responses tool transform is not byte-stable")
	}
}

func TestResponsesToolFilterKeepsForcedAndPendingCalls(t *testing.T) {
	cfg := pipe(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter: {remove: [shell]}\n")
	p, _ := cfg.Build(nil)
	for _, tc := range []struct{ name, suffix string }{
		{"forced", `,"tool_choice":{"type":"function","name":"shell"}`},
		{"pending", `,"input":[{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}]`},
		{"answered", `,"input":[{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"function","name":"other","parameters":{"type":"object"}}]` + tc.suffix + `}`)
			res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body})
			want := int64(2)
			if tc.name == "answered" {
				want = 1
			}
			if got := gjson.GetBytes(res.Body, "tools.#").Int(); got != want {
				t.Fatalf("tool count = %d, want %d: %s", got, want, res.Body)
			}
		})
	}
}

func TestResponsesToolFilterReadsLeadingDeveloperProse(t *testing.T) {
	cfg := pipe(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter: {remove: [Keep, unused]}\n")
	p, _ := cfg.Build(nil)
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"developer","content":"Keep can be used when necessary."},{"role":"user","content":"task"}],` +
		`"tools":[{"type":"function","name":"Keep","parameters":{"type":"object"}},{"type":"function","name":"unused","parameters":{"type":"object"}}]}`)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body})
	if got := gjson.GetBytes(res.Body, "tools.#").Int(); got != 1 || gjson.GetBytes(res.Body, "tools.0.name").String() != "Keep" {
		t.Fatalf("developer prose did not protect named tool: %s", res.Body)
	}
}

func TestResponsesToolFilterRemovesSkillFromInstructions(t *testing.T) {
	cfg := pipe(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter: {remove: [skill__unused]}\n")
	p, _ := cfg.Build(nil)
	instructions := "<system-reminder>\n" + skills.Header + "\n\n- unused: Old skill.\n- needed: Kept skill.\n" + skills.ReminderEnd
	body, _ := json.Marshal(map[string]any{"model": "gpt-5.6", "instructions": instructions,
		"input": []any{map[string]any{"role": "user", "content": "task"}}})
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	got := gjson.GetBytes(res.Body, "instructions").String()
	if !res.Changed || res.FilteredDecls != 1 || strings.Contains(got, "- unused:") || !strings.Contains(got, "- needed:") {
		t.Fatalf("Responses instruction listing not filtered safely: changed=%v decls=%d instructions=%q", res.Changed, res.FilteredDecls, got)
	}
}

func TestResponsesSummarizeRebuildsPlainTextInput(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 2, keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","instructions":"keep this instruction","input":[` +
		`{"role":"user","content":"task"},` +
		`{"role":"assistant","content":"` + strings.Repeat("long answer ", 50) + `"},` +
		`{"role":"user","content":"more context"},` +
		`{"role":"user","content":"final question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "essential facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("background summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if !res.Changed {
		t.Fatalf("summary was not written to Responses input: %+v", res.Run)
	}
	if got := gjson.GetBytes(res.Body, "input.#").Int(); got != 3 {
		t.Fatalf("input count = %d, want 3: %s", got, res.Body)
	}
	if gjson.GetBytes(res.Body, "instructions").Raw != gjson.GetBytes(body, "instructions").Raw ||
		gjson.GetBytes(res.Body, "input.0").Raw != gjson.GetBytes(body, "input.0").Raw ||
		gjson.GetBytes(res.Body, "input.2").Raw != gjson.GetBytes(body, "input.3").Raw {
		t.Fatalf("retained fields changed: %s", res.Body)
	}
	if summary := gjson.GetBytes(res.Body, "input.1.content").String(); !strings.Contains(summary, "History Summary") || !strings.Contains(summary, "<<cg:") {
		t.Fatalf("missing recoverable summary: %q", summary)
	}
}

func TestResponsesSummarizeSummaryOnlyModeRebuilds(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 1, keep_last: 1, marker_mode: summary, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"task"},{"role":"assistant","content":"` + strings.Repeat("older context ", 50) + `"},{"role":"user","content":"question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if !res.Changed || gjson.GetBytes(res.Body, "input.#").Int() != 3 ||
		!strings.Contains(gjson.GetBytes(res.Body, "input.1.content").String(), expand.SummaryMarker) {
		t.Fatalf("summary-only marker was not written: %s", res.Body)
	}
}

func TestResponsesSummarizeStashesMultimodalHistory(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 2, keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","instructions":"stable","input":[` +
		`{"role":"user","content":"task"},` +
		`{"type":"reasoning","id":"r1","encrypted_content":"opaque"},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"` + strings.Repeat("tool output ", 50) + `"},{"type":"input_image","image_url":"opaque"}]},` +
		`{"role":"user","content":"question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("multimodal summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if !res.Changed || gjson.GetBytes(res.Body, "input.#").Int() != 3 {
		t.Fatalf("multimodal history was not recoverably summarized: %s", res.Body)
	}
	keys := expand.ParseMarkers(gjson.GetBytes(res.Body, "input.1.content").String())
	if len(keys) != 1 {
		t.Fatalf("summary has no expand marker: %s", res.Body)
	}
	stash, ok := expand.Resolve(st, keys[0])
	if !ok || !strings.Contains(stash, `"type":"input_image"`) || !strings.Contains(stash, `"encrypted_content":"opaque"`) {
		t.Fatalf("multimodal original was not stashed: %q", stash)
	}
}

func TestResponsesSummarizeDoesNotClaimToCompactServerHeldHistory(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	body := []byte(`{"model":"gpt-5.6","previous_response_id":"resp_prior","input":[` +
		`{"role":"user","content":"` + strings.Repeat("visible history ", 80) + `"},` +
		`{"role":"user","content":"question"}]}`)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "facts"}},
	})
	if res.Changed || string(res.Body) != string(body) {
		t.Fatalf("upstream-held history was misrepresented as compacted: %s", res.Body)
	}
}

func TestResponsesServerHeldHistoryDoesNotCreateCachedPrefixBoundaries(t *testing.T) {
	for _, useTracker := range []bool{false, true} {
		t.Run(strconv.FormatBool(useTracker), func(t *testing.T) {
			probe := &cachePhaseProbe{}
			p := components.NewPipeline([]components.Component{probe}, nil)
			st := store.NewMemory(store.Options{})
			var tracker *modes.Tracker
			if useTracker {
				tracker = modes.NewTracker(0)
			}
			beforeResets := modes.CompactionResets()
			for i, count := range []int{3, 1, 2} {
				var items []string
				for j := 0; j < count; j++ {
					items = append(items, `{"role":"user","content":"visible delta `+strconv.Itoa(j)+`"}`)
				}
				body := []byte(`{"model":"gpt-5.6","previous_response_id":"resp_` + strconv.Itoa(i) +
					`","input":[` + strings.Join(items, ",") + `]}`)
				res := apply.BodyOpts(context.Background(), p, st, apply.Opts{
					Provider: bschemas.OpenAI, API: "responses", Body: body,
					Session: "same-session", Tracker: tracker,
					Now: time.Unix(1_700_000_000+int64(i*60), 0),
				})
				if !res.CacheAware || res.MaxCachedIdx != -1 || res.FrozenTokens != 0 ||
					res.AttemptedTokens <= 0 || probe.seen.IdleMs != -1 ||
					probe.seen.MaxCachedIdx != -1 || !probe.seen.DisallowCountChange {
					t.Fatalf("server-held turn %d reported a visible cached prefix: result=%+v ctx=%+v",
						i, res.Trace, probe.seen)
				}
			}
			if got := modes.CompactionResets(); got != beforeResets {
				t.Fatalf("server-held deltas produced %d false compaction resets", got-beforeResets)
			}
			if tracker != nil && tracker.Sessions() != 0 {
				t.Fatal("server-held deltas entered the cumulative-transcript tracker")
			}
		})
	}
}

func TestResponsesMinimumLifetimeNeverClaimsColdAfterLongIdle(t *testing.T) {
	probe := &cachePhaseProbe{}
	p := components.NewPipeline([]components.Component{probe}, nil)
	st, tracker := store.NewMemory(store.Options{}), modes.NewTracker(0)
	body := []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"stable"}]}`)
	base := time.Unix(1_700_000_000, 0)
	for _, at := range []time.Time{base, base.Add(48 * time.Hour)} {
		apply.BodyOpts(context.Background(), p, st, apply.Opts{
			Provider: bschemas.OpenAI, API: "responses", Body: body,
			Session: "same-session", Tracker: tracker, Now: at,
		})
	}
	if probe.seen.ColdCache || probe.seen.CacheTTLMs != (30*time.Minute).Milliseconds() ||
		!probe.seen.CacheTTLMinimum {
		t.Fatalf("minimum lifetime was treated as expiry: cold=%v ttl=%d minimum=%v",
			probe.seen.ColdCache, probe.seen.CacheTTLMs, probe.seen.CacheTTLMinimum)
	}
}

func TestResponsesSummarizeKeepsOpaqueTailVerbatim(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 2, keep_last: 5, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","instructions":"stable","input":[` +
		`{"role":"user","content":"task"},` +
		`{"role":"assistant","content":"` + strings.Repeat("old answer ", 50) + `"},` +
		`{"role":"user","content":"old follow-up"},` +
		`{"role":"assistant","content":"latest answer"},` +
		`{"type":"reasoning","id":"r1","encrypted_content":"opaque"},` +
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"tool result"},` +
		`{"role":"user","content":"latest question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "earlier facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("background summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if !res.Changed || gjson.GetBytes(res.Body, "input.#").Int() != 7 {
		t.Fatalf("mixed-state summary did not reduce text span: body=%s report=%+v", res.Body, res.Run)
	}
	for outIdx, origIdx := range map[int]int{0: 0, 2: 3, 3: 4, 4: 5, 5: 6, 6: 7} {
		outPath, origPath := "input."+strconv.Itoa(outIdx), "input."+strconv.Itoa(origIdx)
		if gjson.GetBytes(res.Body, outPath).Raw != gjson.GetBytes(body, origPath).Raw {
			t.Errorf("retained item %d changed: %s", origIdx, res.Body)
		}
	}
	if summary := gjson.GetBytes(res.Body, "input.1.content").String(); !strings.Contains(summary, "History Summary") || !strings.Contains(summary, "<<cg:") {
		t.Fatalf("summary not recoverable: %q", summary)
	}
}

func TestResponsesSummarizeStashesAndReplacesCompleteOpaqueHistory(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_first: 2, keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := []byte(`{"model":"gpt-5.6","instructions":"stable","input":[` +
		`{"role":"user","content":"task"},` +
		`{"role":"assistant","content":"` + strings.Repeat("old answer ", 50) + `"},` +
		`{"type":"reasoning","id":"r1","encrypted_content":"opaque"},` +
		`{"type":"custom_tool_call","call_id":"c1","name":"shell","input":"{}"},` +
		`{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"first old tool result"},{"type":"input_text","text":"second old tool result"}]},` +
		`{"role":"user","content":"old follow-up"},` +
		`{"role":"user","content":"latest question"}]}`)
	o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", Body: body,
		Models: components.ModelSpec{Incoming: stubModel{resp: "earlier facts"}}}
	apply.BodyOpts(context.Background(), p, st, o)
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("background summary did not finish")
	}
	res := apply.BodyOpts(context.Background(), p, st, o)
	if !res.Changed || gjson.GetBytes(res.Body, "input.#").Int() != 3 {
		t.Fatalf("complete historical exchange did not compact: body=%s report=%+v", res.Body, res.Run)
	}
	if gjson.GetBytes(res.Body, "input.0").Raw != gjson.GetBytes(body, "input.0").Raw ||
		gjson.GetBytes(res.Body, "input.2").Raw != gjson.GetBytes(body, "input.6").Raw {
		t.Fatal("retained task or latest turn changed")
	}
	keys := expand.ParseMarkers(gjson.GetBytes(res.Body, "input.1.content").String())
	if len(keys) != 1 {
		t.Fatalf("summary has no expand key: %s", res.Body)
	}
	stash, ok := expand.Resolve(st, keys[0])
	if !ok || gjson.Get(stash, "#").Int() != 5 ||
		gjson.Get(stash, "1.encrypted_content").String() != "opaque" ||
		gjson.Get(stash, "2.call_id").String() != "c1" {
		t.Fatalf("opaque history not recoverable from stash: %q", stash)
	}
	changedOpaque := []byte(strings.Replace(string(body), `"encrypted_content":"opaque"`, `"encrypted_content":"different"`, 1))
	o.Body = changedOpaque
	changed := apply.BodyOpts(context.Background(), p, st, o)
	if changed.Changed || string(changed.Body) != string(changedOpaque) {
		t.Fatalf("changed opaque history incorrectly reused the old checkpoint: %s", changed.Body)
	}
	if !offload.WaitForAllSummariesForTest(5 * time.Second) {
		t.Fatal("replacement summary did not finish")
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

// The Responses route must set Ctx.RawInput exactly as the Messages route does (PR #431 review
// round 2): without it, a stale-prefix guard falls back to req.Input at its own pipeline position,
// which an earlier component has already rewritten.
func TestResponsesSetsRawInputBeforeThePipelineRewrites(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"original output"}]}`)
	probe := &cachePhaseProbe{}
	p := components.NewPipeline([]components.Component{responseReducer{}, probe}, nil)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, API: "responses", Body: body,
	})
	raw := probe.seen.RawInput
	if len(raw) == 0 || len(raw) != res.Messages {
		t.Fatalf("RawInput has %d messages, want %d (the normalized input)", len(raw), res.Messages)
	}
	var sawTool bool
	for _, m := range raw {
		if m.Role == bschemas.ChatMessageRoleTool {
			sawTool = true
			if got := schema.MessageText(m); got != "original output" {
				t.Fatalf("RawInput tool output = %q, want the pre-pipeline text", got)
			}
		}
	}
	if !sawTool {
		t.Fatal("RawInput has no tool message, so the test proves nothing")
	}
	if got := gjson.GetBytes(res.Body, "input.2.output").String(); got != "reduced" {
		t.Fatalf("the earlier component did not run: output = %q", got)
	}
}
