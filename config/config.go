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
var presets = map[string][]string{
	"off":        {}, // passthrough: no components (baseline / A-B control)
	"safe":       {"format", "cachesplit"},
	"balanced":   {"format", "dedup", "failed_run", "cmdfilter", "cachesplit"},
	"aggressive": {"format", "dedup", "failed_run", "cmdfilter", "smartcrush", "extract", "extract_llm", "cachesplit"},
	"coding":     {"format", "skeleton", "cmdfilter", "cachesplit"},
	"mcp":        {"format", "smartcrush", "cachesplit"},
	// agent: tuned for long agentic sessions (e.g. Claude Code on SWE-bench),
	// where the dominant cost is the transcript of tool outputs (file reads)
	// re-sent every turn. mask (drop old tool outputs) is the biggest lever
	// there — ~27% content-token savings with no task-reward loss in the
	// eval-containers SWE-bench sweep (see docs/RESULTS.md); extract + failed_run
	// + dedup add relevance/supersession/dup wins; cachesplit keeps the shared system
	// prefix cacheable. Order: lossless first, then offload old-then-large, cache last.
	"agent": {"format", "dedup", "failed_run", "mask", "extract", "extract_llm", "cachesplit"},
	// general: the recommended all-round pipeline, safe+effective for any agent/
	// benchmark. Ordered by pipeline semantics: lossless repack first (format, toon)
	// so downstream token counts are honest; cheap structural offloaders next (dedup,
	// failed_run, cmdfilter); age-based mask; relevance-based extract; the blind
	// head/tail collapse as the last-resort catch-all for anything still oversized;
	// cachesplit last (it edits `system`, not `messages`). Every offloader
	// defaults to marker_mode:full (reversible via the injected expand tool) and skips
	// content already carrying a placeholder, so they never double-reduce. Combines the
	// levers that proved reward-neutral in the benchmark sweeps without stacking the
	// two overlapping old-context reducers (mask is the one kept; summarize
	// is its own preset — see docs/components.md redundancy notes).
	"general": {"format", "toon", "dedup", "failed_run", "cmdfilter", "mask", "extract", "extract_llm", "collapse", "cachesplit"},
	// summarize restructures the whole transcript (changes the message count) — run
	// it alone so no other component's in-place edits race apply's rebuild.
	"summarize": {"summarize"},
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
