package proxy

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The agent-class rates are literals (see the comment at the constant). This test keeps them
// equal to what the one price function says for the Sonnet 4.5 class they describe.
func TestAgentClassRatesMatchPrice(t *testing.T) {
	const id = "claude-sonnet-4-5"
	cr := modelinfo.Price{Input: agentFreshPerMTok}.CacheReadFor(id)
	if math.Abs(agentCacheReadPerMTok-cr) > 1e-12 {
		t.Errorf("read literal %v, function %v", agentCacheReadPerMTok, cr)
	}
}
