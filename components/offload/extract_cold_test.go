package offload

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// The cold sweep left this component. It is `extract_llm_sweep` now, and the behaviour that used to
// live here — reaching depth on a cold turn, the sweep's own floor and cap, min_idle_seconds, the
// context mode, not drawing on the hot path's session budget — is tested against that component in
// extract_sweep_test.go, where it is exercised through the shape that was actually measured good
// (batched adjudication) rather than through a compaction pass pointed at deep history.
//
// Two things stay here, because neither is about the sweep component:
//
//   - the PRICING of a cold turn, which extract_llm still sees: it can run on a cold turn like any
//     other, it just no longer treats one specially.
//   - the refusal of the keys that moved, which is a property of THIS component's config surface.

// A cold turn's tokens are re-billed as cache CREATION at 1.25x fresh, so a removed token is
// worth 12.5x its warm-turn value. Getting this backwards is what would make the gate
// suppress the one case that pays best.
func TestColdTurnPricesTokensAtTheWriteRate(t *testing.T) {
	warm := savedTokenValue(&components.Ctx{CacheAware: true})
	cold := savedTokenValue(&components.Ctx{CacheAware: true, ColdCache: true})
	fresh := savedTokenValue(&components.Ctx{})

	if !warm.cached {
		t.Fatal("a warm cache-aware turn must price at the cache-read rate")
	}
	if cold.cached {
		t.Fatal("a cold turn must not be treated as cached; that applies the 10x haircut " +
			"to the one case where saving is worth MORE than fresh input")
	}
	if !(cold.perToken > fresh.perToken && fresh.perToken > warm.perToken) {
		t.Fatalf("expected cold > fresh > warm per-token value, got cold=%g fresh=%g warm=%g",
			cold.perToken, fresh.perToken, warm.perToken)
	}
	if ratio := cold.perToken / warm.perToken; ratio < 12 || ratio > 13 {
		t.Fatalf("cold/warm token value ratio is %.2f, want ~12.5 (1.25x fresh vs 0.1x fresh)", ratio)
	}
}

// THE KEYS THAT MOVED MUST SAY SO. Breaking existing configs is deliberate — there is one deployment
// and it is migrated by hand — but a removed key that produces "field not found" reads as a typo
// rather than as a relocation, and `cold_cache: {enabled: true}` silently accepted would read as "the
// sweep is on" while nothing swept. That is the most expensive available misreading of this config:
// the sweep exists for the turns measured at 4% of requests and 31% of spend.
func TestKeysThatMovedToTheSweepAreRefusedByName(t *testing.T) {
	for _, tc := range []struct {
		key, yaml, wants string
	}{
		{"per_output", "per_output: false\n", "warm/tail pass"},
		{"cold_cache", "cold_cache:\n  enabled: true\n", "extract_llm_sweep"},
		{"cold_cache.min_tokens", "cold_cache:\n  min_tokens: 800\n", "min_tokens"},
		{"cold_cache.max_calls", "cold_cache:\n  max_calls: 2\n", "max_calls"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, err := newExtractLLM([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s was accepted; the operator would believe it still does something", tc.key)
			}
			// It must name the REPLACEMENT, not merely reject the key. A generic yaml
			// "field not found" is what this test exists to rule out.
			if !strings.Contains(err.Error(), "extract_llm_sweep") {
				t.Errorf("error does not name the component the key moved to: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error does not say what %s becomes (want mention of %q): %v",
					tc.key, tc.wants, err)
			}
		})
	}
}

// And the component must still BUILD with an empty config: the split removed keys, it did not add a
// required one. A config error here would take the whole pipeline down at boot.
func TestExtractLLMStillBuildsWithNoConfig(t *testing.T) {
	c, err := newExtractLLM(nil)
	if err != nil {
		t.Fatalf("empty config must build: %v", err)
	}
	if _, ok := c.(*ExtractLLM); !ok {
		t.Fatal("constructor returned the wrong type")
	}
	// The keys that remain must be untouched by the removal.
	e := c.(*ExtractLLM)
	if e.minTokens != 300 || e.strategy != "code" {
		t.Errorf("the surviving defaults moved: min_tokens=%d strategy=%q", e.minTokens, e.strategy)
	}
}

var _ = components.Report{} // keep the import honest if the pricing test above is ever trimmed
