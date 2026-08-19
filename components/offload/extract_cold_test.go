package offload

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// The cold-cache sweep. Measured motivation, on this deployment over 1.4 days: turns whose
// prompt cache had expired were 219 of 5,596 requests (4%) but $360 of $1,173 of spend
// (31%) — $1.64 each against $0.144 for a warm turn — because all 56.7M of their tokens
// billed as cache_creation. The shipped pipeline saved 0.015% of that.
//
// Two things are true only on such a turn, and both are load-bearing below:
//   - rewriting deep history is free (there is no live cached prefix to invalidate);
//   - a removed token is worth the cache-WRITE rate, 12.5x its warm-turn value.

// coldReq builds a transcript whose BIG tool output sits at depth (index 1), well before
// the cached-prefix boundary, so only a sweep can reach it.
func coldReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		toolResultMsg(strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py\n", 700)),
		assistantMsg("Read the file next."),
		toolResultMsg(strings.Repeat("filler line to grow the transcript\n", 50)),
		userMsg("keep going"),
	}}
}

func coldCtx(session string, cold bool, idleMs int64, model components.Model) *components.Ctx {
	return &components.Ctx{
		Session: session, Ctx: context.Background(),
		Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
		// CacheAware with the boundary AFTER the big output: index 1 is inside the cached
		// prefix, so the tail gate blocks it on a warm turn.
		CacheAware: true, MaxCachedIdx: 3,
		ColdCache: cold, IdleMs: idleMs,
		Model: components.ModelSpec{Static: model, Incoming: model},
	}
}

// The core of the feature: on a cold turn the sweep reaches a tool output at DEPTH that the
// tail gate blocks on a warm turn. Without the sweep, the warm arm records cached_prefix and
// makes no call — which is correct there and is what leaves the cold turn's money on the
// table today.
//
// PROVEN TO FAIL WITHOUT THE CHANGE: with `!sweeping` removed from the tail-gate condition,
// the cold subtest reports calls=0 and gates map[cached_prefix:2 cached_prefix_above_floor:1].
func TestColdSweepReachesDepthThatAWarmTurnCannot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yaml     string
		cold     bool
		wantCall bool
		wantGate string
	}{
		{
			name:     "warm turn: the tail gate protects the cached prefix",
			yaml:     "cold_cache:\n  enabled: true\nstrategy: code\neconomic_gate: false\n",
			cold:     false,
			wantCall: false,
			wantGate: "cached_prefix",
		},
		{
			name:     "cold turn: the prefix is gone, so depth is fair game",
			yaml:     "cold_cache:\n  enabled: true\nstrategy: code\neconomic_gate: false\n",
			cold:     true,
			wantCall: true,
		},
		{
			name:     "cold turn with the sweep disabled changes nothing",
			yaml:     "strategy: code\neconomic_gate: false\n",
			cold:     true,
			wantCall: false,
			wantGate: "cached_prefix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &silentModel{}
			e := newSizeComponent(t, model, tc.yaml)
			rep := &components.Report{}
			c := coldCtx("cold-"+tc.name, tc.cold, 3_600_000, model)
			if _, err := e.Offload(coldReq(), rep, c); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			calls := atomic.LoadInt64(&model.calls)
			if tc.wantCall != (calls > 0) {
				t.Fatalf("calls=%d, wantCall=%v (gates: %v)", calls, tc.wantCall, rep.Gates)
			}
			if tc.wantGate != "" && rep.Gates[tc.wantGate] == 0 {
				t.Fatalf("expected the %s gate, got %v", tc.wantGate, rep.Gates)
			}
		})
	}
}

