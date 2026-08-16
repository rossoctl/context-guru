package all_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func run(t *testing.T, yaml string, req *schemas.BifrostChatRequest) (*components.RunReport, store.Store) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st}
	return pipe.Run(req, c), st
}

func TestFormatCompactsJSON(t *testing.T) {
	type row struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	rows := make([]row, 20)
	for i := range rows {
		rows[i] = row{ID: i, Name: "item-with-a-longish-name"}
	}
	pretty, _ := json.MarshalIndent(rows, "", "    ")
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(string(pretty))}}
	before := schema.MessagesTokens(req)
	run(t, "pipeline: [format]\n", req)
	after := schema.MessagesTokens(req)
	if after >= before {
		t.Fatalf("format did not compact JSON: before=%d after=%d", before, after)
	}
	// still valid JSON (lossless)
	var back []row
	if err := json.Unmarshal([]byte(schema.MessageText(req.Input[0])), &back); err != nil || len(back) != 20 {
		t.Fatalf("format broke JSON validity: %v", err)
	}
}

// TestSkeletonElidesBodies moved to skeleton_test.go (cg_skeleton build tag).

func TestFailedRunSupersedes(t *testing.T) {
	run1 := "=== test session starts ===\n" + strings.Repeat("detail line about the failing run\n", 20) + "3 failed, 2 passed in 1.2s\n"
	run2 := "=== test session starts ===\n" + strings.Repeat("detail line about the passing run\n", 20) + "0 failed, 5 passed in 1.0s\n"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(run1), toolMsg("some unrelated note"), toolMsg(run2)}}
	_, st := run(t, "pipeline: [failed_run]\n", req)
	first := schema.MessageText(req.Input[0])
	if !strings.Contains(first, "superseded") {
		t.Fatalf("earlier run should be collapsed: %q", first)
	}
	if !strings.Contains(schema.MessageText(req.Input[2]), "0 failed") {
		t.Fatal("latest run must be kept in full")
	}
	if keys := expand.ParseMarkers(first); len(keys) != 1 {
		t.Fatal("collapsed run should carry an expand marker")
	} else if orig, ok := expand.Resolve(st, keys[0]); !ok || !strings.Contains(orig, "3 failed") {
		t.Fatal("expand must recover the superseded run")
	}
}

// A PASSED earlier run is a distinct result the agent may still reference, so it
// must NOT be collapsed as "superseded" — only failed earlier runs are.
func TestFailedRunKeepsPassedRun(t *testing.T) {
	pass1 := "=== test session starts ===\n" + strings.Repeat("detail line about test suite A\n", 20) + "0 failed, 7 passed in 1.1s\n"
	pass2 := "=== test session starts ===\n" + strings.Repeat("detail line about test suite B\n", 20) + "0 failed, 4 passed in 0.9s\n"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(pass1), toolMsg(pass2)}}
	run(t, "pipeline: [failed_run]\n", req)
	if strings.Contains(schema.MessageText(req.Input[0]), "superseded") {
		t.Fatalf("a PASSED earlier run must be kept verbatim, got: %q", schema.MessageText(req.Input[0]))
	}
	if !strings.Contains(schema.MessageText(req.Input[0]), "7 passed") {
		t.Fatal("passed earlier run content must remain")
	}
}

// In cache-aware mode a superseded FAILED run that sits in the already-cached
// prefix (index <= MaxCachedIdx) must NOT be collapsed — doing so would mutate the
// cached prefix and force a provider cache-write. Only runs in the uncached tail
// are collapsible.
func TestFailedRunCacheAwareTailOnly(t *testing.T) {
	run1 := "=== test session starts ===\n" + strings.Repeat("detail line about the failing run\n", 20) + "3 failed, 2 passed in 1.2s\n"
	run2 := "=== test session starts ===\n" + strings.Repeat("detail line about the passing run\n", 20) + "0 failed, 5 passed in 1.0s\n"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(run1), toolMsg("some unrelated note"), toolMsg(run2)}}

	cfg, err := config.LoadBytes([]byte("pipeline: [failed_run]\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Boundary at index 0: run1 (index 0) is "already cached", so it must be left
	// verbatim even though a later run supersedes it.
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{}), CacheAware: true, MaxCachedIdx: 0}
	pipe.Run(req, c)
	if strings.Contains(schema.MessageText(req.Input[0]), "superseded") {
		t.Fatalf("cache-aware: cached-prefix run must stay verbatim, got: %q", schema.MessageText(req.Input[0]))
	}
	if !strings.Contains(schema.MessageText(req.Input[0]), "3 failed") {
		t.Fatal("cached-prefix run content must remain intact")
	}
}

