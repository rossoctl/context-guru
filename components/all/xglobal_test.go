package all_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
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

// housellmExtractLLM returns the extract_llm block EXACTLY as the housellm preset ships it,
// so the guard above tests the shipped configuration instead of a transcription of it.
func housellmExtractLLM(t *testing.T) string {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: housellm\n"))
	if err != nil {
		t.Fatalf("load housellm preset: %v", err)
	}
	node, ok := cfg.Components["extract_llm"]
	if !ok {
		t.Fatal("housellm preset no longer configures extract_llm; this guard is testing nothing")
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatalf("marshal extract_llm block: %v", err)
	}
	return string(raw)
}

// TestHousellmColdSweepActuallyFires is the other half of
// TestDefaultConfigsSpendOnlyOnTheUncachedTail replaces
// TestNoDefaultConfigRunsExtractLLMOnCachingBackend, whose premise this change deliberately
// reverses. That test asserted a default config makes NO call on a caching backend even for a
// candidate its own comment identified as being in the tail. The reason it could assert that
// was a mis-pricing, not a measurement: savedTokenValue reported `cached: true` for the whole
// request, so a tail candidate — content being written INTO the cache on this very turn, at
// 1.25x fresh — was valued at the cache-READ rate, 12.5x too low. The ~30,500-token
// break-even quoted alongside it is explicitly the CACHED break-even. Together they read as
// "extraction cannot pay on a caching backend", which is true at depth and was never
// established for the tail.
//
// So the decision this pins is now positional, which is the honest form of it:
//
//	at DEPTH — inside the live cached prefix — a default must still not spend. Removing
//	  cached content saves the read rate and forces a suffix re-write on top.
//	in the TAIL, a default MAY spend, and must, when the economics pass.
//
// What is NOT relaxed is the economic gate itself: see
// TestTheTailIsStillGatedOnItsOwnEconomics, which is the other half of this and the reason
// this is a re-pricing rather than an opening of the floodgates.
func TestDefaultConfigsSpendOnlyOnTheUncachedTail(t *testing.T) {
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	pad := strings.Repeat("padding ", 30_000) // ~240k tokens: economics pass on their own
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	cfgs := map[string]string{
		"defaults": "strategy: code\nmodel:\n  source: config\n",
		"codesmart": "strategy: code\nmodel:\n  source: config\nmin_tokens: 3000\n" +
			"trigger:\n  min_request_tokens: 3000\nllm_every_n_requests: 1\nllm_max_per_request: 4\n",
		// Read from config.presetConfigs rather than copied, because a copy cannot catch the
		// drift this test exists to catch.
		"housellm": housellmExtractLLM(t),
	}
	// The tool output sits at index 1, so MaxCachedIdx 1 puts it inside the cached prefix and
	// MaxCachedIdx 0 leaves it in the tail. One number is the whole difference.
	for _, pos := range []struct {
		name         string
		maxCachedIdx int
		wantCall     bool
	}{
		{"at depth, inside the cached prefix", 1, false},
		{"in the uncached tail", 0, true},
	} {
		for name, cfg := range cfgs {
			t.Run(pos.name+"/"+name, func(t *testing.T) {
				off := newComp(t, "extract_llm", cfg)
				cm := &countingModel{resp: filter}
				req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
					userMsg("find the keep records"), toolMsg(body),
				}}
				c := &components.Ctx{Ctx: context.Background(), Session: "s1",
					Store: store.NewMemory(store.Options{}), Model: components.ModelSpec{Static: cm},
					// 75k, not 1M, and the number is measured rather than picked: this request is
					// 60,026 tokens (schema.MessagesTokens). With no explicit min_tokens the
					// `defaults` config decides on context PRESSURE, and against a 1M window
					// 60k is 0.06 — far under the 0.25 bar — so it declined for a reason with
					// nothing to do with caching, and the depth case passed without exercising
					// anything. 75k puts pressure at 0.80, past the 0.60 bar that fires on
					// pressure alone, so position is the only variable left.
					CacheAware: true, MaxCachedIdx: pos.maxCachedIdx, CtxWindow: 75_000}
				var rep components.Report
				if _, err := off.Offload(req, &rep, c); err != nil {
					t.Fatal(err)
				}
				if got := cm.calls > 0; got != pos.wantCall {
					if pos.wantCall {
						t.Fatalf("a 240k-token candidate in the UNCACHED tail must be worth a "+
							"call — it is billed at the cache-write rate, not the read rate. "+
							"calls=%d gates=%v", cm.calls, rep.Gates)
					}
					t.Fatalf("a default config must NOT spend on content inside the live cached "+
						"prefix (read-rate saving, plus a forced suffix re-write), calls=%d", cm.calls)
				}
			})
		}
	}
}

