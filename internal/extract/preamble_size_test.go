package extract

import (
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
