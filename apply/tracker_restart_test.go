package apply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// restartModel satisfies components.Model so extract_llm accepts candidates. The
// deterministic strategy never calls it.
type restartModel struct{}

func (restartModel) Complete(context.Context, string) (string, error) {
	return "essential facts", nil
}

// restartBody builds the #496 shape: 925 messages, the user turn then 462 tool_use/tool_result
// pairs (the restart row 29967). hourAsk puts ttl "1h" on the first user message's cache
// control, the way Claude Code on a subscription asks for the extended tier.
func restartBody(t *testing.T, hourAsk bool) []byte {
	t.Helper()
	first := map[string]any{"role": "user", "content": "go"}
	if hourAsk {
		first = map[string]any{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "go", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}}
	}
	msgs := []map[string]any{first}
	for i := 0; i < 462; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": id,
					"content": fmt.Sprintf("output %d\n%s", i, strings.Repeat("payload line\n", 600))}}})
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5", "metadata": map[string]any{"taskId": "restart-496"}, "messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// extract returns the extract_llm report of a run.
func extract(t *testing.T, res apply.Result) *components.Report {
	t.Helper()
	for i := range res.Run.Components {
		if res.Run.Components[i].Component == "extract_llm" {
			return &res.Run.Components[i]
		}
	}
	t.Fatal("no extract_llm report in the run")
	return nil
}

// runRestart sends body through the pipeline on a tracker that started age ago (Now left at
// zero, so the request runs on the real clock the tracker started on). It returns the tracker
// too, so a test can send a second request on the same thread.
func runRestart(t *testing.T, p *components.Pipeline, body []byte, age time.Duration) (apply.Result, *modes.Tracker) {
	t.Helper()
	tr := modes.NewTracker(0)
	tr.SetStartedAt(time.Now().Add(-age).UnixMilli())
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, CacheMode: "on", Tracker: tr,
		Body: body, Models: components.ModelSpec{Incoming: restartModel{}},
	})
	return res, tr
}

// Issue #496. After a proxy restart the tracker is new but the provider's cache is not: the
// first request of a live thread, inside that request's cache lifetime, reads the whole prompt
// as the cached part, so extract_llm gates on cached_prefix instead of compacting deep in the
// prefix. A plain Anthropic body carries the 5-minute lifetime, so 2 minutes after the start
// is inside it.
func TestTheFirstRequestAfterARestartKeepsTheCachedPart(t *testing.T) {
	cfg := pipe(t, "pipeline: [extract_llm]\ncomponents:\n"+
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, "+
		"allow_on_caching_backend: true, trigger: {min_request_frac: 0}}\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := restartBody(t, false)

	res, tr := runRestart(t, p, body, 2*time.Minute)
	if res.Messages != 925 {
		t.Fatalf("normalized message count = %d, want 925 (the fixture changed shape)", res.Messages)
	}
	if res.MaxCachedIdx != 924 {
		t.Fatalf("first request after a restart: MaxCachedIdx = %d, want 924 (the whole prompt is the cached part)", res.MaxCachedIdx)
	}
	rep := extract(t, res)
	if rep.Gates["cached_prefix"] == 0 {
		t.Errorf("extract_llm gated nothing on cached_prefix (gates %v): it was free to compact the prefix", rep.Gates)
	}
	if len(rep.ChangedIdx) != 0 {
		t.Errorf("extract_llm changed messages %v deep in a prefix the provider still holds", rep.ChangedIdx)
	}

	// Turn 2 on the same thread: the first request wrote the thread record, so the boundary is
	// now real (925 of 927 messages) and the grace no longer applies. The NEW tail must be
	// changeable, and nothing deeper.
	tail := append(append([]map[string]any{}, bodyMessages(body)...),
		map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "call_tail", "name": "Read", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "call_tail",
				"content": "output tail\n" + strings.Repeat("payload line\n", 600)}}})
	body2, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5", "metadata": map[string]any{"taskId": "restart-496"}, "messages": tail,
	})
	if err != nil {
		t.Fatal(err)
	}
	res2 := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, CacheMode: "on", Tracker: tr,
		Body: body2, Models: components.ModelSpec{Incoming: restartModel{}},
	})
	if res2.Messages != 927 {
		t.Fatalf("turn 2: normalized message count = %d, want 927 (the fixture changed shape)", res2.Messages)
	}
	if res2.MaxCachedIdx != 924 {
		t.Fatalf("turn 2: MaxCachedIdx = %d, want 924 (the boundary is the 925 the first request recorded)", res2.MaxCachedIdx)
	}
	rep2 := extract(t, res2)
	changed := false
	for _, i := range rep2.ChangedIdx {
		if i < 925 {
			t.Errorf("turn 2: extract_llm changed message %d, below the boundary 925", i)
		}
		changed = true
	}
	if !changed {
		t.Fatalf("turn 2: extract_llm changed nothing; the new tail must be changeable (gates %v)", rep2.Gates)
	}

	// Control: the same request with the boundary unknown (no tracker, the library path)
	// compacts deep in the prefix — the defect shape. Without this, the assertions above
	// could pass because the fixture never offered extract_llm a candidate.
	ctl := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, CacheMode: "on",
		Body: body, Models: components.ModelSpec{Incoming: restartModel{}},
	})
	if ctl.MaxCachedIdx != -1 {
		t.Fatalf("control: MaxCachedIdx = %d, want -1 (boundary unknown)", ctl.MaxCachedIdx)
	}
	crep := extract(t, ctl)
	deep := false
	for _, i := range crep.ChangedIdx {
		if i < 900 {
			deep = true
		}
	}
	if !deep {
		t.Fatalf("control: extract_llm changed only %v; the fixture never compacted deep in the prefix", crep.ChangedIdx)
	}
}

