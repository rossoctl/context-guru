package proxy

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/kvcache"
)

// The live campaign prices a decision on the same economics as the replay. On a tiered model a request
// above the price line bills at the tier's rates, so the stake and the ping are those rates. Before, the
// live path read the base rates and chose K and X on numbers that the replay never saw.
func TestLiveCampaignPricesAtThePromptTierLikeTheReplay(t *testing.T) {
	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = tieredPricer{gpt56Price()}
	rt, pc := campaignRuntime(t, planSpec, nil, campaignModeShadow)
	k.pred = rt
	t0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	arr := rt.arrive("t1", "s", "", t0)
	body := []byte(`{"model":"gpt-5.6","messages":[]}`)
	u := Usage{CacheRead: 300_000, FreshInput: 100, StopReason: "end_turn"} // 300,100 prompt tokens: above 272,000
	if _, ok := k.predictiveDecide(arr, false, "t1", "s", "", "openai", t0, body, agent.IdentifyUserAgent("codex_cli_rs/0.50.0"), u); !ok {
		t.Fatal("decision refused")
	}
	pc.mu.Lock()
	pend := pc.pending["s\x00"]
	pc.mu.Unlock()
	if len(pend) != 1 {
		t.Fatalf("%d pending decisions, want 1", len(pend))
	}
	// Above the line every rate is the tier's: read 1e-6, write 10e-6, input 10e-6, output 45e-6.
	wantStake := 300_000 * (10e-6 - 1e-6)
	wantPing := 300_000*1e-6 + 7*10e-6 + 1*45e-6
	if math.Abs(pend[0].stake-wantStake) > 1e-9 || math.Abs(pend[0].ping-wantPing) > 1e-9 {
		t.Errorf("live stake %v ping %v, want the tier's %v and %v", pend[0].stake, pend[0].ping, wantStake, wantPing)
	}
	// The replay of the same request gives the same figures.
	req := &kvcache.Request{ID: 1, User: "t1", ConversationID: "s", Model: "gpt-5.6", TS: t0.UnixMilli(), UpstreamMs: 1000,
		InputTokens: 100, CacheRead: 300_000, CachedContext: 300_000, CostKnown: true}
	_, facts := campaign.Rows([]*kvcache.Request{req}, k.h.autoPrice(), false, 1<<60)
	if len(facts) != 1 {
		t.Fatalf("replay kept %d rows", len(facts))
	}
	if math.Abs(facts[0].Stake-pend[0].stake) > 1e-9 || math.Abs(facts[0].Ping-pend[0].ping) > 1e-9 {
		t.Errorf("replay stake %v ping %v differs from live %v %v", facts[0].Stake, facts[0].Ping, pend[0].stake, pend[0].ping)
	}
}
