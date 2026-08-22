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
//
// The LOSSLESS TRIO — format, textclean, searchfold — leads every preset that does any
// deterministic work. All three verify-then-adopt (format re-parses, textclean compares
// informative lines, searchfold checks its own inverse byte-for-byte) so they cannot lose
// content, and running them first makes every downstream token count honest.
// Measured on 2026-08 production traffic before this change:
//   - textclean was in `general` ALONE, on 5,734 of 19,775 requests, while 49.6% of corpus
//     messages carry ANSI and it had zero false positives across 861 acting requests.
//   - searchfold was written, tested, round-trip verified — and in ZERO presets. 22,014
//     tokens on the measured sample went unfolded because nothing ran it.
//
// `linecap` runs directly after `cmdfilter`, and it is the answer to why cmdfilter's 939
// lines of per-command filters have matched exactly two filters in production: the value in
// tool output is not per-command, it is a per-line cap and a duplicate-line collapse, which
// need no command signature. Measured 20.3% of all shipped tokens on the same corpus where
// sixteen rtk command signatures matched zero messages. It runs after cmdfilter so a
// specific filter still gets first refusal on any output it recognizes.
//
// `toon` is RETIRED from every preset (the component and its tests stay, so anyone with
// tabular traffic can enable it explicitly). Production: `not_uniform_object_array`
// 234,437, `below_min_tokens` 64,831, **acted 0 of 5,752 requests**, and an independent
// sweep found 0 convertible candidates in 11.67M tokens. It was costing 1.53 ms and a
// TextTokens call per tool message to convert nothing.
var presets = map[string][]string{
	"off":        {}, // passthrough: no components (baseline / A-B control)
	"safe":       {"format", "textclean", "searchfold", "cachesplit"},
	"balanced":   {"format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter", "linecap", "cachesplit"},
	"aggressive": {"format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter", "linecap", "smartcrush", "extract", "extract_llm", "cachesplit"},
	// coding: deterministic only, no model calls. It named `skeleton` until 2026-08 — which is behind the
	// `cg_skeleton` build tag and therefore NOT registered in a normal binary, so
	// `preset: coding` failed to build with `unknown component "skeleton"` for every user
	// who selected it. TestEveryPresetBuilds now makes that class of breakage impossible.
	// The substitutes are the components measured to actually act on Claude Code traffic
	// (see docs/results/measured-2026-08.md).
	"coding": {"format", "textclean", "searchfold", "dedup", "cmdfilter", "linecap", "extract", "cachesplit"},
	"mcp":    {"format", "textclean", "smartcrush", "cachesplit"},
	// agent: tuned for long agentic sessions (e.g. Claude Code on SWE-bench),
	// where the dominant cost is the transcript of tool outputs (file reads)
	// re-sent every turn. mask (drop old tool outputs) is the biggest lever
	// there — ~27% content-token savings with no task-reward loss in the
	// eval-containers SWE-bench sweep (see docs/RESULTS.md); extract + failed_run
	// + dedup add relevance/supersession/dup wins; cachesplit keeps the shared system
	// prefix cacheable. Order: lossless first, then offload old-then-large, cache last.
	"agent": {"format", "textclean", "searchfold", "dedup", "failed_run", "mask", "extract", "extract_llm", "cachesplit"},
	// general: the recommended all-round pipeline, safe+effective for any agent/
	// benchmark. Ordered by pipeline semantics: the lossless trio first (format,
	// textclean, searchfold — textclean is the plain-text one, and plain text is what most
	// tool output is: 1,724 of 1,748 distinct outputs in the captures measured here)
	// so downstream token counts are honest; cheap structural offloaders next (dedup,
	// failed_run, cmdfilter); age-based mask; relevance-based extract; the blind
	// head/tail collapse as the last-resort catch-all for anything still oversized;
	// cachesplit last (it edits `system`, not `messages`). Every offloader
	// defaults to marker_mode:full (reversible via the injected expand tool) and skips
	// content already carrying a placeholder, so they never double-reduce. Combines the
	// levers that proved reward-neutral in the benchmark sweeps without stacking the
	// two overlapping old-context reducers (mask is the one kept; summarize
	// is its own preset — see docs/components.md redundancy notes).
	"general": {"format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter", "linecap", "mask", "extract", "extract_llm", "collapse", "cachesplit"},
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
	"codesmart": {"format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter", "linecap", "extract_llm", "extract", "cachesplit"},
	"codesafe":  {"format", "textclean", "searchfold", "dedup", "failed_run", "cmdfilter", "linecap", "extract", "collapse", "cachesplit"},
}

// presetConfigs carries FULL config docs for presets whose behavior depends on tuned
// per-component settings a bare pipeline name-list cannot express. Derived from the
// SWE-bench study (deploy/harbor/swebench.py), but NOT identical to it any more — the
// study's arm used `cacheinject` where this uses `cachesplit`, and the lossless trio
// (textclean, searchfold) has since replaced the never-firing `toon`. The
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
	"codesmart": `pipeline: [format, textclean, searchfold, dedup, failed_run, cmdfilter, linecap, extract_llm, extract, cachesplit]
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
	"codesafe": `pipeline: [format, textclean, searchfold, dedup, failed_run, cmdfilter, linecap, extract, collapse, cachesplit]
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
