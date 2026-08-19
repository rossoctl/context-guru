package apply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// TestTriggerCounterfactual answers ONE question with measurement: production reports
// 7,660 requests / 380.7M tokens / $744.62 as `below_trigger`, so would LIFTING the size
// floors have removed anything on that traffic?
//
// It replays one capture through the deployed deterministic pipeline and through the same
// pipeline with every size floor dropped to 1, then reports, per request class, what the
// second removed that the first did not. `below_trigger` is reproduced with dash's own
// rule (dash/event.go uncompressedReason): no component mutated and nothing was saved.
//
// Deterministic on purpose — no LLM component, so the table is reproducible, free, and
// attributes nothing to a model call. extract_llm's own economics are measured separately
// (it lost $10.14 net in production).
//
//	CONTEXT_GURU_CAPTURE=/path/capture.jsonl CG_TRIGGER_CF=1 \
//	  go test ./apply -run TriggerCounterfactual -v
//
// CG_TRIGGER_CF_MAX caps the request count. CG_TRIGGER_CF_IN_RATE is the capture model's
// real $/MTok (default 1.52, ete-litellm aws/claude-sonnet-5).
func TestTriggerCounterfactual(t *testing.T) {
	if os.Getenv("CG_TRIGGER_CF") == "" {
		t.Skip("set CG_TRIGGER_CF=1 to run the trigger counterfactual")
	}
	max := 0
	fmt.Sscan(os.Getenv("CG_TRIGGER_CF_MAX"), &max)
	recs := loadCapture(t, max)
	fresh := sweepRate("CG_TRIGGER_CF_IN_RATE", 1.52)

	// Arm A is what production runs (codesmart minus the LLM call). Arm B lifts every
	// size floor those components own — that IS "the trigger lifted" on the deterministic
	// path, because no deployed component except extract_llm consults Trigger.Fires.
	const armA = `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
`
	const armB = `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  format:
    min_tokens: 1
  toon:
    min_tokens: 1
  dedup:
    min_tokens: 1
  failed_run:
    min_tokens: 1
  cmdfilter:
    min_size: 1
  extract:
    min_tokens: 1
`
	a := replayArm(t, "deployed", armA, recs)
	b := replayArm(t, "floors-lifted", armB, recs)

	fmt.Printf("\ncapture: %d requests   fresh rate: $%.2f/MTok   (cache_read 0.1x, cache_write 1.25x)\n",
		len(recs), fresh)
	fmt.Printf("\n%-26s %5s %12s %12s %11s %11s %11s\n",
		"class (by messages)", "n", "tok_before", "attempted", "rm:deployed", "rm:lifted", "delta")
	var totDelta, totBefore int
	for _, k := range classOrder {
		ra, rb := a[k], b[k]
		if ra == nil {
			continue
		}
		d := rb.removed - ra.removed
		totDelta += d
		totBefore += ra.before
		fmt.Printf("%-26s %5d %12d %12d %11d %11d %11d\n",
			k, ra.n, ra.before, ra.attempted, ra.removed, rb.removed, d)
	}
	fmt.Printf("%-26s %5s %12d %12s %11s %11s %11d\n", "TOTAL", "", totBefore, "", "", "", totDelta)

	// Trap: removed > attempted is CACHE INVALIDATION, not saving. A component that
	// rewrote frozen content shows up as a big "removed" number and a bigger provider
	// bill. Checked per class, because a whole-corpus total hides it.
	for _, k := range classOrder {
		for name, r := range map[string]armResult{"deployed": a, "floors-lifted": b} {
			c := r[k]
			if c != nil && c.removed > c.attempted {
				t.Errorf("%s/%s: removed %d > attempted %d — that is cache invalidation, "+
					"not a saving; do not read the delta above as value", name, k, c.removed, c.attempted)
			}
		}
	}

	// What the lifted arm's EXTRA removal would be worth, at each tier it could be
	// billed at. Only the write tier applies to a token that is genuinely entering the
	// prompt for the first time; a token inside a cached prefix is worth the READ tier,
	// and CHANGING it costs a re-write of everything after it.
	fmt.Printf("\nvalue of the extra removal: %d tokens  @cache_read($%.3f/M)=$%.4f  @cache_write($%.3f/M)=$%.4f\n",
		totDelta, fresh*0.1, float64(totDelta)*fresh*0.1/1e6, fresh*1.25, float64(totDelta)*fresh*1.25/1e6)

	fmt.Printf("\nbelow_trigger reproduction (dash's own rule):\n")
	fmt.Printf("  deployed:      %d/%d requests, %d tokens\n", a.btReqs(), len(recs), a.btTokens())
	fmt.Printf("  floors-lifted: %d/%d requests, %d tokens\n", b.btReqs(), len(recs), b.btTokens())
	fmt.Printf("  requests the lifted floors rescued from below_trigger: %d\n", a.btReqs()-b.btReqs())
}

