package litellm

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLivePriceLookup is a diagnostic against the real LiteLLM prices map. It is
// skipped unless CG_LIVE_PRICE=1, so CI never depends on the network.
func TestLivePriceLookup(t *testing.T) {
	if os.Getenv("CG_LIVE_PRICE") == "" {
		t.Skip("set CG_LIVE_PRICE=1 to probe the live LiteLLM prices map")
	}
	l := NewLiteLLM("", nil, 0)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		if _, ok := l.Price(ctx, "aws/claude-sonnet-5"); ok {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, m := range []string{"aws/claude-sonnet-5", "claude-sonnet-4-5", "aws/claude-haiku-4-5", "gpt-4o"} {
		p, ok := l.Price(ctx, m)
		w, wok := l.Window(ctx, m)
		t.Logf("%-24s price_ok=%v %+v window=%d %v", m, ok, p, w, wok)
	}
}
