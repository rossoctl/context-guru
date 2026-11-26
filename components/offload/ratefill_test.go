package offload

import (
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"testing"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.

func TestRateFillAgentRates(t *testing.T) {
	rates := map[string]components.TokenRates{
		"blank":     {Input: 2e-6, Output: 1e-5},
		"stated":    {Input: 2e-6, Output: 1e-5, CacheRead: 3e-7, CacheWrite: 3e-6},
		"readonly":  {Input: 2e-6, Output: 1e-5, CacheRead: 3e-7},
		"writeonly": {Input: 2e-6, Output: 1e-5, CacheWrite: 3e-6},
		"noinput":   {Output: 1e-5, CacheRead: 3e-7, CacheWrite: 3e-6},
		"zero":      {},
	}
	got := map[string]string{}
	for rn, r := range rates {
		for _, m := range ratefilltest.Models {
			f, rd, wr := agentRates(&components.Ctx{ModelName: m, SelfRates: r})
			got["agent/"+rn+"/"+m] = ratefilltest.F(f, rd, wr)
		}
	}
	f, rd, wr := agentRates(nil)
	got["agent/nil"] = ratefilltest.F(f, rd, wr)
	got["agent/consts"] = ratefilltest.F(agentFreshPerMTok, agentCacheReadPerMTok, agentCacheWritePerMTok)
	ratefilltest.Check(t, got)
}