// bodyMessages decodes a restart body's messages, for extending them in the two-turn test.
func bodyMessages(body []byte) []map[string]any {
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	json.Unmarshal(body, &parsed)
	return parsed.Messages
}

// Past the request's cache lifetime the restart can no longer matter: no entry it forgot can
// still be alive, so the first request of the thread is a new thread, as before the PR. A
// plain Anthropic body carries 5 minutes; 7 minutes after the start the grace is over.
func TestARequestPastItsCacheLifetimeAfterARestartIsANewThread(t *testing.T) {
	cfg := pipe(t, "pipeline: [extract_llm]\ncomponents:\n"+
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, "+
		"allow_on_caching_backend: true, trigger: {min_request_frac: 0}}\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := restartBody(t, false)

	res, _ := runRestart(t, p, body, 7*time.Minute)
	if res.MaxCachedIdx != -1 {
		t.Fatalf("7 minutes past the start, 5 minute lifetime: MaxCachedIdx = %d, want -1 (a new thread, as today)", res.MaxCachedIdx)
	}
	rep := extract(t, res)
	deep := false
	for _, i := range rep.ChangedIdx {
		if i < 900 {
			deep = true
		}
	}
	if !deep {
		t.Fatalf("extract_llm changed only %v; the fixture never compacted deep in the prefix", rep.ChangedIdx)
	}
}

// A body that asks for the extended tier (ttl "1h", what Claude Code on a subscription sends)
// carries a 1-hour lifetime, so its thread stays protected longer: 40 minutes after the start
// is inside that lifetime and past the 5-minute one.
func TestAnExtendedTierRequestStaysProtectedLongerAfterARestart(t *testing.T) {
	cfg := pipe(t, "pipeline: [extract_llm]\ncomponents:\n"+
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, "+
		"allow_on_caching_backend: true, trigger: {min_request_frac: 0}}\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := restartBody(t, true)

	res, _ := runRestart(t, p, body, 40*time.Minute)
	if res.MaxCachedIdx != 924 {
		t.Fatalf("40 minutes past the start, 1 hour lifetime: MaxCachedIdx = %d, want 924 (the whole prompt is the cached part)", res.MaxCachedIdx)
	}
	rep := extract(t, res)
	if rep.Gates["cached_prefix"] == 0 {
		t.Errorf("extract_llm gated nothing on cached_prefix (gates %v): it was free to compact the prefix", rep.Gates)
	}
	if len(rep.ChangedIdx) != 0 {
		t.Errorf("extract_llm changed messages %v deep in a prefix the provider still holds", rep.ChangedIdx)
	}
}
