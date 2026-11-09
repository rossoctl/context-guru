package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Named cache strategies: one table, owned by the proxy. A name stands for a set of `cache:`
// keys. The Claude Code plugin and the OpenCode plugin used to keep their own copy of this table;
// they read it from `context-guru-proxy strategies --json` instead.
//
// Names: none, auto, 1-ping, 2-ping, 1-hour-head, 30-min-ping (the Codex name).
//
// RULES
//   - `cache.strategy: <name>` fills the keys of the table row.
//   - A key the file sets itself ALWAYS wins over the row. A hand-tuned file keeps working.
//     A key with a null value (`keepalive_max_pings:` and nothing after it) counts as set,
//     so the row does not fill it and the library default applies.
//   - No `strategy` key: nothing here runs, and the config is the same as before.
//   - An unknown name is a config error. The alias in StrategyAliases is read as its new name.
//   - `none` has no keys: it sets nothing. (The plugin writes no file for it; the proxy only
//     accepts the name.)
//
// The numbers are written out here, not read from the Default* constants, on purpose: a later
// change to a default must not silently re-tune a strategy that an install already uses. A drift
// test pins this table to context-guru-plugin/scripts/settings.py until the plugin reads it from
// the proxy.

// DefaultStrategy is the name the plugin picks when the user has not chosen one. The proxy does
// NOT apply it: a file with no `strategy` behaves as before.
const DefaultStrategy = "auto"

// StrategyAliases maps an old name to its current name.
var StrategyAliases = map[string]string{"5-min-ping": "auto"}

// StrategyKey is one `cache:` key a strategy sets. Key is the YAML key. Value is a bool, an int
// or a float64.
type StrategyKey struct {
	Key   string
	Value any
}

// Strategy is one row of the table.
type Strategy struct {
	Name string
	// Spends is true when the strategy sends model calls on the caller's credential.
	Spends bool
	// Desc is the one-line text for a picker.
	Desc string
	// Providers says which provider(s) the strategy is meant for: "anthropic", "openai" or "any".
	// The proxy does not enforce it. A picker uses it to hide a name that does not fit.
	Providers []string
	Cache     []StrategyKey
}

// Strategies is the table, in picker order.
var Strategies = []Strategy{
	{
		Name:      "none",
		Spends:    false,
		Desc:      "no cache strategy: the preset runs and nothing else. No pings, no model calls, no spend. Written as the ABSENCE of a config file.",
		Providers: []string{"any"},
		Cache:     []StrategyKey{},
	},
	{
		Name:      "auto",
		Spends:    true,
		Desc:      "idle pings that follow the cache lifetime each session was billed at. 5-minute cache: first ping at 240s, at most 2. 1-hour cache: 1 ping at 59 minutes. >=80k-token prefix, <=$0.50/ping. SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"keepalive", true},
			{"keepalive_idle_seconds", 240},
			{"keepalive_max_pings", 2},
			{"keepalive_max_usd_per_ping", 0.5},
			{"keepalive_min_prefix_tokens", 80000},
			{"keepalive_idle_seconds_1h", 3540},
			{"keepalive_max_pings_1h", 1},
		},
	},
	{
		Name:      "1-ping",
		Spends:    true,
		Desc:      "at most 1 idle ping per idle span, whichever cache lifetime the session was billed at (240s into a 5-minute cache, 59 minutes into a 1-hour cache). SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"keepalive", true},
			{"keepalive_idle_seconds", 240},
			{"keepalive_max_pings", 1},
			{"keepalive_max_usd_per_ping", 0.5},
			{"keepalive_min_prefix_tokens", 80000},
			{"keepalive_idle_seconds_1h", 3540},
			{"keepalive_max_pings_1h", 1},
		},
	},
	{
		Name:      "2-ping",
		Spends:    true,
		Desc:      "at most 2 idle pings per idle span, whichever cache lifetime the session was billed at (a 1-hour cache is pinged at 59 minutes and again 59 minutes later). SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"keepalive", true},
			{"keepalive_idle_seconds", 240},
			{"keepalive_max_pings", 2},
			{"keepalive_max_usd_per_ping", 0.5},
			{"keepalive_min_prefix_tokens", 80000},
			{"keepalive_idle_seconds_1h", 3540},
			{"keepalive_max_pings_1h", 2},
		},
	},
	{
		Name:      "1-hour-head",
		Spends:    false,
		Desc:      "asks for the 1-hour tier on the tools/system breakpoints, and ONLY on requests with a >=50k-token prefix (below that it does nothing at all; the size gate is what makes it pay, +$48.81 against -$18.34 applied blanket). No pings. Measured GRANTED on Haiku 4.5 and SILENTLY DOWNGRADED on Sonnet 5 (zero 1h writes in 19,805 production requests), so on Opus/Sonnet the honest projection is $0 - verify with Usage.CacheWrite1h before believing otherwise, and note both of those measurements were on prefixes UNDER the 50k gate this ships with.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"head_ttl_1h", true},
			{"head_ttl_min_tokens", 50000},
		},
	},
	{
		Name:      "30-min-ping",
		Spends:    true,
		Desc:      "refreshes eligible OpenAI Responses caches shortly before their 30-minute lifetime and spends a small amount of the user's own quota. On a proxy that also serves Anthropic, keepalive: true also turns on the Anthropic keep-alive with the proxy defaults.",
		Providers: []string{"openai"},
		Cache: []StrategyKey{
			{"keepalive", true},
		},
	},
}