// classOrder is the message-count banding production is analysed in, so the replay table
// and the DB table line up row for row.
var classOrder = []string{"A 1 msg", "B 2-4 msgs", "C 5-20 msgs", "D 21-100 msgs", "E >100 msgs"}

func msgClass(n int) string {
	switch {
	case n <= 1:
		return classOrder[0]
	case n <= 4:
		return classOrder[1]
	case n <= 20:
		return classOrder[2]
	case n <= 100:
		return classOrder[3]
	}
	return classOrder[4]
}

type armClass struct {
	n, before, attempted, frozen, removed int
	btReqs, btTokens                      int
}

type armResult map[string]*armClass

func (r armResult) btReqs() (n int) {
	for _, c := range r {
		n += c.btReqs
	}
	return
}

func (r armResult) btTokens() (n int) {
	for _, c := range r {
		n += c.btTokens
	}
	return
}

// replayArm replays every captured request once through one config, bucketed by message
// count. One session id and a real Tracker, so the cached-prefix boundary evolves exactly
// as it does in production — replaying each request against a fresh boundary would report
// a freeze state no live turn ever has.
func replayArm(t *testing.T, name, yaml string, recs []capRec) armResult {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatalf("%s build: %v", name, err)
	}
	st := store.NewMemory(store.Options{})
	tracker := modes.NewTracker(0)
	out := armResult{}
	now := time.Now()
	for _, r := range recs {
		res := apply.BodyOpts(context.Background(), pipe, st, apply.Opts{
			Provider: bschemas.ModelProvider(r.Provider),
			Body:     r.Body,
			Session:  "cf-" + name,
			Now:      now,
			Tracker:  tracker,
		})
		if res.Run == nil {
			continue
		}
		k := msgClass(int(gjson.GetBytes(r.Body, "messages.#").Int()))
		c := out[k]
		if c == nil {
			c = &armClass{}
			out[k] = c
		}
		c.n++
		c.before += res.Run.TokensBefore
		c.attempted += res.AttemptedTokens
		c.frozen += res.FrozenTokens
		saved := res.Run.TokensBefore - res.Run.TokensAfter
		c.removed += saved
		// dash/event.go: below_trigger is "nothing saved AND no component mutated".
		mutated := 0
		for _, rep := range res.Run.Components {
			if !rep.Skipped && !rep.Reverted {
				mutated++
			}
		}
		if saved <= 0 && mutated == 0 {
			c.btReqs++
			c.btTokens += res.Run.TokensBefore
		}
	}
	return out
}

// TestBelowTriggerIsNotTheTrigger pins the finding this whole investigation turned on:
// `below_trigger` is reported when NO COMPONENT MUTATED, which on the deployed pipeline
// has nothing to do with components.Trigger — only extract_llm consults Trigger.Fires,
// and it is not in the deterministic default. A request with a transcript that no
// reducer can shrink is therefore reported as "below trigger" while every trigger fired.
//
// Deliberately hermetic: a synthetic 1-message request with no tool_result surface, which
// is the shape 53% of production's gated tokens actually have.
func TestBelowTriggerIsNotTheTrigger(t *testing.T) {
	cfg := pipe(t, `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 1
  dedup:
    min_tokens: 1
  failed_run:
    min_tokens: 1
`)
	p, _ := cfg.Build(nil)
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"` +
		"a plain user question with no tool output anywhere in it" + `"}]}]}`)
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body, Session: "s1",
	})
	if res.Run == nil {
		t.Fatal("no run report")
	}
	// Every floor is 1, so nothing is "below" anything — yet nothing mutates, because
	// there is no tool_result block to reduce.
	for _, rep := range res.Run.Components {
		if !rep.Skipped && !rep.Reverted {
			t.Fatalf("component %s mutated a request with no tool output", rep.Component)
		}
	}
	if got := res.Run.TokensBefore - res.Run.TokensAfter; got != 0 {
		t.Fatalf("removed %d tokens from a request with no reducible surface", got)
	}
}