// TestHousellmColdSweepActuallyFires is the other half of
// TestNoDefaultConfigRunsExtractLLMOnCachingBackend, and it exists because the preset
// shipped for a day in a state where BOTH halves were silent.
//
// Every extraction call this service has ever made was a cold one, so cold_cache.min_tokens
// is the single knob deciding whether extract_llm does anything at all. It was 3000, and at
// 3000 production recorded `below_output_floor` on all 36 sweeping turns and zero
// extractions across 3,437 requests — the component was configured into a no-op while
// looking fully enabled. A candidate of ~1,500 tokens is the size that regression turned
// away, so that is what this asserts on: the preset, not a copy of it, must call the model
// on a cold turn.
//
// Raising the preset's cold floor above ~1,500 fails this; re-adding
// allow_on_caching_backend fails the warm guard above. The pair pins the economics from
// both sides.
func TestHousellmColdSweepActuallyFires(t *testing.T) {
	off := newComp(t, "extract_llm", housellmExtractLLM(t))
	// ~1,500 tokens of the noise the sweep is meant to reduce, with a filterable shape.
	body := `[`
	for i := 0; i < 120; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"id":` + strconv.Itoa(i) + `,"name":"record ` + strings.Repeat("payload ", 6) + `"}`
	}
	body += `]`
	cm := &countingModel{resp: "data = json.decode(INPUT)\nOUTPUT = json.encode(data[:2])\n"}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("summarize the records"), toolMsg(body),
	}}
	// ColdCache is what makes this a sweep: the prefix TTL has expired, so the whole
	// transcript is about to be re-billed at the write rate and savedTokenValue reports
	// cached:false — which is why the warm guard's decline does not apply here.
	c := &components.Ctx{Ctx: context.Background(), Session: "cold1",
		Store: store.NewMemory(store.Options{}), Model: components.ModelSpec{Static: cm},
		CacheAware: true, ColdCache: true, MaxCachedIdx: -1, CtxWindow: 1_000_000}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if cm.calls == 0 {
		t.Fatalf("the housellm preset made NO model call on a cold turn with a ~1.5k-token "+
			"candidate — the component is configured into a no-op. gates=%v", rep.Gates)
	}
}

// TestHousellmDoesNotAttemptTheTailBelowBreakEven pins the floor that makes the warm/tail
// path safe, from the loss side. TestDefaultConfigsSpendOnlyOnTheUncachedTail shows a huge
// tail candidate IS worth a call now; this shows a realistically-sized one is not, and that
// the preset refuses it before spending anything.
//
// The number is measured, not chosen. On real warm sessions through a local proxy an
// extraction call cost ~$0.0193 per ACCEPTED result — output-dominated, and including the
// 1-in-5 rejected outright that pays full price for nothing. At the cache-write rate that
// needs ~4,060 saved tokens, and the observed reduction on accepted tail calls was ~65%, so
// the candidate must be ~6,250 tokens before a call can repay itself. The preset's floor is
// 8,000.
//
// At a 1,000 floor the same sessions made 5 warm calls, accepted 4, saved 8,718 tokens for
// $0.0771 — net -$0.036 — and every one of those calls was permitted by the EXPLORATION
// budget rather than by the arithmetic. That is what this floor exists to prevent, and it is
// why the assertion is on below_output_floor specifically: the floor must refuse the
// candidate BEFORE the exploration allowance can spend on it.
func TestHousellmDoesNotAttemptTheTailBelowBreakEven(t *testing.T) {
	off := newComp(t, "extract_llm", housellmExtractLLM(t))
	// ~4,000 tokens of log lines: comfortably above the OLD 1,000 floor, below break-even,
	// and in the tail. This is the shape that lost money.
	body := strings.Repeat("2024-01-01 GET /users/42 200 12ms handler=src/api/users.py\n", 330)
	cm := &countingModel{resp: "data = json.decode(INPUT)\nOUTPUT = json.encode(data[:1])\n"}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the slow handler"), toolMsg(body),
	}}
	c := &components.Ctx{Ctx: context.Background(), Session: "warm-tail",
		Store: store.NewMemory(store.Options{}), Model: components.ModelSpec{Static: cm},
		CacheAware: true, MaxCachedIdx: 0, CtxWindow: 1_000_000}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if cm.calls != 0 {
		t.Fatalf("the preset spent a call on a ~4k-token tail candidate, which measured a "+
			"net LOSS of $0.036 across five such calls; the hot floor must refuse it. "+
			"calls=%d gates=%v", cm.calls, rep.Gates)
	}
	if rep.Gates["below_output_floor"] == 0 {
		t.Fatalf("expected below_output_floor to be what refused it — anything else means the "+
			"exploration budget could still spend here. gates=%v", rep.Gates)
	}
}