// CanonicalStrategy returns the current name for name: itself, or what an alias maps to.
func CanonicalStrategy(name string) string {
	if n, ok := StrategyAliases[name]; ok {
		return n
	}
	return name
}

// LookupStrategy finds a row by name or alias.
func LookupStrategy(name string) (Strategy, bool) {
	name = CanonicalStrategy(name)
	for _, s := range Strategies {
		if s.Name == name {
			return s, true
		}
	}
	return Strategy{}, false
}

// StrategyNames lists the names in table order.
func StrategyNames() []string {
	out := make([]string, len(Strategies))
	for i, s := range Strategies {
		out[i] = s.Name
	}
	return out
}

// applyStrategy fills the cache keys of the named strategy into c.Cache. A key the document sets
// itself is kept. raw is the document the config came from.
func (c *Config) applyStrategy(raw []byte) error {
	if c.Cache.Strategy == "" {
		return nil
	}
	s, ok := LookupStrategy(c.Cache.Strategy)
	if !ok {
		return fmt.Errorf("config: cache: unknown strategy %q (known: %s)",
			c.Cache.Strategy, strings.Join(StrategyNames(), ", "))
	}
	var doc struct {
		Cache map[string]yaml.Node `yaml:"cache"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// A key present in the document is "set", also when its value is zero, false or null.
	missing := map[string]any{}
	for _, k := range s.Cache {
		if _, set := doc.Cache[k.Key]; !set {
			missing[k.Key] = k.Value
		}
	}
	if len(missing) == 0 {
		return nil
	}
	b, err := yaml.Marshal(missing)
	if err != nil {
		return fmt.Errorf("config: cache: strategy %q: %w", s.Name, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c.Cache); err != nil {
		return fmt.Errorf("config: cache: strategy %q: %w", s.Name, err)
	}
	return nil
}

// StrategiesJSON renders the table for a plugin: the default name, the aliases, every row with its
// keys as an object, and "presets": every preset the proxy registers (PresetNames). That is the
// full list. The short picker ladder (off ... ultrahigh) is a plugin choice and is not defined in
// the proxy, so it is not listed here.
func StrategiesJSON() ([]byte, error) {
	type row struct {
		Name      string         `json:"name"`
		Spends    bool           `json:"spends"`
		Desc      string         `json:"desc"`
		Providers []string       `json:"providers"`
		Cache     map[string]any `json:"cache"`
	}
	out := struct {
		Default    string            `json:"default"`
		Aliases    map[string]string `json:"aliases"`
		Strategies []row             `json:"strategies"`
		Presets    []string          `json:"presets"`
	}{Default: DefaultStrategy, Aliases: StrategyAliases, Presets: PresetNames()}
	for _, s := range Strategies {
		r := row{Name: s.Name, Spends: s.Spends, Desc: s.Desc, Providers: s.Providers, Cache: map[string]any{}}
		for _, k := range s.Cache {
			r.Cache[k.Key] = k.Value
		}
		out.Strategies = append(out.Strategies, r)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep ">=" and "<=" readable in the picker text
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
