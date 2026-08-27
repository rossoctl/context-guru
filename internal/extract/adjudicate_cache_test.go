package extract

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// blockRecorder is a Model that also implements SystemBlocksModel, so a test can see WHICH half of
// the prompt each piece landed in. Without it the split is unobservable: a joined prompt looks
// identical to a split one from the outside, which is exactly how the missing breakpoint went
// unnoticed.
type blockRecorder struct {
	system []string
	user   string
}

func (r *blockRecorder) Complete(context.Context, string) (string, error) {
	r.system, r.user = nil, "COMPLETE-FALLBACK"
	return "[]", nil
}

func (r *blockRecorder) CompleteBlocks(_ context.Context, system []string, user string) (string, error) {
	r.system, r.user = system, user
	return "[]", nil
}

// THE ADJUDICATION CALL MUST CARRY A SYSTEM PREFIX. It did not: the component called Model.Complete,
// which routes to CompleteSystem(ctx, "", prompt) and thence to CompleteBlocks(ctx, nil, prompt) — no
// system field, so systemBlocks placed no cache_control mark and there was nothing for a sibling
// batch to read. A batch's worth of shared contract was re-sent as fresh input on every call.
func TestAdjudicationSendsTheContractAsACacheableSystemPrefix(t *testing.T) {
	items := []AdjudicationItem{
		{Label: 0, SizeTokens: 900, Content: "ALPHA-PAYLOAD-ostrich\n"},
		{Label: 1, SizeTokens: 900, Content: "BETA-PAYLOAD-narwhal\n"},
	}
	rec := &blockRecorder{}
	if _, err := AskAdjudication(context.Background(), rec, "fix the flaky test", items, true); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the split capability was used at all. If the call had fallen through to
	// Complete, every assertion below would be about a prompt with no prefix to cache.
	if rec.user == "COMPLETE-FALLBACK" {
		t.Fatal("the call fell through to Complete, so no cache_control breakpoint can be placed")
	}
	if len(rec.system) == 0 {
		t.Fatal("no system blocks: there is no prefix for a breakpoint to mark")
	}
	joined := strings.Join(rec.system, "\n")
	if !strings.Contains(joined, "SPENT only if") {
		t.Error("the contract is not in the system half, so the invariant part is not cacheable")
	}

	// THE ITEMS MUST NEVER BE IN THE PREFIX. They differ per batch, so a cache entry containing them
	// could never be read — strictly worse than no breakpoint, because a write costs 1.25x fresh.
	for _, it := range items {
		if strings.Contains(joined, strings.TrimSpace(it.Content)) {
			t.Errorf("output %d is inside the cacheable prefix; no sibling batch could ever read it",
				it.Label)
		}
		if !strings.Contains(rec.user, strings.TrimSpace(it.Content)) {
			t.Errorf("output %d is not in the user half, so the model was never shown it", it.Label)
		}
	}
	// And with cacheContext on, the goal joins the prefix — it is invariant across a sweep's batches.
	if !strings.Contains(joined, "fix the flaky test") {
		t.Error("cacheContext: the goal is not in the cacheable prefix, so siblings re-send it")
	}
	if strings.Contains(rec.user, "fix the flaky test") {
		t.Error("the goal is in BOTH halves, so it is paid for twice")
	}
}

// With cacheContext OFF — a single-batch sweep — the goal belongs in the user half. There is no
// sibling to read a cache entry, and a write costs 1.25x fresh, so paying for one is a 25% loss.
func TestASingleBatchDoesNotPayForACacheWriteOnTheGoal(t *testing.T) {
	rec := &blockRecorder{}
	if _, err := AskAdjudication(context.Background(), rec, "fix the flaky test",
		[]AdjudicationItem{{Label: 0, Content: "only-one\n"}}, false); err != nil {
		t.Fatal(err)
	}
	if rec.user == "COMPLETE-FALLBACK" {
		t.Fatal("the call fell through to Complete")
	}
	joined := strings.Join(rec.system, "\n")
	if !strings.Contains(joined, "SPENT only if") {
		t.Fatal("the contract left the system half even without cacheContext")
	}
	if strings.Contains(joined, "fix the flaky test") {
		t.Error("a single-batch call put the goal in the cacheable prefix, paying a 1.25x write " +
			"for an entry nothing will read")
	}
	if !strings.Contains(rec.user, "fix the flaky test") {
		t.Error("the goal reached neither half, so relevance is judged against nothing")
	}
}

// THE MINIMUM CACHEABLE PREFIX IS THE BINDING CONSTRAINT, AND IT IS NOT MET ON HAIKU.
//
// A cache_control below the model's minimum is silently ignored — no error,
// cache_creation_input_tokens: 0 — so a split that clears no floor buys nothing. This records the
// measurement rather than asserting a hope, and it is what the sweep's serialize-first decision is
// gated on: there is no point paying a serialized queue round to earn a write the provider will
// refuse.
//
// If the contract ever grows past a floor this test says it does not clear, that is a REAL change in
// the component's economics and this test is where it should be noticed.
func TestAdjudicationPrefixSizeAgainstTheProviderFloors(t *testing.T) {
	contract := tokens.Count(adjudicationContract)
	// A two-message `recent` context, which is the shipped default.
	goal := "user: fix the flaky auth test\nassistant: I will run the suite then patch it.\n"
	prefix := AdjudicationPrefixTokens(goal, true)
	t.Logf("contract %d o200k, contract+recent-goal %d o200k", contract, prefix)
	if prefix <= contract {
		t.Fatalf("the goal did not reach the prefix (%d <= %d), so this measures nothing", prefix, contract)
	}
	// haiku-class, which is what the shipped housellm preset pins, and what an unnameable gateway
	// alias is conservatively treated as: PROVABLY not cacheable at this context size.
	for _, m := range []string{"claude-haiku-4-5", "some-gateway-alias"} {
		if cheapmodel.CacheablePrefix(m, prefix) {
			t.Errorf("%s now caches a %d-token prefix — the sweep's economics changed and the "+
				"comments in adjudicate.go and extract_sweep.go are stale", m, prefix)
		}
	}
	// sonnet-class is REACHABLE: it needs only a few hundred tokens of conversation on top of the
	// contract, which a real two-message context often supplies. Asserted as reachable-in-principle
	// rather than reachable-at-this-goal, because the goal is the operator's.
	big := AdjudicationPrefixTokens(strings.Repeat("a plausible line of agent conversation.\n", 60), true)
	if !cheapmodel.CacheablePrefix("claude-sonnet-5", big) {
		t.Errorf("sonnet-class does not cache even a %d-token prefix; the split can never pay "+
			"anywhere and serializing the first batch is dead code", big)
	}
	if cheapmodel.CacheablePrefix("claude-haiku-4-5", big) {
		t.Logf("note: haiku DOES cache at %d o200k — the asymmetry has narrowed", big)
	}
}

// The one-string form must still contain everything, for a client with no system capability. Content
// is identical in all three paths; only the caching differs.
func TestTheJoinedPromptCarriesBothHalves(t *testing.T) {
	items := []AdjudicationItem{{Label: 7, SizeTokens: 5, Content: "GAMMA-PAYLOAD\n"}}
	p := BuildAdjudicationPrompt("the goal", items)
	for _, want := range []string{"SPENT only if", "the goal", "GAMMA-PAYLOAD",
		"=== OUTPUT " + strconv.Itoa(7)} {
		if !strings.Contains(p, want) {
			t.Errorf("the joined prompt is missing %q", want)
		}
	}
}