// deterministicPipeline is the deployed pipeline minus the LLM call: what production
// actually runs on the traffic this file measures.
const deterministicPipeline = `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
`

// actingPipeline is a reducer that demonstrably rewrites this transcript (mask changes 34
// positions on turn 2 below). The freeze assertions have to be made against a component
// that ACTS, or they pass vacuously — the deployed deterministic pipeline finds nothing on
// a synthetic transcript, which is the whole subject of this file.
const actingPipeline = "pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 3, min_tokens: 50}\n"

// TestUpperFrozenModeStaysConservative is the guarantee for the upper mode of the frozen
// distribution (p75 0.995, p90 0.9996). The invariant that actually protects the provider's
// cache is not "no component edits a low index" — a reducer legitimately re-applies its own
// earlier decision to a re-sent message, and Trace.Changes is a diff against the INPUT, not
// against what the provider cached. The invariant is that the BYTES SENT for the cached
// prefix are identical from one turn to the next. That is what a prefix hash compares, so
// that is what this asserts.
func TestUpperFrozenModeStaysConservative(t *testing.T) {
	cfg := pipe(t, actingPipeline)
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	tracker := modes.NewTracker(0)
	o := apply.Opts{Provider: bschemas.Anthropic, Session: bobSession, Tracker: tracker}

	o.Body = compactBody(t, "grow", 30)
	r1 := apply.BodyOpts(context.Background(), p, st, o)
	if r1.Trace.MaxCachedIdx != -1 {
		t.Fatalf("turn 1 MaxCachedIdx = %d, want -1", r1.Trace.MaxCachedIdx)
	}
	// Turn 2 appends to turn 1, so turn 1's messages are now cached and frozen.
	o.Body = compactBody(t, "grow", 40)
	r2 := apply.BodyOpts(context.Background(), p, st, o)
	if want := 32 - 1; r2.Trace.MaxCachedIdx != want { // 2 head + 30 tools
		t.Fatalf("turn 2 MaxCachedIdx = %d, want %d", r2.Trace.MaxCachedIdx, want)
	}
	if r2.Trace.FrozenTokens == 0 {
		t.Fatal("turn 2 froze nothing; this is not the upper frozen mode")
	}
	if len(r2.Trace.Changes) == 0 {
		t.Fatal("the reducer changed nothing; the byte assertion below would be vacuous")
	}
	// The proof: every message inside the boundary goes to the wire byte-for-byte as it
	// did last turn, so the provider's prefix hash still matches.
	before := gjson.GetBytes(r1.Body, "messages").Array()
	after := gjson.GetBytes(r2.Body, "messages").Array()
	if len(before) <= r2.Trace.MaxCachedIdx || len(after) <= r2.Trace.MaxCachedIdx {
		t.Fatalf("turn 1 sent %d and turn 2 sent %d messages; boundary %d is out of range",
			len(before), len(after), r2.Trace.MaxCachedIdx)
	}
	for i := 0; i <= r2.Trace.MaxCachedIdx; i++ {
		if before[i].Raw != after[i].Raw {
			t.Fatalf("message %d (at or below the cached boundary %d) changed bytes between "+
				"turns: the provider's prefix hash breaks here and everything after it "+
				"re-bills at the cache-write tier (12.5x a read)", i, r2.Trace.MaxCachedIdx)
		}
	}
}

