package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// A REPLAY UNDER marker_mode summary/off MUST STILL DECLARE ITSELF IRREVERSIBLE.
//
// components/pipeline.go reverts an Offload that shrank the request, returned no cache keys, and
// set neither Skipped nor Irreversible — "offload dropped content without stashing a cache_key".
// A deliberate lossy drop is exempt because it sets Irreversible, and commitMark's non-full branch
// is what sets it.
//
// So a replay path that stops calling commitMark stops setting it, and the component is reverted
// on every replay turn: the transcript goes upstream verbatim, which is a full-suffix cache write
// per turn — the precise harm the reserve work exists to prevent, produced by the reserve work.
//
// Asserted on rep rather than through the pipeline because rep is what the pipeline reads; the
// revert itself is pipeline.go's behaviour and already covered there.
func TestADegradedModeReplayDeclaresItselfIrreversible(t *testing.T) {
	body := strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 200)

	t.Run("extract_llm", func(t *testing.T) {
		st := store.NewMemory(store.Options{MaxEntries: 400})
		model := &shrinkingModel{}
		c, err := newExtractLLM([]byte("min_tokens: 1\nstrategy: code\neconomic_gate: false\n" +
			"marker_mode: summary\n"))
		if err != nil {
			t.Fatal(err)
		}
		e := c.(*ExtractLLM)
		e.modelClient = model
		ctx := &components.Ctx{Session: "s", Store: st, Ctx: context.Background(),
			Model: components.ModelSpec{Static: model, Incoming: model}}
		// A frozen decision, as an earlier turn leaves one — putResult is not gated on markerFull,
		// which is what makes a summary-mode replay reachable at all.
		putResult(ctx, extract.ContentKey(body), "one line kept", "")
		if _, hit := getResult(ctx, extract.ContentKey(body)); !hit {
			t.Fatal("the fixture's frozen decision is not readable, so the replay path is not taken")
		}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("summarize the log"), toolResultMsg(body),
		}}
		originals := capturedText(req)
		var rep components.Report
		if _, err := e.Offload(req, &rep, ctx); err != nil {
			t.Fatal(err)
		}
		assertReplayIsExemptFromRevert(t, &rep, req, originals)
	})

	t.Run("extract_sweep_drop", func(t *testing.T) {
		// The fourth replay branch, and the one nothing pinned: the other three are covered by the
		// subtests here and by the table test, so this branch could have regressed silently.
		asker := &labelAsker{verdict: "drop", needed: "none"}
		asker.cacheRead = 19595
		e := newSweepSmall(t, "marker_mode: summary\n")
		st := store.NewMemory(store.Options{MaxEntries: 400})
		c := preExpiryCtx("s-sweep", asker, st)
		req := sweepReqStocked()
		// Turn 1 takes the decisions and freezes them.
		var r1 components.Report
		if _, err := e.Offload(req, &r1, c); err != nil {
			t.Fatal(err)
		}
		if r1.Events["sweep_dropped"] == 0 {
			t.Fatal("turn 1 dropped nothing, so turn 2 has no frozen decision to replay")
		}
		// Turn 2 replays them through applySweepDropReplay.
		req2 := sweepReqStocked()
		originals := capturedText(req2)
		var r2 components.Report
		if _, err := e.Offload(req2, &r2, c); err != nil {
			t.Fatal(err)
		}
		if r2.Replays == 0 {
			t.Fatalf("turn 2 replayed nothing, so the branch under test never ran "+
				"(gates: %v, events: %v)", r2.Gates, r2.Events)
		}
		assertReplayIsExemptFromRevert(t, &r2, req2, originals)
	})

	t.Run("mask via reapplyFrozen", func(t *testing.T) {
		// Pre-existing rather than a regression, and the same omission: every turn AFTER the one
		// that took the decision replays through reapplyFrozen, which never set Irreversible.
		st := store.NewMemory(store.Options{MaxEntries: 400})
		c, err := newMask([]byte("keep_recent: 0\nmin_tokens: 20\nmarker_mode: summary\n"))
		if err != nil {
			t.Fatal(err)
		}
		ctx := &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
		mk := func() *bschemas.BifrostChatRequest {
			m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
			schema.SetMessageText(&m, body)
			return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{m}}
		}
		// Turn 1 takes the decision and freezes it. commitMark sets Irreversible here, and that
		// turn was never the broken one.
		var r1 components.Report
		if _, err := c.(components.Offload).Offload(mk(), &r1, ctx); err != nil {
			t.Fatal(err)
		}
		if !r1.Irreversible {
			t.Fatal("turn 1 did not set Irreversible, so the fixture is not in summary mode and " +
				"turn 2 below is not the case under test")
		}
		// Turn 2 replays it.
		req2 := mk()
		originals := capturedText(req2)
		var r2 components.Report
		if _, err := c.(components.Offload).Offload(req2, &r2, ctx); err != nil {
			t.Fatal(err)
		}
		assertReplayIsExemptFromRevert(t, &r2, req2, originals)
	})
}

// assertReplayIsExemptFromRevert checks the exact conjunction components/pipeline.go tests: the
// component shrank the request and returned no keys, so it must have said the loss was chosen.
//
// `originals` is the request's text BEFORE the replay, message by message. It used to be a single
// string — the candidate's original — and every message was compared against it, which made the
// vacuity guard below vacuous: every fixture carries a userMsg ("summarize the log", …) that
// differs from the candidate's text by construction, so `shrank` was unconditionally true and the
// guard could never detect the case its own fatal message describes. The extract_llm subtest had
// no other protection, so it would have passed vacuously if that replay ever declined — and that
// replay branch is the one whose regression started this thread.
func assertReplayIsExemptFromRevert(t *testing.T, rep *components.Report, req *bschemas.BifrostChatRequest, originals []string) {
	t.Helper()
	if len(originals) != len(req.Input) {
		// A replay must not add or drop messages, so a length change means the fixture is not
		// what this helper assumes and the comparison below would be meaningless.
		t.Fatalf("the request has %d messages, captured %d before the replay",
			len(req.Input), len(originals))
	}
	shrank := false
	for i := range req.Input {
		got := schema.MessageText(req.Input[i])
		if got != originals[i] && schema.TextTokens(got) < schema.TextTokens(originals[i]) {
			shrank = true
		}
	}
	if !shrank || rep.Skipped {
		t.Fatalf("the replay did not rewrite anything (skipped=%v), so the revert condition is "+
			"not reachable and this assertion would pass vacuously", rep.Skipped)
	}
	if len(rep.CacheKeys) != 0 {
		t.Fatalf("the fixture returned cache keys (%v), so it is not in a degraded marker mode",
			rep.CacheKeys)
	}
	if !rep.Irreversible {
		t.Error("a replay rewrote the message, returned NO cache key, and did not set " +
			"rep.Irreversible — the exact conjunction components/pipeline.go reverts as " +
			"\"offload dropped content without stashing a cache_key\". The whole component is " +
			"reverted and the transcript goes upstream verbatim on every replay turn: a " +
			"full-suffix cache write per turn, which is the harm this change exists to prevent")
	}
}

// capturedText snapshots every message's text, so an assertion can compare each one against its
// OWN before-state rather than against one shared string.
func capturedText(req *bschemas.BifrostChatRequest) []string {
	out := make([]string, len(req.Input))
	for i := range req.Input {
		out[i] = schema.MessageText(req.Input[i])
	}
	return out
}
