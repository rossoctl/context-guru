package litellm

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/testutil"
)

const tierSample = `{
  "gpt-5.5": {"max_input_tokens": 1050000, "input_cost_per_token": 5e-06, "output_cost_per_token": 3e-05,
              "cache_read_input_token_cost": 5e-07,
              "input_cost_per_token_above_272k_tokens": 1e-05,
              "output_cost_per_token_above_272k_tokens": 4.5e-05,
              "cache_read_input_token_cost_above_272k_tokens": 1e-06,
              "input_cost_per_token_above_272k_tokens_priority": 2e-05,
              "input_cost_per_token_above_272k_tokens_batches": 5e-06},
  "gpt-5.6-x": {"input_cost_per_token": 4e-06, "output_cost_per_token": 2e-05,
              "input_cost_per_token_above_200k_tokens": 6e-06},
  "claude-haiku-5-5": {"input_cost_per_token": 1e-07, "output_cost_per_token": 5e-07,
              "cache_read_input_token_cost": 1e-08, "cache_creation_input_token_cost": 1.25e-07},
  "claude-sonnet-5": {"input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05},
  "gpt-5.5-partial": {"input_cost_per_token": 5e-06, "output_cost_per_token": 3e-05,
              "input_cost_per_token_above_272k_tokens": 1e-05},
  "gpt-5.5-neg": {"input_cost_per_token": 5e-06, "output_cost_per_token": 3e-05,
              "input_cost_per_token_above_272k_tokens": -1e-05},
  "two-line": {"input_cost_per_token": 1e-06, "output_cost_per_token": 4e-06,
              "cache_creation_input_token_cost": 1.25e-06,
              "input_cost_per_token_above_256k_tokens": 3e-06,
              "cache_creation_input_token_cost_above_256k_tokens": 4e-06,
              "input_cost_per_token_above_32k_tokens": 2e-06},
  "gpt-5.4-mini": {"input_cost_per_token": 7.5e-07, "output_cost_per_token": 4.5e-06}
}`

func tierPrice(t *testing.T, id string) modelinfo.Price {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(tierSample))
	}))
	defer srv.Close()
	l := NewLiteLLM(srv.URL, srv.Client(), time.Hour)
	var got modelinfo.Price
	testutil.WaitFor(t, "price "+id, func() bool {
		p, ok := l.Price(context.Background(), id)
		got = p
		return ok
	})
	return got
}

func TestLiteLLMParsesAboveTiers(t *testing.T) {
	// A stated feed tier: absolute rates, other price modes (_priority, _batches) ignored.
	p := tierPrice(t, "gpt-5.5")
	if p.Tiers[0].AbovePromptTokens != 272_000 || p.Tiers[1].AbovePromptTokens != 0 {
		t.Fatalf("Tiers = %+v, want one at 272000", p.Tiers)
	}
	hi := p.At(272_001)
	if hi.Input != 1e-05 || hi.Output != 4.5e-05 || hi.CacheRead != 1e-06 {
		t.Errorf("At(272001) = %+v", hi)
	}
	if lo := p.At(272_000); lo.Input != 5e-06 {
		t.Errorf("At(272000).Input = %g, want base", lo.Input)
	}
	// Any _above_<N>k_tokens line; unstated rates scale with the input ratio (1.5x here).
	q := tierPrice(t, "gpt-5.6-x")
	if q.Tiers[0].AbovePromptTokens != 200_000 || q.Tiers[1].AbovePromptTokens != 0 {
		t.Fatalf("Tiers = %+v, want one at 200000", q.Tiers)
	}
	if got := q.At(200_001); got.Input != 6e-06 || math.Abs(got.Output-3e-05) > 1e-18 {
		t.Errorf("At(200001) = %+v, want Input 6e-6 Output 3e-5", got)
	}
}

func TestLiteLLMAppliesBuiltinTiersOnlyWhenFeedStatesNone(t *testing.T) {
	h := tierPrice(t, "claude-haiku-5-5")
	if h.Tiers[0].AbovePromptTokens != 100_000 {
		t.Fatalf("haiku Tiers = %+v, want built-in at 100000", h.Tiers)
	}
	if got := h.At(100_001).Input; math.Abs(got-5e-07) > 1e-18 {
		t.Errorf("haiku At(100001).Input = %g, want 5e-7", got)
	}
	for _, id := range []string{"claude-sonnet-5", "gpt-5.4-mini"} {
		if p := tierPrice(t, id); p.HasTiers() {
			t.Errorf("%s Tiers = %+v, want none", id, p.Tiers)
		}
	}
}

func TestLiteLLMPartialTierUsesBuiltinMultiple(t *testing.T) {
	p := tierPrice(t, "gpt-5.5-partial")
	// Input stated 2x. Output not stated: OpenAI's 1.5x, not the input ratio 2x.
	got := p.At(272_001)
	if got.Input != 1e-05 || math.Abs(got.Output-4.5e-05) > 1e-18 {
		t.Errorf("At(272001) = %+v, want Input 1e-5 Output 4.5e-5", got)
	}
}

func TestLiteLLMNegativeTierRateIsInvalid(t *testing.T) {
	p := tierPrice(t, "gpt-5.5-neg")
	// The bad line is dropped. The built-in tier takes its place.
	if got := p.At(272_001).Input; math.Abs(got-1e-05) > 1e-18 {
		t.Errorf("Input above line = %g, want built-in 1e-5", got)
	}
}

func TestLiteLLMTwoTierLinesAndWriteKey(t *testing.T) {
	p := tierPrice(t, "two-line")
	if p.Tiers[0].AbovePromptTokens != 32_000 || p.Tiers[1].AbovePromptTokens != 256_000 || p.Tiers[2].AbovePromptTokens != 0 {
		t.Fatalf("Tiers = %+v, want 32k then 256k", p.Tiers)
	}
	if got := p.At(32_000).Input; got != 1e-06 {
		t.Errorf("At(32000).Input = %g, want base", got)
	}
	if got := p.At(100_000); got.Input != 2e-06 || math.Abs(got.Output-8e-06) > 1e-18 {
		t.Errorf("At(100000) = %+v, want Input 2e-6 Output 8e-6 (input ratio)", got)
	}
	if got := p.At(300_000); got.Input != 3e-06 || got.CacheWrite != 4e-06 {
		t.Errorf("At(300000) = %+v, want Input 3e-6 CacheWrite 4e-6 (stated key)", got)
	}
}
