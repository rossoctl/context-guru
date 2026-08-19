package apply_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// TestSweepCapture replays one capture of real traffic through several configurations and
// reports, per variant, what each component actually removed and what our own LLM calls
// cost. It is a measurement instrument, not an assertion: it exists because comparing two
// live agent sessions compares two different conversations, and the only way to attribute a
// difference to the CONFIG is to hold the traffic fixed.
//
//	CG_SWEEP=1 CONTEXT_GURU_CAPTURE=/path/capture.jsonl go test ./apply -run SweepCapture -v
//
// Set CG_SWEEP_IDLE=<seconds> to advance the injected clock between requests, which is what
// makes a cold-cache sweep fire. CG_SWEEP_VARIANTS=name1,name2 narrows the table.
//
// CG_SWEEP_YAML=a.yaml,b.yaml replaces the built-in table with configs read from disk —
// that is what makes this a general A/B instrument (bench/ab.sh) instead of a fixed study.
// CG_SWEEP_IN_RATE / CG_SWEEP_OUT_RATE are the capture model's real $/MTok on the
// deployment being measured; they set both the agent-model SelfRates and the tier prices
// the value line is quoted at.
func TestSweepCapture(t *testing.T) {
	if os.Getenv("CG_SWEEP") == "" {
		t.Skip("set CG_SWEEP=1 to run the config sweep")
	}
	recs := loadCapture(t, 0)
	models := sweepModels(t)

	only := map[string]bool{}
	for _, n := range splitComma(os.Getenv("CG_SWEEP_VARIANTS")) {
		only[n] = true
	}
	var idle time.Duration
	if s := os.Getenv("CG_SWEEP_IDLE"); s != "" {
		var secs int
		fmt.Sscan(s, &secs)
		idle = time.Duration(secs) * time.Second
	}

	variants := sweepVariants
	if paths := splitComma(os.Getenv("CG_SWEEP_YAML")); len(paths) > 0 {
		variants = nil
		for _, p := range paths {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("variant yaml %s: %v", p, err)
			}
			name := filepath.Base(p)
			name = strings.TrimSuffix(name, filepath.Ext(name))
			variants = append(variants, sweepVariant{name: name, yaml: string(b)})
		}
	}

	fmt.Printf("\ncapture: %d requests   idle-gap: %v   in-rate: $%.2f/MTok\n",
		len(recs), idle, sweepRate("CG_SWEEP_IN_RATE", 3.80))
	for _, v := range variants {
		if len(only) > 0 && !only[v.name] {
			continue
		}
		runVariant(t, v, recs, models, idle)
	}
}

type sweepVariant struct {
	name string
	yaml string
}