// A cold turn's tokens are re-billed as cache CREATION at 1.25x fresh, so a removed token is
// worth 12.5x its warm-turn value. Getting this backwards is what would make the gate
// suppress the one case that pays best.
func TestColdTurnPricesTokensAtTheWriteRate(t *testing.T) {
	warm := savedTokenValue(&components.Ctx{CacheAware: true})
	cold := savedTokenValue(&components.Ctx{CacheAware: true, ColdCache: true})
	fresh := savedTokenValue(&components.Ctx{})

	if !warm.cached {
		t.Fatal("a warm cache-aware turn must price at the cache-read rate")
	}
	if cold.cached {
		t.Fatal("a cold turn must not be treated as cached; that applies the 10x haircut " +
			"to the one case where saving is worth MORE than fresh input")
	}
	if !(cold.perToken > fresh.perToken && fresh.perToken > warm.perToken) {
		t.Fatalf("expected cold > fresh > warm per-token value, got cold=%g fresh=%g warm=%g",
			cold.perToken, fresh.perToken, warm.perToken)
	}
	if ratio := cold.perToken / warm.perToken; ratio < 12 || ratio > 13 {
		t.Fatalf("cold/warm token value ratio is %.2f, want ~12.5 (1.25x fresh vs 0.1x fresh)", ratio)
	}
}

