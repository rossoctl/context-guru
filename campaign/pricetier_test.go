package campaign

import (
	"testing"

	"github.com/rossoctl/context-guru/kvcache"
)

// A replayed ping reads CachedContext plus its own input: it is priced at that size.
func TestReplayPingIsPricedAtItsOwnSize(t *testing.T) {
	const t0 = int64(1_786_000_000_000)
	r := &kvcache.Request{ID: 1, User: "u", ConversationID: "s", Model: "m", TS: t0, UpstreamMs: 1000,
		InputTokens: 10, CacheRead: 271_000, CachedContext: 272_000, CostKnown: true}
	var sizes []int64
	price := func(_ string, n int64) (float64, float64, float64, float64, bool) {
		sizes = append(sizes, n)
		if n > 272_000 { // above the line: dear
			return 2, 2, 2, 4, true
		}
		return 1, 1, 1, 4, true
	}
	_, facts := Rows([]*kvcache.Request{r}, price, false, t0+10*3600e3)
	if len(facts) != 1 {
		t.Fatalf("rows %d", len(facts))
	}
	found := false
	for _, n := range sizes {
		if n == 272_000+kvcache.DefaultPingInputTokens {
			found = true
		}
	}
	if !found {
		t.Fatalf("no price lookup at the ping's size, got %v", sizes)
	}
	// 7 fresh input tokens (kvcache.DefaultPingInputTokens) at 2, output 1 at 2.
	if want := 272_000*2.0 + 7*2 + 2; facts[0].Ping != want {
		t.Fatalf("ping cost %v want %v", facts[0].Ping, want)
	}
}
