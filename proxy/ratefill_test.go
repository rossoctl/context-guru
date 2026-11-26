package proxy

import (
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"testing"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.

func TestRateFillStatsAgentRates(t *testing.T) {
	ratefilltest.Check(t, map[string]string{"stats": ratefilltest.F(agentFreshPerMTok, agentCacheReadPerMTok)})
}