// min_idle_seconds may only RAISE the bar. The TTL check is the correctness condition —
// whether the cache is actually gone — and this is extra caution on top of it.
func TestColdSweepMinIdleRaisesTheBar(t *testing.T) {
	e := newSizeComponent(t, &silentModel{},
		"cold_cache:\n  enabled: true\n  min_idle_seconds: 1800\nstrategy: code\n")
	x := e
	for _, tc := range []struct {
		name   string
		cold   bool
		idleMs int64
		want   bool
	}{
		{"cold but only 10 minutes idle", true, 600_000, false},
		{"cold and an hour idle", true, 3_600_000, true},
		{"warm, however long idle", false, 7_200_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := x.sweepThisRequest(coldCtx("x", tc.cold, tc.idleMs, nil))
			if got != tc.want {
				t.Fatalf("sweepThisRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE SWEEP MUST NOT FORCE `context: full`. It used to, and that one line was the largest
// single cost in this component: `full` renders the whole request (measured 138,596 context
// tokens on a 138,341-token request), once per candidate, so the break-even removal at k=4
// was 113,286 tokens — more than the transcript holds — against 6,833 under `recent`.
//
// The original justification was that a full transcript is needed to judge what an old
// message may lose. It was tested and it is the keep-list, not the context, that carries
// that: a full-transcript context took acceptance from 3/4 to 0/6 because every unique token
// in the noise became a required identifier, and HarvestIdentifiers now reads ctxRecent
// explicitly, so the two concerns are separate. Measured on bench/cold.jsonl (8 requests,
// coldness verified by cache_read=0), `full` spent $0.0387 on a 36,686-token prompt to
// remove 0 tokens.
//
// So the configured mode governs on a sweep exactly as it does on a warm turn, and an
// operator who wants the old behaviour writes `context: full`.
func TestColdSweepHonoursTheConfiguredContextMode(t *testing.T) {
	goalOnly := newSizeComponent(t, &silentModel{},
		"context: goal\ncold_cache:\n  enabled: true\nstrategy: code\n")
	if warm, swept := goalOnly.extractionContext(ctxReq(), false), goalOnly.extractionContext(ctxReq(), true); warm != swept {
		t.Fatalf("sweep overrode context: goal (%d bytes warm, %d bytes swept)", len(warm), len(swept))
	}
	if s := goalOnly.extractionContext(ctxReq(), true); strings.Contains(s, "file body line") {
		t.Fatal("sweep included tool output under context: goal, so it is still rendering the transcript")
	}
	// `full` still means full, on the sweep and off it — the escape hatch has to work.
	full := newSizeComponent(t, &silentModel{},
		"context: full\ncold_cache:\n  enabled: true\nstrategy: code\n")
	if s := full.extractionContext(ctxReq(), true); !strings.Contains(s, "file body line") {
		t.Fatal("context: full excluded tool output, so the escape hatch no longer renders the transcript")
	}
}

// The two paths are switched independently, so a sweep must not drain the hot path's
// per-session allowance (or the reverse) depending on which happened to fire first.
func TestSweepDoesNotConsumeTheSessionBudget(t *testing.T) {
	model := &silentModel{}
	e := newSizeComponent(t, model, "fire_on: size\nmin_tokens: 500\nstrategy: code\n"+
		"economic_gate: false\nllm_max_per_session: 1\ncold_cache:\n  enabled: true\n")

	// One cold sweep first.
	rep := &components.Report{}
	if _, err := e.Offload(coldReq(), rep, coldCtx("shared", true, 3_600_000, model)); err != nil {
		t.Fatal(err)
	}
	afterSweep := atomic.LoadInt64(&model.calls)
	if afterSweep == 0 {
		t.Fatalf("the sweep made no call (gates: %v)", rep.Gates)
	}

	// Then a warm turn on the SAME session, with distinct content so no cache answers it.
	rep2 := &components.Report{}
	req := coldReq()
	req.Input[1] = toolResultMsg(strings.Repeat("distinct warm output line\n", 700) + "x")
	c := coldCtx("shared", false, 0, model)
	c.MaxCachedIdx = 0 // put the big output in the tail so only the budget can stop it
	if _, err := e.Offload(req, rep2, c); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&model.calls) == afterSweep {
		t.Fatalf("the warm turn made no call after a sweep: the sweep consumed the "+
			"per-session budget it is not supposed to touch (gates: %v)", rep2.Gates)
	}
}

// A component configured to do nothing at all is a configuration error, not a silent no-op
// sitting in someone's pipeline looking enabled.
func TestPerOutputFalseWithoutColdSweepIsRejected(t *testing.T) {
	if _, err := newExtractLLM([]byte("per_output: false\n")); err == nil {
		t.Fatal("per_output: false with cold_cache disabled was accepted")
	}
	if _, err := newExtractLLM([]byte("per_output: false\ncold_cache:\n  enabled: true\n")); err != nil {
		t.Fatalf("sweep-only configuration rejected: %v", err)
	}
}

// per_output: false must leave the hot path alone while still sweeping. This is the
// configuration the deployment is expected to use first, since the sweep is the half whose
// economics are unambiguous.
func TestSweepOnlyConfigurationSkipsWarmTurns(t *testing.T) {
	model := &silentModel{}
	e := newSizeComponent(t, model, "per_output: false\nstrategy: code\neconomic_gate: false\n"+
		"cold_cache:\n  enabled: true\n")

	rep := &components.Report{}
	c := coldCtx("sweep-only", false, 0, model)
	c.MaxCachedIdx = 0 // the big output is in the tail: nothing but per_output can stop it
	if _, err := e.Offload(coldReq(), rep, c); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&model.calls); got != 0 {
		t.Fatalf("a warm turn made %d call(s) with per_output: false", got)
	}
	if rep.Gates["per_output_disabled"] == 0 {
		t.Fatalf("no per_output_disabled gate, so the skip is undiagnosable: %v", rep.Gates)
	}

	rep2 := &components.Report{}
	if _, err := e.Offload(coldReq(), rep2, coldCtx("sweep-only", true, 3_600_000, model)); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatalf("the cold turn made no call either, so the component does nothing at all: %v",
			rep2.Gates)
	}
}

// The sweep's own cap bounds one turn's calls, and the refusal is countable.
func TestColdSweepCapIsItsOwn(t *testing.T) {
	model := &silentModel{}
	e := newSizeComponent(t, model, "strategy: code\neconomic_gate: false\n"+
		"llm_max_per_request: 1\ncold_cache:\n  enabled: true\n  max_calls: 2\n  min_tokens: 100\n")

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout and fix it."),
	}}
	for i := 0; i < 5; i++ {
		req.Input = append(req.Input,
			toolResultMsg(strings.Repeat("log line for candidate "+strconv.Itoa(i)+"\n", 200)))
	}
	req.Input = append(req.Input, userMsg("keep going"))

	rep := &components.Report{}
	if _, err := e.Offload(req, rep, coldCtx("cap", true, 3_600_000, model)); err != nil {
		t.Fatal(err)
	}
	// The sweep cap governs, NOT llm_max_per_request (which is 1).
	if got := atomic.LoadInt64(&model.calls); got != 2 {
		t.Fatalf("made %d calls, want the sweep's cap of 2 (gates: %v)", got, rep.Gates)
	}
	if rep.Gates["over_cold_sweep_cap"] == 0 {
		t.Fatalf("the cap dropped candidates with no gate recorded: %v", rep.Gates)
	}
	if rep.Gates["over_per_request_cap"] != 0 {
		t.Fatal("the hot path's per-request cap was applied to a sweep")
	}
}

