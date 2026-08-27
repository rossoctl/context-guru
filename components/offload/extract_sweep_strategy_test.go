package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/extract"
)

// The narrowing must apply on a SWEEP and not on a warm turn, and it must be COUNTED rather than
// silent — the half of the trim change that is easy to lose, since there a model answering `trim`
// degraded to `keep` and was counted, because an unjudged output is otherwise indistinguishable
// from silence. Here, "the operator asked for rlm and got code" must not look like "the operator
// asked for code".
//
// Both arms use the SAME `strategy: rlm` config and differ only in whether the prompt cache is
// cold, so this asserts the sweep condition rather than something general about rlm.
func TestSweepNarrowsATransportingStrategyAndCountsIt(t *testing.T) {
	const yaml = "strategy: rlm\neconomic_gate: false\nmin_tokens: 1\n" +
		"cold_cache:\n  enabled: true\n  min_tokens: 1\n"
	for _, cold := range []bool{false, true} {
		name := "warm"
		if cold {
			name = "cold sweep"
		}
		t.Run(name, func(t *testing.T) {
			comp, err := newExtractLLM([]byte(yaml))
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			e := comp.(*ExtractLLM)
			rep := components.Report{}
			if _, err := e.Offload(coldReq(), &rep,
				coldCtx("s", cold, 600_000, &silentModel{})); err != nil {
				t.Fatalf("offload: %v", err)
			}
			got := rep.Gates["sweep_strategy_narrowed"]
			if cold && got == 0 {
				t.Errorf("a sweep configured with a transporting strategy must record "+
					"sweep_strategy_narrowed; gates=%v", rep.Gates)
			}
			if !cold && got != 0 {
				t.Errorf("a warm turn must not narrow the strategy, got %d; gates=%v",
					got, rep.Gates)
			}
		})
	}
}

// The strategy split that makes the restriction meaningful: a sweep must never select a strategy
// that hands tool-output text back THROUGH the model.
//
// This is the per-output form of removing the merged design's `trim` verdict, and rests on the same
// measurement — trim was chosen zero times in 21 probe opportunities, metrics were identical
// without it, and in production it was accepted once against eight rejected as invented, because it
// was the only verdict that asked the model to transport text.
func TestNonTransportingStrategiesExcludeTheReproducingOnes(t *testing.T) {
	transporting := map[string]bool{"single": true, "rlm": true}
	for _, s := range nonTransportingStrategies {
		if transporting[s] {
			t.Errorf("%q transports text and must not be allowed while sweeping", s)
		}
	}
	// The list must not be empty of the one strategy that can actually compact with a model:
	// narrowing to deterministic alone would silently turn a sweep into a no-LLM projection.
	var hasCode bool
	for _, s := range nonTransportingStrategies {
		if s == "code" {
			hasCode = true
		}
	}
	if !hasCode {
		t.Error("`code` must remain allowed: it is the only model-driven strategy that does not " +
			"transport text, and without it a sweep degrades to a deterministic projection")
	}
	// `auto` must be absent rather than filtered: it is an ORDER, and intersectAllowed narrows the
	// order it produces. Listing it would allow the transporting modes back in.
	for _, s := range nonTransportingStrategies {
		if s == "auto" {
			t.Error("`auto` must not appear in the allow-list: it resolves to an order that " +
				"includes rlm and single")
		}
	}
}

// transportsText decides whether narrowing gets COUNTED, so it must classify `auto` as
// transporting: auto's first pick on a large body is `rlm`, and large bodies are the sweep case.
// Getting this wrong makes the narrowing silent for the configuration where it matters most.
func TestTransportsTextClassifiesEveryStrategy(t *testing.T) {
	want := map[string]bool{
		"code": false, "deterministic": false,
		"single": true, "rlm": true, "auto": true,
	}
	// Every strategy the engine accepts must be classified, or a new one silently defaults to
	// "does not transport" and its narrowing goes uncounted.
	for _, m := range extract.Modes {
		w, ok := want[m]
		if !ok {
			t.Fatalf("strategy %q is accepted by the engine but unclassified here; decide "+
				"whether it transports text", m)
		}
		if got := transportsText(m); got != w {
			t.Errorf("transportsText(%q) = %v, want %v", m, got, w)
		}
	}
	if len(want) != len(extract.Modes) {
		t.Errorf("classification covers %d strategies, engine accepts %d — the two must agree",
			len(want), len(extract.Modes))
	}
}