// runVariant replays every captured request once through one configuration.
func runVariant(t *testing.T, v sweepVariant, recs []capRec, models components.ModelSpec, idle time.Duration) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(v.yaml))
	if err != nil {
		t.Fatalf("variant %s: %v", v.name, err)
	}
	pipe, err := cfg.Build(nil)
	if err != nil {
		t.Fatalf("variant %s build: %v", v.name, err)
	}
	st := store.NewMemory(store.Options{})
	// A Tracker, because coldness is only computed on the tracker path (apply.go: the
	// legacy store path has no previous-turn TIMESTAMP, so coldCache stays false forever).
	// Without this, CG_SWEEP_IDLE advanced the clock and changed nothing — a cold-cache
	// replay silently measured a warm one.
	tracker := modes.NewTracker(0)
	ctx := context.Background()

	type compAgg struct {
		fired, skipped, reverted int
		saved                    int
		ms                       float64
		gates                    map[string]int
	}
	comps := map[string]*compAgg{}
	var (
		before, after, attempted, frozen int
		calls, accepted, cold            int
		callCost, callMs                 float64
		callSaved                        int
		rejections                       = map[string]int{}
		gateReasons                      = map[string]int{}
	)
	now := time.Now().Add(-idle * time.Duration(len(recs)+1))

	for i, r := range recs {
		now = now.Add(idle)
		res := apply.BodyOpts(ctx, pipe, st, apply.Opts{
			Provider: bschemas.ModelProvider(r.Provider),
			Body:     r.Body,
			Session:  fmt.Sprintf("sweep-%s", v.name),
			Models:   models,
			Now:      now,
			Tracker:  tracker,
			// SelfRates are the agent model's real per-token rates on this deployment
			// (aws/claude-opus-5: $3.80 in / $19.00 out per MTok).
			SelfRates: components.TokenRates{
				Input:  sweepRate("CG_SWEEP_IN_RATE", 3.80) / 1e6,
				Output: sweepRate("CG_SWEEP_OUT_RATE", 19.00) / 1e6,
			},
		})
		_ = i
		attempted += res.AttemptedTokens
		frozen += res.FrozenTokens
		if res.Run == nil {
			continue
		}
		before += res.Run.TokensBefore
		after += res.Run.TokensAfter
		for _, rep := range res.Run.Components {
			a := comps[rep.Component]
			if a == nil {
				a = &compAgg{gates: map[string]int{}}
				comps[rep.Component] = a
			}
			a.fired++
			if rep.Skipped {
				a.skipped++
			}
			if rep.Reverted {
				a.reverted++
			}
			a.saved += rep.TokensBefore - rep.TokensAfter
			a.ms += rep.DurationMs
			for g, n := range rep.Gates {
				a.gates[g] += n
			}
			for _, mc := range rep.Calls {
				calls++
				callCost += mc.CostUSD
				callMs += mc.LatencyMs
				callSaved += mc.SavedTokens
				if mc.Accepted {
					accepted++
				}
				if mc.Cold {
					cold++
				}
				if mc.Rejection != "" {
					rejections[mc.Rejection]++
				}
				if mc.GateReason != "" {
					gateReasons[mc.GateReason]++
				}
				if os.Getenv("CG_SWEEP_CALLS") != "" {
					fmt.Printf("  call model=%q strat=%s aggro=%s cold=%v cand=%d saved=%d in=%d out=%d cr=%d cw=%d cost=$%.5f %.0fms accepted=%v rej=%q gate=%q\n",
						mc.Model, mc.Strategy, mc.Aggressiveness, mc.Cold, mc.CandidateTokens, mc.SavedTokens,
						mc.PromptTokens, mc.CompletionTokens, mc.CacheRead, mc.CacheWrite, mc.CostUSD,
						mc.LatencyMs, mc.Accepted, mc.Rejection, mc.GateReason)
				}
			}
		}
	}

	fmt.Printf("\n=== %s ===\n", v.name)
	fmt.Printf("tokens: before=%d after=%d removed=%d   attempted=%d (%.1f%%) frozen=%d\n",
		before, after, before-after, attempted, pct(attempted, before), frozen)
	names := make([]string, 0, len(comps))
	for n := range comps {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%-12s %6s %6s %8s %9s  %s\n", "component", "ran", "acted", "saved", "ms", "gates")
	for _, n := range names {
		a := comps[n]
		fmt.Printf("%-12s %6d %6d %8d %9.1f  %v\n", n, a.fired, a.fired-a.skipped, a.saved, a.ms, a.gates)
	}
	// Value of the WHOLE arm's removed tokens, at each tier they could have been billed
	// at: fresh, cache_creation (1.25x fresh — what an uncached tail token costs) and
	// cache_read (0.1x). Which one applies is a property of the capture, not of the
	// config, so all three are printed and the caller picks from the capture's own tier
	// split; net subtracts what our own model calls cost to get it.
	fresh := sweepRate("CG_SWEEP_IN_RATE", 3.80)
	removed := float64(before - after)
	var pipeMs float64
	for _, a := range comps {
		pipeMs += a.ms
	}
	fmt.Printf("value: removed=%d  @fresh($%.2f)=$%.4f  @cache_write($%.2f)=$%.4f  @cache_read($%.2f)=$%.4f  llm_cost=$%.4f  net@write=$%+.4f  net@read=$%+.4f\n",
		before-after, fresh, removed*fresh/1e6, fresh*1.25, removed*fresh*1.25/1e6,
		fresh*0.1, removed*fresh*0.1/1e6, callCost,
		removed*fresh*1.25/1e6-callCost, removed*fresh*0.1/1e6-callCost)
	fmt.Printf("latency: pipeline=%.0fms total (%.1fms/req)   llm=%.0fms total\n",
		pipeMs, pipeMs/float64(len(recs)), callMs)
	if calls > 0 {
		fmt.Printf("llm: calls=%d accepted=%d cold=%d saved_tokens=%d cost=$%.4f mean_latency=%.0fms\n",
			calls, accepted, cold, callSaved, callCost, callMs/float64(calls))
		fmt.Printf("     rejections=%v gate=%v\n", rejections, gateReasons)
		// Value of a removed token depends on the tier it would have been billed at.
		// warm-tail: it would have been written into the cache at 1.25x fresh, then read
		// back on every later turn. cold: the whole prefix re-bills at 1.25x fresh.
		fmt.Printf("     value@cache_write(4.75/MTok)=$%.4f  value@cache_read(0.38/MTok)=$%.4f  net@write=$%+.4f\n",
			float64(callSaved)*4.75/1e6, float64(callSaved)*0.38/1e6,
			float64(callSaved)*4.75/1e6-callCost)
	} else {
		fmt.Printf("llm: no calls\n")
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// sweepModels builds the extraction client from the same CHEAP_MODEL_* env the proxy reads,
// so a sweep calls the real gateway with the real model.
func sweepModels(t *testing.T) components.ModelSpec {
	t.Helper()
	base, key := os.Getenv("CHEAP_MODEL_BASE"), os.Getenv("CHEAP_MODEL_AUTH")
	model := os.Getenv("CHEAP_MODEL")
	if base == "" || key == "" || model == "" {
		t.Skip("set CHEAP_MODEL, CHEAP_MODEL_BASE and CHEAP_MODEL_AUTH to sweep the LLM path")
	}
	m := cheapmodel.Anthropic{BaseURL: base, APIKey: key, Model: model, AuthScheme: "bearer"}
	return components.ModelSpec{Incoming: m, Static: m}
}

// sweepRate reads a $/MTok price from the environment; def when unset or unparsable.
func sweepRate(env string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(env), 64); err == nil && v > 0 {
		return v
	}
	return def
}
