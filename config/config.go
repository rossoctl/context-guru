// Package config loads context-guru's configuration and builds a pipeline from
// it. One strict YAML struct serves both hosts (design D9): the proxy loads a
// file; the AuthBridge plugin hands its config: subtree to LoadBytes; a k8s
// ConfigMap/CRD just renders the same YAML.
//
// The pipeline: name-list controls order + enablement. Each component's own
// typed config lives under components:<name>; it's handed to the component's
// constructor verbatim, so adding a component makes it configurable with no
// change here. A preset expands to a default pipeline + component configs,
// which the explicit fields then override.
package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
)

// Config is the whole configuration document.
type Config struct {
	Preset     string               `yaml:"preset"`
	Pipeline   []string             `yaml:"pipeline"`
	Components map[string]yaml.Node `yaml:"components"`
	Store      store.Options        `yaml:"store"`
	// Mode is the operating mode: sync (default) | observe. See #31 and
	// docs/how-to/operating-modes.md. Empty = sync, which is byte-identical to the
	// behavior before modes existed.
	Mode string `yaml:"mode"`
	// Observe tunes observe mode's off-path measurement; ignored in sync mode.
	Observe ObserveConfig `yaml:"observe"`
	// Cache holds the provider prompt-cache policies the HOST applies between and
	// around requests, which no component can reach: keeping an idle session's cached
	// prefix alive, and which TTL tier its breakpoints ask for. Both default to off.
	Cache CacheConfig `yaml:"cache"`
}

