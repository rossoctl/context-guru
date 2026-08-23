package all_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// TestExtractResultCacheHitsAcrossSessions is the headline acceptance criterion for issue
// #28 part C: identical content in a DIFFERENT session must reuse the prior extraction
// instead of paying for it again. Before the global re-key the result cache carried a
// session prefix, so the second session re-derived a result the system already had —
// measured wasteful on 82 of 103 unique contents.
func TestExtractResultCacheHitsAcrossSessions(t *testing.T) {
	// economic_gate: false isolates the CACHE behavior under test from the gate's
	// (separately tested) spending decision.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{}) // one store, as a real proxy has
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	runIn := func(session string) *bschemas.BifrostChatRequest {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("find the keep records"), toolMsg(body),
		}}
		c := &components.Ctx{Ctx: context.Background(), Session: session, Store: st,
			Model: components.ModelSpec{Static: cm}}
		var rep components.Report
		if _, err := off.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return req
	}

	req1 := runIn("session-A")
	if cm.calls != 1 {
		t.Fatalf("first session must call the model once, calls=%d", cm.calls)
	}
	out1 := schema.MessageText(req1.Input[1])
	if strings.Contains(out1, "drop this") {
		t.Fatal("first session should have reduced the output")
	}

	// A DIFFERENT session, same content. This is the case the session-scoped key missed.
	req2 := runIn("session-B-completely-different")
	if cm.calls != 1 {
		t.Fatalf("a different session must REUSE the cached extraction (no new model call), calls=%d", cm.calls)
	}
	out2 := schema.MessageText(req2.Input[1])
	if strings.Contains(out2, "drop this") {
		t.Fatal("cross-session reuse must still drop the non-keep record")
	}

	// A third, also free.
	runIn("session-C")
	if cm.calls != 1 {
		t.Fatalf("every later session must reuse, calls=%d", cm.calls)
	}
}

// The gate must actually suppress in a real pipeline run on a cache-aware request with a
// small output — the Terminal-Bench losing case, end to end rather than in unit isolation.
func TestExtractGateSuppressesInPipelineWhenCacheAware(t *testing.T) {
	off := newComp(t, "extract_llm", "strategy: code\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40) // ~400 tokens: far below the ~12.7k cached break-even
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s1", Store: st,
		Model: components.ModelSpec{Static: cm}, CacheAware: true, MaxCachedIdx: -1,
		CtxWindow: 1_000_000}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if cm.calls != 0 {
		t.Fatalf("a small output on a cache-aware request must not be worth a call, calls=%d", cm.calls)
	}
	if schema.MessageText(req.Input[1]) != body {
		t.Fatal("a suppressed candidate must be left verbatim (fail open)")
	}
}

// The shipping guard, end to end and through the PRESETS: no default configuration may
// run extract_llm on a prompt-caching backend. The unit test in components/offload covers
// evaluateGate's hard decline directly; this one covers the wiring, so a rebase that drops
// `allow_on_caching_backend` from the config struct or forgets to thread `allowCached`
// into the gate fails here rather than shipping silently.
//
// The output is deliberately HUGE (far above the ~30,500-token cached break-even), so the
// economics alone would permit the call — only the hard decline can suppress it.
func TestNoDefaultConfigRunsExtractLLMOnCachingBackend(t *testing.T) {
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	pad := strings.Repeat("padding ", 30_000) // ~240k tokens: economics pass on their own
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	// Every default that reaches extract_llm: the bare component, plus each preset's tuned
	// config. None of them may spend on caching traffic.
	cfgs := map[string]string{
		"defaults": "strategy: code\nmodel:\n  source: config\n",
		"codesmart": "strategy: code\nmodel:\n  source: config\nmin_tokens: 3000\n" +
			"trigger:\n  min_request_tokens: 3000\nllm_every_n_requests: 1\nllm_max_per_request: 4\n",
		// housellm sets allow_on_caching_backend: TRUE, and passes anyway — which is the
		// reason to have it here. The flag lifts exactly one check, and with per_output:false
		// the only path into the gate is the cold sweep, whose branch returns cached:false by
		// construction. So `per_output: false` is the brake, not the flag and not the economic
		// gate. If a future edit turns per_output on while leaving the flag set, this case
		// fails and names the combination our own numbers price as net-negative.
		"housellm": "strategy: code\nper_output: false\nallow_on_caching_backend: true\n" +
			"economic_gate: true\nmodel:\n  source: config\nmin_tokens: 3000\n" +
			"aggressiveness: medium\ncontext: recent\ncontext_messages: 2\n" +
			"trigger:\n  min_request_tokens: 20000\nllm_every_n_requests: 1\n" +
			"llm_max_per_request: 4\nllm_max_per_session: 0\n" +
			"cold_cache:\n  enabled: true\n  max_calls: 20\n  min_tokens: 3000\n",
	}
	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			off := newComp(t, "extract_llm", cfg)
			cm := &countingModel{resp: filter}
			req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
				userMsg("find the keep records"), toolMsg(body),
			}}
			c := &components.Ctx{Ctx: context.Background(), Session: "s1",
				Store: store.NewMemory(store.Options{}), Model: components.ModelSpec{Static: cm},
				CacheAware: true, MaxCachedIdx: -1, CtxWindow: 1_000_000}
			var rep components.Report
			if _, err := off.Offload(req, &rep, c); err != nil {
				t.Fatal(err)
			}
			if cm.calls != 0 {
				t.Fatalf("a default config must NOT call the LLM on a caching backend "+
					"(measured net-negative), calls=%d", cm.calls)
			}
			if schema.MessageText(req.Input[1]) != body {
				t.Fatal("a declined candidate must be left verbatim (fail open)")
			}
		})
	}

	// And the escape hatch still works, so the guard is a default and not a wall.
	off := newComp(t, "extract_llm", "strategy: code\nmodel:\n  source: config\n"+
		"allow_on_caching_backend: true\ntrigger:\n  min_request_tokens: 1\n")
	cm := &countingModel{resp: filter}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	if _, err := off.Offload(req, &components.Report{}, &components.Ctx{
		Ctx: context.Background(), Session: "s2", Store: store.NewMemory(store.Options{}),
		Model: components.ModelSpec{Static: cm}, CacheAware: true, MaxCachedIdx: -1,
		CtxWindow: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if cm.calls == 0 {
		t.Fatal("allow_on_caching_backend: true must hand control back to the economics")
	}
}

// Backward compatibility: an existing config that pins min_tokens must keep working
// unchanged — the smarter trigger is the DEFAULT only when nothing was configured.
func TestExplicitMinTokensConfigStillReduces(t *testing.T) {
	// A pinned min_tokens plus the pre-#28 gate setting reproduces old behavior exactly.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	// A tiny context window would make the derived trigger decline; an explicit
	// min_tokens must override that.
	c := &components.Ctx{Ctx: context.Background(), Session: "s1", Store: st,
		Model: components.ModelSpec{Static: cm}, CtxWindow: 1_000_000}
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("explicit min_tokens must still reduce (skipped=%v calls=%d)", rep.Skipped, cm.calls)
	}
}

