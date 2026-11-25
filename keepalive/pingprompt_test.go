package keepalive

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// A ping's own input is the one kvcache constant. A prefix 5 tokens under the 272K line
// sends a ping above it, so the ping prices at the tier rate (#1437).
func TestPingPromptUsesTheOneConstantAndPicksTheTier(t *testing.T) {
	const prefix = 271_995
	tier, ok := modelinfo.BuiltinTier("gpt-5.6")
	if !ok {
		t.Fatal("no built-in tier for gpt-5.6")
	}
	base := modelinfo.Price{Input: 1, CacheRead: 0.1}
	base.Tiers[0] = tier
	if got := base.At(PingPrompt(prefix)).CacheRead; got != 0.2 {
		t.Fatalf("cache read at the ping's prompt size = %v, want the tier rate 0.2", got)
	}
}