// CacheConfig is the `cache:` block: provider prompt-cache policy that lives above the
// pipeline.
//
// It is not a component and could not be. A component runs while a request is in flight,
// and the expensive event here happens when NO request is in flight — a session goes idle,
// its cached prefix lapses, and the next turn re-bills the whole prefix at the creation
// rate. Measured on this service over 19,805 requests: 742 such requests (3.7% of traffic)
// cost $741.07, which is 23.6% of all spend, at $0.9987 each against $0.1178 for a request
// that hit — an 8.5x penalty. $584.83 of it sits in misses whose idle gap was under an hour.
//
// Every field is off or zero by default. KeepAlive spends the caller's money without the
// caller asking, so it is opt-in per account, capped, and reported per ping.
type CacheConfig struct {
	// KeepAlive turns on the idle keep-alive: for a session that has been idle
	// IdleSeconds, re-read its cached prefix once so the provider refreshes the TTL.
	//
	// Off by default and deliberately so. A ping is a real upstream request that costs
	// real money on the caller's own credential — small money (a cache read is 0.1x base
	// input where re-creating the prefix is 1.25x, so the saving:ping ratio is 11.5:1),
	// but money the caller did not ask to spend. That is a consent decision, not a
	// default.
	KeepAlive bool `yaml:"keepalive"`
	// KeepAliveIdleSeconds is X: how long a session must be idle before the first ping.
	// 0 = DefaultKeepAliveIdle (280).
	//
	// 280 rather than 240 is measured, not tuned by feel. The provider's default lifetime
	// is 5 minutes and "the lifetime is measured from the start of the request that writes
	// or reads the cache entry, not from the end of its response", so the budget is 300s
	// from the previous request's START. Simulated over the production window: X=240 wastes
	// 111 pings on gaps that would have hit anyway, X=280 wastes 53, and the net moves from
	// +$94.85 to +$125.08.
	KeepAliveIdleSeconds int `yaml:"keepalive_idle_seconds"`
	// KeepAliveMaxPings is K: the most pings one idle span may send. 0 =
	// DefaultKeepAliveMaxPings (2).
	//
	// K is the main control on the one waste this mechanism cannot avoid — a session that has
	// ENDED looks exactly like one that is thinking.
	//
	// 2 rather than 3, and NOT as a dollars-for-volume trade: the dollars are not real. K=3's
	// +$5.72 over the 4.47-day window is $1.28/day against a bootstrap CI of [$95, $237] and a
	// 1.4x split-half swing — statistically indistinguishable. What K=3 costs IS measurable:
	// +34% pings (1,226 against 912) onto a gateway path that returned 180 HTTP 429s in the same
	// window; the worst single session goes −$2.42 to −$3.63, 50% worse, with total losses +41%,
	// against a promise to save money and not raise anyone's bill while 85 of 119 pinged sessions
	// already lose; and the credential hold window grows 33%, 14 to 18.7 minutes.
	//
	// The decisive one: K=3's extra value comes from pings FURTHER from the last real request,
	// which is exactly where the ping-onto-a-dead-entry failure lives — the single mode that
	// inverts the feature from saving 11.5x to paying 12.5x.
	//
	// If K=3's dollars are ever wanted, the lever is the prefix floor rather than K:
	// KeepAliveMinPrefixTokens at 50,000 with K=2 gives +$125.12 on 908 pings — the same money
	// for fewer requests, a shorter hold, and no extra exposure to the dead-entry mode.
	KeepAliveMaxPings int `yaml:"keepalive_max_pings"`
	// KeepAliveMaxUSDPerPing refuses a ping whose projected cost exceeds this. 0 =
	// DefaultKeepAliveMaxUSDPerPing.
	//
	// PER PING and not per session, which is the opposite of the obvious design and is
	// measured. Ping cost is bimodal — p50 $0.0004, mean $0.0084, p99 $0.2275, max $0.3780 —
	// because 45.7% of pings fire on single-request sessions with ~1k-token prefixes, so the
	// variance to guard is between pings and not between sessions. A per-SESSION budget, by
	// contrast, truncates exactly the long large-prefix sessions that hold the value: capping
	// the window's pings per session at 20 drops the net from +$164 to $92.34, and at 10 to
	// $54.04. So the guard bounds the outlier ping and never the productive session.
	KeepAliveMaxUSDPerPing float64 `yaml:"keepalive_max_usd_per_ping"`
	// KeepAliveMinPrefixTokens is the billed-prefix floor a session must reach before it is
	// pinged at all, in the provider's own units (cache_read + cache_write of the previous
	// request). 0 = DefaultKeepAliveMinPrefix.
	//
	// This is the gate that makes the policy deployable rather than merely profitable.
	// Combined with skipping a session's FIRST request, it sends 9.8x fewer pings (912 against
	// 8,915 over the production window) for 1.7% less money, because what it drops is the
	// near-free pings on tiny prefixes. That matters twice over: those 8,000 requests are
	// real load on a gateway that already returned 180 HTTP 429s in the same window, and the
	// gate leaves 3,748 of 3,891 sessions untouched entirely — which is the fairness answer as
	// well as the efficiency one.
	KeepAliveMinPrefixTokens int `yaml:"keepalive_min_prefix_tokens"`

	// HeadTTL1h asks for the ONE-HOUR tier on the request's HEAD breakpoints (`tools` and
	// `system`) while leaving the trailing message breakpoint at 5 minutes — the provider's
	// documented mixed-TTL shape, in the order it requires (1h entries must precede 5m
	// ones, and the head precedes the messages by construction).
	//
	// Off by default for a measured reason, and NOT the one people expect. A blanket 1h TTL
	// loses money: the 2.0x write premium falls on every cache-creating request (14,499 of
	// them, −$773.00) while the benefit lands on 290 (+$754.66), net −$18.34. Re-labelling
	// only the head fixes that arithmetic — the premium is paid on 769 head-write events
	// instead — and simulates net positive at every head share, +$20.19 at f=0.10 to
	// +$60.56 at f=0.30.
	//
	// It is still off, because on the models that carry this service's spend the tier does not
	// arrive. Measured live, one request each with the head labelled 1h:
	// aws/claude-haiku-4-5 was GRANTED (ephemeral_1h_input_tokens 36,251 of 36,574 written),
	// aws/claude-sonnet-5 was silently downgraded (0 of 48,212, and an otherwise normal 200).
	// So the ttl field DOES reach the provider — Haiku honouring it proves that — and it is
	// Bedrock's model coverage that refuses: the Claude 4.5 family, not the Opus 5 / 4.8 /
	// Sonnet 5 this service runs. Zero 1h writes appear in 19,805 production requests, which is
	// the same fact from the billing side. So this ships implemented, verifiable and off:
	// Usage.CacheWrite1h is what says whether flipping it on did anything, and while that is
	// zero on a model the honest projection for it is $0.
	HeadTTL1h bool `yaml:"head_ttl_1h"`
	// HeadTTLMinTokens gates the 1h head on the request's own size. 0 =
	// DefaultHeadTTLMinTokens (50,000).
	//
	// A dollar filter, not a probability filter, and that distinction is the whole result.
	// Gating 1h on the best available predictor of a long gap leaves the net unchanged
	// (−$18.32 against −$18.34 blanket) because the premium and the benefit scale with the
	// SAME cache_write on the same requests, so multiplying both by a probability cannot
	// flip the sign. Gating on size does flip it (+$48.81): it excludes small-prefix
	// requests that pay the premium and can never produce a large miss.
	HeadTTLMinTokens int `yaml:"head_ttl_min_tokens"`
}

