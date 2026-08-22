package extract

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// The preamble's SIZE decides whether its cache breakpoint does anything, so the size is
// part of the contract and not an implementation detail.
//
// Measured at the time of writing (tokens, o200k_base):
//
//	block 0 (general contract)   916 rewrite / 947 deletion-only
//	block 1 (compaction target)  409 low / 755 medium / 835 high
//	total                        1325 - 1782
//
// Against the measured provider minimums that decide whether a breakpoint caches at all
// (4096 haiku-class, 1024 sonnet/opus-class):
//
//   - on the hosted default (model.source: incoming, so extraction runs on the agent's own
//     sonnet/opus-class model) the prefix clears the floor and the breakpoint is REAL;
//   - on a haiku cheap model it does not, and the mark is correctly omitted rather than
//     sent and silently ignored.
//
// We deliberately do NOT pad the prompt to cross the haiku floor. Filler would buy caching
// by making every call carry ~2.3k tokens of text that teaches the model nothing, and the
// prompt's job is extraction quality, not cache eligibility.
func TestPreambleSizeMatchesTheCacheFloorsItIsJudgedAgainst(t *testing.T) {
	for _, rw := range []bool{true, false} {
		for _, a := range []Aggressiveness{AggroLow, AggroMedium, AggroHigh} {
			b := codeSystemBlocks(rw, a)
			total := tokens.Count(b[0]) + tokens.Count(b[1])
			if total < 1000 || total > 3000 {
				t.Fatalf("rewrite=%v %s: preamble is %d tokens, outside the 1000-3000 band "+
					"this component's cost model and cache reasoning assume", rw, a, total)
			}
			if !cheapmodel.CacheablePrefix("aws/claude-sonnet-5", total) {
				t.Fatalf("rewrite=%v %s: a %d-token preamble no longer clears the "+
					"sonnet-class cache floor, so the breakpoint went inert on the hosted "+
					"default path", rw, a, total)
			}
		}
	}
}

// THE CACHE SPLIT, as a structural invariant rather than as a comment: everything invariant
// across a request's candidates must sit in the SYSTEM blocks (which carry the breakpoint) and
// the candidate itself must sit in the USER message (which cannot).
//
// It is pinned because the whole prompt-cache economics of this component rest on it — measured
// on one production request, 9,896 cached prefix tokens written once and read 26 times, a 9.4:1
// read:write ratio over the corpus — and because a regression would be invisible: the calls
// would still succeed and simply cost several times more. A candidate that leaked into a system
// block would rewrite the prefix on every call.
func TestTheCandidateIsNeverInsideTheCacheablePrefix(t *testing.T) {
	const candidate = "UNIQUE_CANDIDATE_MARKER_9f3a1c totally distinctive tool output"
	const goal = "SHARED_GOAL_MARKER_44b2 the conversation context"
	cfg := DefaultCfg()
	cfg.CacheContext = true
	sys, user := buildCodePromptSplit(candidate, goal, []string{"KEEPID_7a1"}, cfg.Rewrite,
		cfg.CacheContext, cfg.Aggressiveness)

	joined := ""
	for _, b := range sys {
		joined += b + "\n"
	}
	if strings.Contains(joined, candidate) {
		t.Fatal("the candidate reached a cacheable system block: every call would rewrite the prefix")
	}
	if !strings.Contains(user, candidate) {
		t.Fatal("the candidate is not in the user message")
	}
	// The goal is identical across a request's candidates, so with CacheContext it belongs in
	// the prefix — that is what lifts the prefix over the provider's minimum cacheable size on
	// a haiku-class compactor, where the 1,893-token preamble alone does not reach it.
	if !strings.Contains(joined, goal) {
		t.Fatal("CacheContext did not put the conversation in a cacheable system block")
	}
	// And the static half must not move with the candidate: two different candidates, same
	// prefix, byte for byte. This is the property claimCacheWrite's one-writer rule relies on.
	sys2, _ := buildCodePromptSplit("a COMPLETELY different tool output", goal,
		[]string{"KEEPID_7a1"}, cfg.Rewrite, cfg.CacheContext, cfg.Aggressiveness)
	if len(sys2) != len(sys) {
		t.Fatalf("prefix block count moved with the candidate: %d vs %d", len(sys2), len(sys))
	}
	for i := range sys {
		if sys[i] != sys2[i] {
			t.Fatalf("prefix block %d changed with the candidate; the cache key rotates per call", i)
		}
	}
}
