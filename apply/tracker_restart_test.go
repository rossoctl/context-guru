package apply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

// Issue #496. After a proxy restart the tracker is new but the provider's cache is not: the
// first request of a live thread must read the whole prompt as the cached part, so extract_llm
// gates on cached_prefix instead of compacting deep in the prefix (the restart row 29967: 925
// messages, message 253 compacted 62 s after the previous request).
func TestTheFirstRequestAfterARestartKeepsTheCachedPart(t *testing.T) {
	cfg := pipe(t, "pipeline: [extract_llm]\ncomponents:\n"+
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, "+
		"allow_on_caching_backend: true, trigger: {min_request_frac: 0}}\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}

	// 925 messages: the user turn, then 462 tool_use/tool_result pairs.
	msgs := []map[string]any{{"role": "user", "content": "go"}}
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

	extract := func(res apply.Result) *components.Report {
		t.Helper()
		for i := range res.Run.Components {
			if res.Run.Components[i].Component == "extract_llm" {
				return &res.Run.Components[i]
			}
		}
		t.Fatal("no extract_llm report in the run")
		return nil
	}

	// The restart: a NEW tracker, so the live thread's record is gone. Now is left at zero,
	// so the request runs on the real clock the tracker started on.
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0),
		Body: body, Models: components.ModelSpec{Incoming: restartModel{}},
	})
	if res.Messages != 925 {
		t.Fatalf("normalized message count = %d, want 925 (the fixture changed shape)", res.Messages)
	}
	if res.MaxCachedIdx != 924 {
		t.Fatalf("first request after a restart: MaxCachedIdx = %d, want 924 (the whole prompt is the cached part)", res.MaxCachedIdx)
	}
	rep := extract(res)
	if rep.Gates["cached_prefix"] == 0 {
		t.Errorf("extract_llm gated nothing on cached_prefix (gates %v): it was free to compact the prefix", rep.Gates)
	}
	if len(rep.ChangedIdx) != 0 {
		t.Errorf("extract_llm changed messages %v deep in a prefix the provider still holds", rep.ChangedIdx)
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
	crep := extract(ctl)
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