// Keep-alive and head-TTL defaults. Named constants because the same numbers are asserted
// in tests and quoted in the settings page, and three copies of 280 is how one of them
// becomes 240.
const (
	DefaultKeepAliveIdle          = 280
	DefaultKeepAliveMaxPings      = 2
	DefaultKeepAliveMaxUSDPerPing = 0.25
	DefaultKeepAliveMinPrefix     = 20000
	DefaultHeadTTLMinTokens       = 50000
)

// Resolved returns the block with every zero replaced by its default, so callers never
// repeat the fallbacks. Disabled flags are left alone: `enabled: false` with a tuned
// interval is a legitimate parked configuration.
func (c CacheConfig) Resolved() CacheConfig {
	if c.KeepAliveIdleSeconds <= 0 {
		c.KeepAliveIdleSeconds = DefaultKeepAliveIdle
	}
	if c.KeepAliveMaxPings <= 0 {
		c.KeepAliveMaxPings = DefaultKeepAliveMaxPings
	}
	if c.KeepAliveMaxUSDPerPing <= 0 {
		c.KeepAliveMaxUSDPerPing = DefaultKeepAliveMaxUSDPerPing
	}
	if c.KeepAliveMinPrefixTokens <= 0 {
		c.KeepAliveMinPrefixTokens = DefaultKeepAliveMinPrefix
	}
	if c.HeadTTLMinTokens <= 0 {
		c.HeadTTLMinTokens = DefaultHeadTTLMinTokens
	}
	return c
}

// validate rejects a cache block that would misbehave rather than accepting it and
// degrading quietly. An idle interval at or past the provider's 5-minute lifetime cannot
// refresh anything — the entry is gone before the ping fires — and it is the one mistake
// here that turns a saving into a pure cost, because the ping then WRITES at 1.25x instead
// of reading at 0.1x.
func (c CacheConfig) validate() error {
	if c.KeepAliveIdleSeconds < 0 || c.KeepAliveMaxPings < 0 || c.HeadTTLMinTokens < 0 ||
		c.KeepAliveMaxUSDPerPing < 0 || c.KeepAliveMinPrefixTokens < 0 {
		return fmt.Errorf("config: cache: negative values are not meaningful")
	}
	if c.KeepAliveIdleSeconds >= 300 {
		return fmt.Errorf("config: cache: keepalive_idle_seconds must be under 300 "+
			"(the provider's 5-minute lifetime runs from the previous request's START, "+
			"so a later ping re-creates the entry at 1.25x instead of refreshing it at 0.1x); got %d",
			c.KeepAliveIdleSeconds)
	}
	return nil
}

// ObserveConfig is the `observe:` block.
type ObserveConfig struct {
	// MaxQueue bounds the off-path measurement queue (0 = 256). A full queue drops,
	// counted, and never blocks the request path.
	MaxQueue int `yaml:"max_queue"`
	// Workers is the number of drain goroutines (0 = 1).
	Workers int `yaml:"workers"`
}

// OperatingMode validates and returns the configured mode.
func (c *Config) OperatingMode() (components.Mode, error) {
	return components.ParseMode(c.Mode)
}

// Load reads and parses a YAML config file (strict: unknown keys are rejected).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(b)
}

