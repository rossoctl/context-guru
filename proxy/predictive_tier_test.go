package proxy

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
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

// The model inputs keep what the model was trained on (scripts/campaign/dpbuild.py): the ping-cost
// feature is read at the model's BASE rates, though the decision economics are at the prompt tier.
func TestLiveModelInputsKeepTheBaseRates(t *testing.T) {
	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = tieredPricer{gpt56Price()}
	var got *predictive.Row
	rt, _ := campaignRuntime(t, modelSpec, rowModel{&got}, campaignModeShadow)
	k.pred = rt
	t0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	arr := rt.arrive("t1", "s", "", t0)
	body := []byte(`{"model":"gpt-5.6","messages":[]}`)
	u := Usage{CacheRead: 300_000, FreshInput: 100, StopReason: "end_turn"}
	if _, ok := k.predictiveDecide(arr, false, "t1", "s", "", "openai", t0, body, agent.IdentifyUserAgent("codex_cli_rs/0.50.0"), u); !ok {
		t.Fatal("decision refused")
	}
	if got == nil {
		t.Fatal("the model was never asked")
	}
	idx := -1
	for i, n := range predictive.FeatureNames {
		if n == "l_ping" {
			idx = i
		}
	}
	base := 300_000*0.5e-6 + 7*5e-6 + 30e-6 // base rates: read 0.5, input 5, output 30 per million
	if want := math.Log1p(base * 1000); math.Abs(got.F[idx]-want) > 1e-9 {
		t.Errorf("l_ping %v, want %v (the base-rate ping cost %v)", got.F[idx], want, base)
	}
}

// The CostUSD the live model reads is the bill the dashboard stores (cost_usd), which is what the model
// was trained on: tier rates, and the 1-hour part of a write at the 1-hour rate.
func TestLiveBilledCostEqualsTheStoredBill(t *testing.T) {
	cases := []struct {
		name  string
		model string
		price modelinfo.Price
		u     Usage
	}{
		{"above the line", "gpt-5.6", gpt56Price(), Usage{FreshInput: 100, CacheRead: 300_000, Output: 500}},
		{"below the line", "gpt-5.6", gpt56Price(), Usage{FreshInput: 100, CacheRead: 10_000, CacheWrite: 2000, Output: 50}},
		{"one-hour write", "claude-haiku-5-5", haiku55Price(), Usage{FreshInput: 10, CacheRead: 20_000, CacheWrite: 5000, CacheWrite1h: 3000, Output: 200}},
		{"haiku above its line", "claude-haiku-5-5", haiku55Price(), Usage{FreshInput: 10, CacheRead: 120_000, CacheWrite: 5000, CacheWrite1h: 5000, Output: 200}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, _, _ := testKeeper(t, Limits{})
			k.h.opts.Prices = tieredPricer{c.price}
			e := &dash.Event{Model: c.model, FreshInput: c.u.FreshInput, CacheRead: c.u.CacheRead, CacheWrite: c.u.CacheWrite,
				CacheWrite1h: c.u.CacheWrite1h, OutputTokens: c.u.Output}
			e.Price(c.price, true)
			p, ok := k.h.livePrice(c.model)
			if !ok {
				t.Fatal("no price")
			}
			if got := billedCostOf(p, c.model, time.Now(), c.u); got != e.CostUSD || e.CostUSD == 0 {
				t.Errorf("live cost %v, stored cost %v", got, e.CostUSD)
			}
		})
	}
}
