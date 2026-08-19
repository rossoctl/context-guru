package all_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// varyingModel returns a DIFFERENT valid projection on each call — exactly what a SAMPLED
// model may do. cheapmodel sends no temperature and no seed, so extract_llm's replacement
// text is not reproducible.
//
// Each variant is a DERIVED projection (a prefix of INPUT of a varying length), not invented
// text: the acceptance gate now requires the result to derive from the input, so a stub that
// answers with a string absent from the body is refused before this fixture's real subject —
// what happens to a LOST decision at depth — is reached.
type varyingModel struct {
	mu sync.Mutex
	n  int
}

func (m *varyingModel) Complete(context.Context, string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return "OUTPUT = INPUT[:" + strconv.Itoa(200+m.n) + "]\n", nil
}

func (m *varyingModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// A LOST extract_llm decision must NOT be re-derived inside the provider's cached prefix.
// Re-deriving costs a sampled model call that can emit DIFFERENT bytes at depth — the very
// corruption the repair was meant to prevent — and it buys nothing: if the bytes differ the
// suffix is cache-written either way, so the model call is pure loss. The message is
// therefore left verbatim, like any other tail-gated miss.
//
// Only mask/failed_run get the depth repair, because their replacement is a pure function
// of (content, config) and so is genuinely reproducible.
func TestLostExtractLLMResultIsNotReDerivedAtDepth(t *testing.T) {
	// allow_on_caching_backend + economic_gate: false keep this fixture exercising the
	// CACHE-SAFETY path under test. extract_llm is off by default on caching backends and
	// its gate declines small outputs, so without these turn 1 never compacts and the guard
	// passes vacuously — which is the same trap #40 hit with the wrong MaxCachedIdx.
	off := newComp(t, "extract_llm",
		"strategy: code\nmin_tokens: 1\nallow_on_caching_backend: true\neconomic_gate: false\n"+
			"model:\n  source: config\ntrigger:\n  min_request_tokens: 1\n")
	now := time.Unix(0, 0)
	st := store.NewMemory(store.Options{TTLSeconds: 10})
	st.SetClock(func() time.Time { return now })
	vm := &varyingModel{}
	body := strings.Repeat("verbose tool output line\n", 60)

	// The message under test is Input[1]; MaxCachedIdx >= 1 puts it in the CACHED PREFIX.
	run := func(maxCached int) string {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("find the keep records in output.txt"), toolMsg(body), toolMsg("tail"),
		}}
		c := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st,
			Model: components.ModelSpec{Static: vm}, CacheAware: true,
			MaxCachedIdx: maxCached, CtxWindow: 200000}
		var rep components.Report
		if _, err := off.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return schema.MessageText(req.Input[1])
	}

	first := run(-1) // tail turn: a NEW compaction is allowed and gets cached
	if first == body {
		t.Fatal("turn 1 must compact the tail output (fixture no longer exercises the path)")
	}
	callsAfterFirst := vm.calls()

	// Force the loss, then re-present the message at DEPTH.
	now = now.Add(11 * time.Second)
	got := run(1)

	if got != body {
		t.Fatalf("a lost LLM decision must leave the cached-prefix message VERBATIM, not "+
			"re-project it with freshly sampled bytes:\n turn1=%q\n turn2=%q", first, got)
	}
	if vm.calls() > callsAfterFirst+1 {
		t.Fatalf("no re-derivation call for the depth message (calls %d -> %d)",
			callsAfterFirst, vm.calls())
	}
}
