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
// Names: none, auto, 1-ping, 2-ping, 1-hour-head, 1-hour-on-wait, 1-hour-always, 30-min-ping (the Codex name).
//
// RULES
//   - `cache.strategy: <name>` fills the keys of the table row.
//   - A key the file sets itself ALWAYS wins over the row. A hand-tuned file keeps working.
//     A key with a null value (`keepalive_max_pings:` and nothing after it) counts as set,
//     so the row does not fill it and the library default applies.
//   - No `strategy` key: nothing here runs, and the config is the same as before.
//   - An unknown name is a config error. The alias in StrategyAliases is read as its new name.
//   - `none` has no keys: it sets nothing. (The Claude Code plugin writes no file for it. The
//     Codex plugin writes `strategy: none`. The proxy accepts the name.)
//
// The numbers are written out here, not read from the Default* constants, on purpose: a later
// change to a default must not silently re-tune a strategy that an install already uses. The
// Claude Code plugin keeps no copy: it reads this table with `context-guru-proxy strategies`.
// TestEveryStrategyHasTheSameEffectiveConfigAsItsOldKeys keeps the files it wrote before that valid.
// A test also pins the floor in this table to DefaultKeepAliveMinPrefix, so the two are
// changed together. The table does not write keepalive_tool_wait_min_prefix_tokens: the default
// (20000) applies, so an old strategy file gets the tool-wait floor without a re-render.

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
	// Providers says which provider(s) the strategy is meant for: "anthropic", "openai" (the
	// OpenAI Responses route), "openai-chat" (the OpenAI Chat Completions route, for example IBM
	// Bob) or "any". No row uses "openai-chat" yet. A strategy for both Anthropic and OpenAI lists
	// both. The proxy does not enforce it. A picker uses it to hide a name that does not fit: the
	// Claude Code plugin offers a row that lists anthropic or any, the Codex plugin only a row
	// whose list is exactly openai or any.
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
		Desc:      "idle pings that follow the cache lifetime each session was billed at. Anthropic 5-minute cache: first ping at 240s, at most 2. Anthropic 1-hour cache: 1 ping at 59 minutes. OpenAI: pings at its own cache lifetime. prefix floor 20k tokens for a tool wait, background work, a teammate or a subagent, 80k for a main thread that waits for a person; <=$0.50/ping. SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic", "openai"},
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
		Desc:      "at most 1 idle ping per idle span, on Anthropic and on OpenAI, whichever cache lifetime the session was billed at (Anthropic: 240s into a 5-minute cache, 59 minutes into a 1-hour cache). SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic", "openai"},
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
		Desc:      "at most 2 idle pings per idle span, on Anthropic and on OpenAI, whichever cache lifetime the session was billed at (Anthropic: a 1-hour cache is pinged at 59 minutes and again 59 minutes later). SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic", "openai"},
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
		Desc:      "asks for the 1-hour tier on the tools/system breakpoints ONLY, and ONLY on requests with a >=50k-token prefix (below that it does nothing at all; the size gate is what makes it pay, +$48.81 against -$18.34 applied blanket). It covers only tools and system, a small part of a long prompt that other sessions often keep warm anyway. The message history stays on the 5-minute cache, and this strategy turns the pings off, so the history goes cold after 5 minutes of idle time and the next turn rebuilds it. For long sessions with idle gaps see 1-hour-on-wait. Whether the 1-hour tier is granted depends on the model and the provider, and it changes over time. Measured with fresh writes: claude-haiku-4-5 granted (36,251 of 36,574 tokens at 1h); claude-sonnet-5 REFUSED on an earlier date (0 of 48,212 written at 1h); on 2026-11-19 the same gateway granted 1h on claude-sonnet-5, claude-sonnet-5-5, claude-opus-4-8, claude-opus-5-5 and claude-haiku-5-5 (about 12.6k tokens each). A zero in Usage.CacheWrite1h counts only on a request that wrote the entry: a head that is only read shows zero too.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"head_ttl_1h", true},
			{"head_ttl_min_tokens", 50000},
		},
	},
	{
		Name:      "1-hour-on-wait",
		Spends:    true,
		Desc:      "MAIN AGENT ONLY. auto's keep-alive, except that when the main agent has a long wait (a person, a teammate, background work, or an unknown cause) the first idle ping, at the usual time, carries every cache marker at the 1-hour tier and is the ONLY ping of that wait: no 59-minute ping. On the gateway measured on 2026-11-19 that switch is a read, not a rewrite, so one read at 0.1x keeps the whole prompt for 1 hour and no request pays the 2x write premium. Tool waits and every subagent thread behave exactly as auto (5-minute pings). The 1-hour grant is unknown until proven, per provider, route and model: the first long wait is a probe, and the route is proven when the thread returns more than 5 minutes later and still reads the prompt. If the return is a cold rebuild (a silent downgrade), the proxy stops sending 1-hour pings on that route and falls back to auto. If the ping itself rewrites the prompt (expected on Anthropic first-party, not tested), the route is COSTLY: the proxy stops the pings there, and each main thread runs as auto until its first cold rebuild of a prefix of at least 150,000 tokens (keepalive_one_hour_rebuild_tokens). That rebuild is sent with every cache marker at 1 hour, and from then on the thread writes at 1 hour and gets NO pings. The thread that paid the rewrite does the same. Subagents are unchanged. If a 1-hour request that wrote tokens is billed at 5 minutes, that mode stops for the route. Simulated (hosted, 97 main sessions, free switch): A -24%; on a costly route the fallback is -12% against auto, owner's laptop -25%. SPENDS THE CALLER'S CREDENTIAL while nobody is at the keyboard.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"keepalive", true},
			{"keepalive_idle_seconds", 240},
			{"keepalive_max_pings", 2},
			{"keepalive_max_usd_per_ping", 0.5},
			{"keepalive_min_prefix_tokens", 80000},
			{"keepalive_idle_seconds_1h", 3540},
			{"keepalive_max_pings_1h", 1},
			{"keepalive_one_hour_on_wait", true},
		},
	},
	{
		Name:      "1-hour-always",
		Spends:    true,
		Desc:      "OPT-IN ONLY, AND ON MOST USERS IT DOES NOT PAY OFF. The MAIN agent writes at the 1-hour tier from its first request, on any route: every existing cache marker (tools, system, history) of every real main-thread request carries 1 hour, none is added, and the main thread gets NO ping. Subagents keep the 5-minute cache and auto's pings. It pays the 1-hour write premium (2x instead of 1.25x) on every write, and most sessions have few long waits. Simulated on Bedrock-shaped data, main threads only: hosted, 97 main sessions, +22% cost against auto, 77 worse, the premium 39% of the cost; the owner's laptop, 33 sessions, -25%. Pick 1-hour-on-wait first: it was -24% hosted and -36% on the laptop. A 1-hour request that wrote tokens and is billed at 5 minutes stops the mode for the route (a silent downgrade). See #1261. SPENDS THE CALLER'S CREDENTIAL on subagent pings while nobody is at the keyboard.",
		Providers: []string{"anthropic"},
		Cache: []StrategyKey{
			{"keepalive", true},
			{"keepalive_idle_seconds", 240},
			{"keepalive_max_pings", 2},
			{"keepalive_max_usd_per_ping", 0.5},
			{"keepalive_min_prefix_tokens", 80000},
			{"keepalive_idle_seconds_1h", 3540},
			{"keepalive_max_pings_1h", 1},
			{"keepalive_one_hour_always", true},
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
