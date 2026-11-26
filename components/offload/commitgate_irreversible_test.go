package offload

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
//
// The completeness side is TestEveryComputedEffCommitRefreshSiteIsCoveredByTheReplayTable below,
// the commitRefresh counterpart of TestNoStateIsRecordedBeforeTheCommitGate's gateExempt: only a
// call site passing a COMPUTED eff can reach commitRefresh's eff != markerFull branch — the one
// that sets rep.Irreversible — so those are exactly the sites this table must drive, and that
// test fails when one appears without a subtest. The sites passing markerFull (state.go's
// reapplyFrozen, summarize.go's two) cannot reach the branch and handle the degraded case
// themselves; reapplyFrozen is still driven here because its own len(keys) == 0 block is what
// carries the mask replay.
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

	t.Run("mask via reapplyFrozen", func(t *testing.T) {
		// Overlaps TestASummaryModeReplayIsNotRevertedFromTurnTwoOnward deliberately, and neither
		// replaces the other: that test's subject is reapplyFrozen's behaviour, this entry's is
		// that reapplyFrozen is held to the SAME assertion as the other three replay branches,
		// through the same helper. See that test's doc for how the two fail differently.
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

	t.Run("sweep drop replay", func(t *testing.T) {
		// The third replay branch, driven at the same seam the sweep itself uses. A replayed
		// drop cannot decline (see applySweepDropReplay), so its Irreversible is carried by
		// commitRefresh alone — the same dependence the extract_llm subtest pins.
		st := store.NewMemory(store.Options{MaxEntries: 400})
		c := &components.Ctx{Session: "s", Store: st, Ctx: context.Background()}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(sweepFixture)}}
		originals := capturedText(req)
		var rep components.Report
		if _, ok := applySweepDropReplay(c, &rep, markerSummary, &req.Input[0], sweepFixture); !ok {
			t.Fatal("the replay was refused, so nothing under test ran")
		}
		assertReplayIsExemptFromRevert(t, &rep, req, originals)
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

// irreversibleCovered maps every commitRefresh call site passing a COMPUTED eff — keyed
// "file:function" — to the subtest of TestADegradedModeReplayDeclaresItselfIrreversible that
// drives it. Only a computed eff can differ from markerFull, which is the only way to reach
// commitRefresh's rep.Irreversible branch, so those are exactly the sites the table must drive.
// The completeness test below keeps this list honest in both directions.
var irreversibleCovered = map[string]string{
	"extract_llm.go:Offload":          "extract_llm",
	"extract_sweep_drop.go:sweepDrop": "sweep drop replay",
}

// TestEveryComputedEffCommitRefreshSiteIsCoveredByTheReplayTable is #198's completeness check,
// the commitRefresh counterpart of TestNoStateIsRecordedBeforeTheCommitGate's gateExempt. It
// parses the package's non-test sources, classifies every commitRefresh call by whether its eff
// argument is the markerFull identifier, and fails when a site passing anything else is not in
// irreversibleCovered — because a replay through such a site that forgot rep.Irreversible is
// reverted by components/pipeline.go on EVERY replay turn, the exact defect two of the three
// subtests exist to catch. It also fails when an entry goes stale (its site disappeared) or
// names a subtest the table does not run, so a renamed subtest cannot keep a dead entry looking
// like coverage.
//
// go/ast rather than a source regex because the classification IS the property that matters:
// "is the eff argument the markerFull identifier" is a syntactic fact, and a regex over call
// sites gets argument boundaries wrong in exactly the complicated calls this check exists for.
func TestEveryComputedEffCommitRefreshSiteIsCoveredByTheReplayTable(t *testing.T) {
	// The subtest names the table actually runs, read from this file's own AST.
	tableSubtests := map[string]bool{}
	fset := token.NewFileSet()
	self, err := parser.ParseFile(fset, "commitgate_irreversible_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(self, func(n ast.Node) bool {
		run, ok := n.(*ast.CallExpr)
		if !ok || len(run.Args) == 0 {
			return true
		}
		if sel, ok := run.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
			if lit, ok := run.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				tableSubtests[strings.Trim(lit.Value, "`\"")] = true
			}
		}
		return true
	})

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "commitRefresh" || len(call.Args) != 5 {
					return true
				}
				if id, ok := call.Args[2].(*ast.Ident); ok && id.Name == "markerFull" {
					return true // constant: cannot reach the branch that sets Irreversible
				}
				key := name + ":" + fn.Name.Name
				seen[key] = true
				sub, listed := irreversibleCovered[key]
				if !listed {
					t.Errorf("%s passes a computed eff to commitRefresh and is not in "+
						"irreversibleCovered: a replay through it that forgets "+
						"rep.Irreversible is reverted on every turn under a degraded marker "+
						"mode. Drive it with a subtest of "+
						"TestADegradedModeReplayDeclaresItselfIrreversible, or list it here "+
						"with the test that pins it and a reason", key)
					return true
				}
				if !tableSubtests[sub] {
					t.Errorf("irreversibleCovered maps %s to %q, a subtest the table does not "+
						"run — the entry covers nothing", key, sub)
				}
				return true
			})
		}
	}
	for key := range irreversibleCovered {
		if !seen[key] {
			t.Errorf("irreversibleCovered entry %s matches no commitRefresh call site — it went "+
				"stale when the site changed or disappeared", key)
		}
	}
}