// The other half of TestFailedRunCacheAwareTailOnly, and the half that was missing: a
// superseded FAILED run in the UNCACHED TAIL must still be collapsed under
// CacheAware:true. failed_run gated on `c.CacheAware` (per REQUEST) where its sibling
// mask gates on `!c.TailOnly(i)` (per MESSAGE), so cache-awareness disabled every new
// collapse at every depth — and since resolveCacheAware is true by default for
// Anthropic/Bedrock/Vertex, the component could never act at all on the flagship
// workload. The negative test above passed vacuously for the same reason.
func TestFailedRunCacheAwareStillCollapsesTheTail(t *testing.T) {
	run1 := "=== test session starts ===\n" + strings.Repeat("detail line about the failing run\n", 20) + "3 failed, 2 passed in 1.2s\n"
	run2 := "=== test session starts ===\n" + strings.Repeat("detail line about the passing run\n", 20) + "0 failed, 5 passed in 1.0s\n"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(run1), toolMsg("some unrelated note"), toolMsg(run2)}}

	cfg, err := config.LoadBytes([]byte("pipeline: [failed_run]\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	// First turn of the session: nothing is committed to the provider cache yet, so every
	// message is in the mutable tail (MaxCachedIdx = -1).
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{}),
		CacheAware: true, MaxCachedIdx: -1}
	pipe.Run(req, c)
	if !strings.Contains(schema.MessageText(req.Input[0]), "superseded") {
		t.Fatalf("cache-aware: a superseded failed run in the UNCACHED TAIL must be collapsed, got: %q",
			schema.MessageText(req.Input[0]))
	}
	if !strings.Contains(schema.MessageText(req.Input[2]), "0 failed") {
		t.Fatal("latest run must be kept in full")
	}
}

func TestCollapseHeadTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("log line number with some content ")
		b.WriteString(strings.Repeat("x", 5))
		b.WriteByte('\n')
	}
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(b.String())}}
	before := schema.MessagesTokens(req)
	_, st := run(t, "pipeline: [collapse]\ncomponents:\n  collapse: {max_tokens: 20, head_lines: 3, tail_lines: 3}\n", req)
	got := schema.MessageText(req.Input[0])
	if schema.TextTokens(got) >= before || !strings.Contains(got, "lines omitted") {
		t.Fatalf("collapse should head/tail truncate: %q", got)
	}
	if keys := expand.ParseMarkers(got); len(keys) != 1 {
		t.Fatal("collapse should leave an expand marker")
	} else if _, ok := expand.Resolve(st, keys[0]); !ok {
		t.Fatal("collapse original must be recoverable")
	}
}

// TestMaskFreezeByteStableAcrossTurns is the core cache-stability regression: once mask
// hides an output, it must replay the IDENTICAL masked bytes on every later turn even
// after that output slides into the already-cached prefix. Before the fix, mask gated on
// TailOnly with no freeze/reapply, so on turn 2 the cached-prefix output reverted to full
// (masked→full→…), churning the provider KV cache every turn.
func TestMaskFreezeByteStableAcrossTurns(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 1, min_tokens: 20}\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	big := strings.Repeat("older tool output content line that is long enough to mask\n", 30)

	// Turn 1: whole request is the uncached tail (MaxCachedIdx -1); index 0 gets masked.
	req1 := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(big), toolMsg("newest tiny output")}}
	c1 := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, CacheAware: true, MaxCachedIdx: -1}
	pipe.Run(req1, c1)
	masked1 := schema.MessageText(req1.Input[0])
	if !strings.Contains(masked1, "masked") {
		t.Fatalf("turn 1 should mask index 0: %q", masked1)
	}

	// Turn 2: the agent re-sends index 0's FULL original; it is now in the cached prefix
	// (MaxCachedIdx 1 ⇒ TailOnly(0)=false). mask must replay the frozen bytes, not revert.
	req2 := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(big), toolMsg("newest tiny output"), toolMsg("turn 2 new output")}}
	c2 := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, CacheAware: true, MaxCachedIdx: 1}
	pipe.Run(req2, c2)
	masked2 := schema.MessageText(req2.Input[0])
	if masked2 != masked1 {
		t.Fatalf("mask must replay byte-identically across turns (cache-stable):\n turn1=%q\n turn2=%q", masked1, masked2)
	}
}

// TestKeptVerbatimNotRecompacted: after the agent expands an offloaded output (the proxy
// marks it kept-verbatim), no offloader may re-compact it on the next turn — otherwise the
// model expands it again, a per-turn bounce loop. Before the fix only extract_llm honored
// the flag; mask/collapse/extract/dedup/etc. ignored it.
func TestKeptVerbatimNotRecompacted(t *testing.T) {
	cfg, err := config.LoadBytes([]byte("pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 1, min_tokens: 20}\n"))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	big := strings.Repeat("recoverable content the agent just expanded and needs verbatim\n", 30)
	offload.MarkKeptVerbatim(st, big) // simulate the proxy's expand loop

	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{toolMsg(big), toolMsg("newest tiny output")}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st}
	pipe.Run(req, c)
	if strings.Contains(schema.MessageText(req.Input[0]), "masked") {
		t.Fatalf("kept-verbatim content must not be re-masked: %q", schema.MessageText(req.Input[0]))
	}
}