// overlapModel records whether any two calls were ever in flight at once, and how many ran.
type overlapModel struct {
	mu       sync.Mutex
	inFlight int
	overlaps int
	calls    int
}

func (m *overlapModel) Complete(context.Context, string) (string, error) {
	m.mu.Lock()
	m.calls++
	m.inFlight++
	if m.inFlight > 1 {
		m.overlaps++
	}
	m.mu.Unlock()
	time.Sleep(5 * time.Millisecond) // wide enough for a sibling to arrive if one is coming
	m.mu.Lock()
	m.inFlight--
	m.mu.Unlock()
	return "", nil
}

// THE CACHE WRITE HAS TO BE EARNED BEFORE ANYTHING CAN READ IT. `CacheContext = len(cands) > 1`
// moves the conversation context into a cacheable system block, but cheapmodel.claimCacheWrite
// deliberately withholds the breakpoint from CONCURRENT siblings — a cache entry that is only
// ever written costs 1.25x fresh and buys nothing. So with llmConcurrency = 4 the first call
// took the write slot and calls 2..4 sent no mark and paid plain fresh input for the identical
// context. Measured on production: five haiku calls on ONE request each sent ~138,000 prompt
// tokens with cache_read = 0 AND cache_write = 0.
//
// The sweep therefore issues its first call ALONE, then the rest concurrently. At T = 180k,
// k = 4 that moves break-even removal from 198,620 tokens to 79,088.
//
// The warm per-output path stays fully concurrent: serializing costs a whole gateway queue
// round (~2-4 s p50, tail 12-16 s — latency here is queue time, not prompt size), which is
// worth paying only on a turn whose entire transcript is being re-billed at 1.25x fresh.
func TestSweepEarnsTheContextCacheWriteBeforeReadingIt(t *testing.T) {
	// Two big outputs at depth, so the sweep has k >= 2 and CacheContext is on. The salt keeps
	// the two subtests' content DISTINCT: the extraction result cache and the seen-content
	// ledger are process-wide, so reusing bytes makes the second subtest answer from state and
	// make no calls at all.
	req := func(salt string) *bschemas.BifrostChatRequest {
		return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("Find the auth timeout in src/api/users.py and fix it."),
			toolResultMsg(strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py "+salt+"\n", 700)),
			assistantMsg("Now the second log."),
			toolResultMsg(strings.Repeat("2024-01-02 POST /login 500 31ms src/api/auth.py "+salt+"\n", 700)),
			userMsg("keep going"),
		}}
	}
	run := func(t *testing.T, yaml string, cold bool) *overlapModel {
		t.Helper()
		m := &overlapModel{}
		e := newSizeComponent(t, m, yaml)
		c := coldCtx("earn-"+t.Name(), cold, 3_600_000, m)
		c.MaxCachedIdx = 0 // both outputs in the tail, so only the sweep/gate can stop them
		rep := &components.Report{}
		if _, err := e.Offload(req(t.Name()), rep, c); err != nil {
			t.Fatal(err)
		}
		if m.calls < 2 {
			t.Fatalf("fixture made %d calls, need >= 2 for the write/read split to exist (gates: %v)",
				m.calls, rep.Gates)
		}
		return m
	}

	t.Run("sweep serializes the writer", func(t *testing.T) {
		m := run(t, "per_output: false\nstrategy: code\neconomic_gate: false\n"+
			"cold_cache:\n  enabled: true\n  min_tokens: 500\n", true)
		// The FIRST call must have run alone. With k=2 that means no overlap at all; with
		// more, the readers may overlap each other but never the writer.
		if m.overlaps > m.calls-2 {
			t.Fatalf("%d overlapping calls across %d: the first call did not run alone, so "+
				"the readers cannot have read what nothing had written yet", m.overlaps, m.calls)
		}
	})

	t.Run("warm per-output path stays concurrent", func(t *testing.T) {
		m := run(t, "fire_on: size\nmin_tokens: 500\nstrategy: code\neconomic_gate: false\n"+
			"cold_cache:\n  enabled: true\n", false)
		if m.overlaps == 0 {
			t.Fatalf("no overlap across %d warm calls: the hot path was serialized too, and it "+
				"pays a whole gateway queue round (~2-4s p50) for a fraction of a cent", m.calls)
		}
	})
}