// LoadBytes parses a YAML config document (strict). Used by the AuthBridge
// plugin's Configure, which receives its subtree as bytes.
func LoadBytes(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true) // reject typos loudly
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.applyPreset(); err != nil {
		return nil, err
	}
	if _, err := c.OperatingMode(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.Cache.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate reports whether a configuration document is usable, without keeping
// anything it builds. It is the full check, not just a parse: LoadBytes catches
// typos, an unknown preset and a bad mode, but only Build catches an unknown
// component name or a malformed per-component block. A hosted deployment saving a
// user-supplied config needs the strict answer at write time, so the failure is a
// 400 on their settings page rather than a surprise on their next agent turn.
func Validate(b []byte) error {
	c, err := LoadBytes(b)
	if err != nil {
		return err
	}
	_, err = c.Build(components.NopEmitter{})
	return err
}

// applyPreset fills an empty Pipeline (and, for rich presets, default component
// configs) from the named preset. Explicit fields in the document always win — they
// were already decoded, so the preset only supplies what the user left unset.
func (c *Config) applyPreset() error {
	if c.Preset == "" {
		return nil
	}
	// Rich preset: a full config doc carrying tuned per-component settings a bare
	// pipeline name-list can't express (e.g. extract_llm's cheap-model routing +
	// thresholds). Decode it into a scratch Config, then use its values only where the
	// user didn't specify (pipeline; per-component config merged with user override).
	if doc, ok := presetConfigs[c.Preset]; ok {
		var pc Config
		dec := yaml.NewDecoder(bytes.NewReader([]byte(doc)))
		dec.KnownFields(true)
		if err := dec.Decode(&pc); err != nil {
			return fmt.Errorf("config: preset %q: %w", c.Preset, err)
		}
		if len(c.Pipeline) == 0 {
			c.Pipeline = pc.Pipeline
		}
		if len(pc.Components) > 0 {
			merged := make(map[string]yaml.Node, len(pc.Components)+len(c.Components))
			for k, v := range pc.Components {
				merged[k] = v
			}
			for k, v := range c.Components { // user config wins per component
				merged[k] = v
			}
			c.Components = merged
		}
		return nil
	}
	p, ok := presets[c.Preset]
	if !ok {
		return fmt.Errorf("config: unknown preset %q", c.Preset)
	}
	if len(c.Pipeline) == 0 {
		c.Pipeline = append([]string(nil), p...)
	}
	return nil
}

// presets map a name to a default pipeline (component names in run order). The
// referenced components are registered by P1+; an unknown name surfaces at
// Build time as a clear error.
var presets = map[string][]string{
	"off":        {}, // passthrough: no components (baseline / A-B control)
	"safe":       {"format", "cachesplit"},
	"balanced":   {"format", "dedup", "failed_run", "cmdfilter", "cachesplit"},
	"aggressive": {"format", "dedup", "failed_run", "cmdfilter", "smartcrush", "extract", "extract_llm", "cachesplit"},
	// coding: deterministic only. It named `skeleton` until 2026-08 — which is behind the
	// `cg_skeleton` build tag and therefore NOT registered in a normal binary, so
	// `preset: coding` failed to build with `unknown component "skeleton"` for every user
	// who selected it. TestEveryPresetBuilds now makes that class of breakage impossible.
	// The substitutes are the components measured to actually act on Claude Code traffic
	// (see docs/results/measured-2026-08.md).
	"coding": {"format", "toon", "dedup", "cmdfilter", "extract", "cachesplit"},
	"mcp":    {"format", "smartcrush", "cachesplit"},
	// agent: tuned for long agentic sessions (e.g. Claude Code on SWE-bench),
	// where the dominant cost is the transcript of tool outputs (file reads)
	// re-sent every turn. mask (drop old tool outputs) is the biggest lever
	// there — ~27% content-token savings with no task-reward loss in the
	// eval-containers SWE-bench sweep (see docs/RESULTS.md); extract + failed_run
	// + dedup add relevance/supersession/dup wins; cachesplit keeps the shared system
	// prefix cacheable. Order: lossless first, then offload old-then-large, cache last.
	"agent": {"format", "dedup", "failed_run", "mask", "extract", "extract_llm", "cachesplit"},
	// general: the recommended all-round pipeline, safe+effective for any agent/
	// benchmark. Ordered by pipeline semantics: lossless repack first (format, toon,
	// textclean — textclean is the plain-text one, and plain text is what most tool
	// output is: 1,724 of 1,748 distinct outputs in the captures measured here)
	// so downstream token counts are honest; cheap structural offloaders next (dedup,
	// failed_run, cmdfilter); age-based mask; relevance-based extract; the blind
	// head/tail collapse as the last-resort catch-all for anything still oversized;
	// cachesplit last (it edits `system`, not `messages`). Every offloader
	// defaults to marker_mode:full (reversible via the injected expand tool) and skips
	// content already carrying a placeholder, so they never double-reduce. Combines the
	// levers that proved reward-neutral in the benchmark sweeps without stacking the
	// two overlapping old-context reducers (mask is the one kept; summarize
	// is its own preset — see docs/components.md redundancy notes).
	"general": {"format", "toon", "textclean", "dedup", "failed_run", "cmdfilter", "mask", "extract", "extract_llm", "collapse", "cachesplit"},
	// summarize restructures the whole transcript (changes the message count) — run
	// it alone so no other component's in-place edits race apply's rebuild.
	"summarize": {"summarize"},
	// agentdiet reproduces the published AgentDiet baseline (arXiv:2509.23586, FSE
	// 2026) so it can be A/B'd against our own reducers on the same traffic. Its
	// tuned thresholds live in presetConfigs; it runs with `format` only, because
	// the method's whole claim is what ONE age-targeted LLM reflection achieves —
	// stacking our offloaders beside it would reduce the same tool outputs first and
	// there would be nothing left to attribute.
	"agentdiet": {"format", "agentdiet", "cachesplit"},
	// codesmart / codesafe are the SWE-bench study's winning configs, shipped as the
	// recommended defaults (codesmart is the proxy default). Their tuned per-component
	// settings live in presetConfigs; the name-lists here keep PresetPipeline (used by
	// /compact?preset=) resolving them.
	"codesmart": {"format", "toon", "dedup", "failed_run", "cmdfilter", "extract_llm", "extract", "cachesplit"},
	"codesafe":  {"format", "dedup", "failed_run", "cmdfilter", "extract", "collapse", "cachesplit"},
}

// presetConfigs carries FULL config docs for presets whose behavior depends on tuned
// per-component settings a bare pipeline name-list cannot express. Derived from the
// SWE-bench study (deploy/harbor/swebench.py), but NOT identical to it any more — the
// study's arm predates `toon` and used `cacheinject` where this uses `cachesplit`. The
// comment used to claim "kept verbatim", which was false and load-bearing: it is what a
// reader relies on when deciding whether the published numbers describe the shipped
// default. They describe an ancestor of it. Treat any preset change as a reason to
// re-measure, not as a documentation edit.
//   - codesmart (the winning cache-aware config, and the proxy default): the LLM
//     relevance-trimmer extract_llm routed to the CHEAP model (model.source: config,
//     nil-when-unset ⇒ it silently no-ops to deterministic — see docs), gated at 3000
//     tok so most turns make no model call, ≤4 calls/req; the free deterministic extract
//     catches smaller noise; cachesplit keeps the shared system prefix warm.
//   - codesafe: the deterministic-only variant (NO LLM, by policy) — same structural
//     offloaders plus a blind collapse fallback, zero model calls.
//
// Component defaults are left untouched, so general/agent/aggressive are unaffected.
var presetConfigs = map[string]string{
	"codesmart": `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    strategy: code
    model:
      source: config
    min_tokens: 3000
    trigger:
      min_request_tokens: 3000
    llm_every_n_requests: 1
    llm_max_per_request: 4`,
	"codesafe": `pipeline: [format, dedup, failed_run, cmdfilter, extract, collapse, cachesplit]
components:
  collapse:
    max_tokens: 3000`,
	// agentdiet: the published AgentDiet baseline at the paper's tuned hyperparameters
	// (a=2, b=1, θ=500) and the authors' artifact apply-gate (saved >= 400 || keep <
	// 0.8). Routed to the CHEAP model because the method's economics depend on the
	// reflection model being much cheaper than the agent's — the paper's own choice was
	// GPT-5 mini against Claude 4 Sonnet, ~12x cheaper. With no cheap model configured
	// the component replays what it already froze and reduces nothing new, which is a
	// visible no-op rather than a silent one (/stats reports zero agentdiet activity).
	"agentdiet": `pipeline: [format, agentdiet, cachesplit]
components:
  agentdiet:
    delay_steps: 2
    context_steps: 1
    min_step_tokens: 500
    min_saved_tokens: 400
    max_keep_ratio: 0.8
    model:
      source: config`,
}

// PresetPipeline returns the default pipeline (ordered component names) for a
// named preset. It's the safe way to resolve a caller-supplied preset name (e.g.
// a /compact ?preset= query param) — a plain map lookup, no YAML parsing of
// untrusted input.
func PresetPipeline(name string) ([]string, bool) {
	p, ok := presets[name]
	return append([]string(nil), p...), ok
}

// Build constructs the ordered pipeline from the config, wiring each named
// component with its raw config block.
func (c *Config) Build(e components.Emitter) (*components.Pipeline, error) {
	comps := make([]components.Component, 0, len(c.Pipeline))
	for _, name := range c.Pipeline {
		var raw []byte
		if node, ok := c.Components[name]; ok {
			b, err := yaml.Marshal(&node)
			if err != nil {
				return nil, fmt.Errorf("config: marshal %q block: %w", name, err)
			}
			raw = b
		}
		comp, err := components.New(name, raw)
		if err != nil {
			return nil, err
		}
		comps = append(comps, comp)
	}
	return components.NewPipeline(comps, e), nil
}

// NewStore builds the configured state store: an in-memory TTL+LRU by default,
// or a no-op store when store.enabled is false (disables offload reversibility).
func (c *Config) NewStore() store.Store {
	if c.Store.Enabled != nil && !*c.Store.Enabled {
		return store.Nop{}
	}
	return store.NewMemory(c.Store)
}