// TestLowerFrozenModeHasNoFreezeProtection pins the hazard that is the reason no
// frozen-aware trigger shipped.
//
// 53% of production's `below_trigger` tokens are compaction resets: the transcript SHRANK,
// so modes.Boundary restarts at 0 and MaxCachedIdx is -1. Every message is then eligible
// and the freeze machinery protects NOTHING — components.Ctx.TailOnly returns true for
// every index. That would be safe if the provider's prefix were also cold, and it is not:
// measured in production, 3,092 such requests carried 404,376,878 cache_READ tokens. So
// "nothing is frozen, therefore acting is free" is false; the only thing keeping the
// deployed pipeline off those cache-hot positions is that it finds nothing to do.
//
// Both halves are asserted: the freeze offers no protection (a reducer given this boundary
// rewrites low positions), and the deployed pipeline rewrites nothing. Making the deployed
// pipeline act here is a REGRESSION, and this test is what says so.
func TestLowerFrozenModeHasNoFreezeProtection(t *testing.T) {
	run := func(yaml string) apply.Result {
		cfg := pipe(t, yaml)
		p, _ := cfg.Build(nil)
		st := store.NewMemory(store.Options{})
		tracker := modes.NewTracker(0)
		o := apply.Opts{Provider: bschemas.Anthropic, Session: bobSession, Tracker: tracker}
		o.Body = compactBody(t, "pre", 50)
		apply.BodyOpts(context.Background(), p, st, o)
		o.Body = compactBody(t, "pre", 60) // grow, so a boundary exists at all
		apply.BodyOpts(context.Background(), p, st, o)
		o.Body = compactBody(t, "post", 5) // the compaction: 62 messages down to 7
		return apply.BodyOpts(context.Background(), p, st, o)
	}

	acting := run(actingPipeline)
	if acting.Trace.MaxCachedIdx != -1 {
		t.Fatalf("post-compaction MaxCachedIdx = %d, want -1", acting.Trace.MaxCachedIdx)
	}
	if acting.Trace.FrozenTokens != 0 {
		t.Fatalf("post-compaction FrozenTokens = %d, want 0 (this IS the lower mode)",
			acting.Trace.FrozenTokens)
	}
	// Half one: the boundary really is wide open. A reducer rewrites positions in the
	// head of the transcript, which on this class the provider is still serving from cache.
	if len(acting.Trace.Changes) == 0 {
		t.Fatal("the reducer changed nothing post-compaction; the exposure claim is untested")
	}
	low := 0
	for _, ch := range acting.Trace.Changes {
		if changeMsgIndex(t, ch.Path) <= 2 { // the head: system + first user turn + first tool
			low++
		}
	}
	if low == 0 {
		t.Fatal("no change landed in the transcript head; if the reset boundary is " +
			"genuinely self-limiting, this test and docs/results/below-trigger-2026-08.md " +
			"are both wrong")
	}

	// Half two: the DEPLOYED pipeline does not take the invitation.
	if deployed := run(deterministicPipeline); len(deployed.Trace.Changes) != 0 {
		t.Fatalf("the deployed pipeline rewrote %d position(s) on a post-compaction turn "+
			"whose provider prefix is cache-hot; measured exposure is $708 per 405M "+
			"cache_read tokens: %+v", len(deployed.Trace.Changes), deployed.Trace.Changes)
	}
}

// TestTriggerDefaultsUnchanged pins that this investigation shipped no behaviour change:
// the zero Trigger still fires on everything (the backward-compatible contract every
// preset without a `trigger:` block relies on), and codesmart's is still the only
// threshold in a deployed preset, at the value production ran.
func TestTriggerDefaultsUnchanged(t *testing.T) {
	var zero components.Trigger
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	var req bschemas.BifrostChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, window := range []int{0, 200000, 1000000} {
		if !zero.Fires(&req, window) {
			t.Fatalf("zero Trigger did not fire at window %d; that breaks every "+
				"config without a trigger: block", window)
		}
	}
	if got := zero.OutputFloor(200000, 1500); got != 1500 {
		t.Fatalf("zero Trigger OutputFloor = %d, want the legacy default 1500", got)
	}
	if zero.IsHuge(1<<30, 200000) {
		t.Fatal("zero Trigger reported a huge output; HugeOutputFrac is unset")
	}
	cfg, err := config.LoadBytes([]byte("preset: codesmart\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := cfg.Components["extract_llm"]
	if !ok {
		t.Fatal("codesmart lost its extract_llm block")
	}
	var got struct {
		Trigger struct {
			MinRequestTokens int `yaml:"min_request_tokens"`
		} `yaml:"trigger"`
	}
	if err := raw.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Trigger.MinRequestTokens != 3000 {
		t.Fatalf("codesmart extract_llm trigger.min_request_tokens = %d, want 3000 "+
			"(unchanged: lowering it reaches 0.21%% of gated tokens for ~$245 of LLM spend "+
			"— see docs/results/below-trigger-2026-08.md)", got.Trigger.MinRequestTokens)
	}
}

// changeMsgIndex pulls N out of a Change path, which is "messages.N" for a whole-message
// rewrite and "messages.N.content.M.content" for a single content block.
func changeMsgIndex(t *testing.T, path string) int {
	t.Helper()
	rest, ok := strings.CutPrefix(path, "messages.")
	if !ok {
		t.Fatalf("unexpected change path %q: not a message path", path)
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("cannot read a message index out of change path %q: %v", path, err)
	}
	return n
}