// A CROSS-SESSION global hit must not be spliced into the provider's cached prefix.
// The same-session replay path is safe at any depth because this session already sent
// those compacted bytes — the cached prefix holds the COMPACTED form. A global hit is
// different: THIS session never compacted this message, so the cached prefix holds the
// ORIGINAL, and splicing at depth mutates already-cached content and forces a suffix
// re-write at 11.5x the read price. Same churn #40's tail gate exists to prevent, just
// sourced from a cache rather than a model call.
func TestGlobalCacheHitIsNotSplicedAtDepth(t *testing.T) {
	cfg := "strategy: code\nmin_tokens: 1\nallow_on_caching_backend: true\neconomic_gate: false\n" +
		"model:\n  source: config\ntrigger:\n  min_request_tokens: 1\n"
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	// Session A populates the GLOBAL cache from its own tail.
	offA := newComp(t, "extract_llm", cfg)
	cmA := &countingModel{resp: filter}
	reqA := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	var repA components.Report
	if _, err := offA.Offload(reqA, &repA, &components.Ctx{Ctx: context.Background(),
		Session: "A", Store: st, Model: components.ModelSpec{Static: cmA},
		CacheAware: true, MaxCachedIdx: -1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(reqA.Input[1]) == body {
		t.Fatal("session A must compact (fixture no longer exercises the path)")
	}

	// Session B sees the SAME content, but at DEPTH (MaxCachedIdx >= 1). B never compacted
	// it, so B's provider prefix holds the ORIGINAL — the global hit must NOT be applied.
	offB := newComp(t, "extract_llm", cfg)
	cmB := &countingModel{resp: filter}
	reqB := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body), toolMsg("tail"),
	}}
	var repB components.Report
	if _, err := offB.Offload(reqB, &repB, &components.Ctx{Ctx: context.Background(),
		Session: "B", Store: st, Model: components.ModelSpec{Static: cmB},
		CacheAware: true, MaxCachedIdx: 1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(reqB.Input[1]); got != body {
		t.Fatalf("a cross-session global hit must leave a cached-prefix message VERBATIM:\n got=%q", got)
	}
	// The depth message must not have been re-derived either: a global miss falls through to
	// the normal tail-gated flow, never to a depth-lifted model call (#40's stance). B's own
	// tail message (Input[2]) may legitimately be compacted, so assert on the DEPTH message
	// rather than on the call count — the marker is what proves Input[1] was left alone.

	// In the TAIL, the same global hit IS applied — and without a model call, which is the
	// whole point of the cross-session cache.
	offC := newComp(t, "extract_llm", cfg)
	cmC := &countingModel{resp: filter}
	reqC := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	var repC components.Report
	if _, err := offC.Offload(reqC, &repC, &components.Ctx{Ctx: context.Background(),
		Session: "C", Store: st, Model: components.ModelSpec{Static: cmC},
		CacheAware: true, MaxCachedIdx: -1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(reqC.Input[1]) == body {
		t.Fatal("a tail-position global hit must be applied")
	}
	if cmC.calls != 0 {
		t.Fatalf("a global hit must avoid the model call, got %d", cmC.calls)
	}
}
