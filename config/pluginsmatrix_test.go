package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// docs/reference/plugins-matrix.md says which cache strategy each plugin offers for which
// provider. This test keeps two of its tables equal to the code:
//
//   - "The proxy's strategy table": the Providers tag of each row of Strategies.
//   - "Cache strategies each plugin offers": the strategies that a plugin's filter gives from
//     Strategies, and its default.
//
// A plugin keeps its filter in its own language (Python or JavaScript), so this file holds a Go
// copy of each rule. The copy is the part to change when a plugin changes its filter. Rows of
// the page other than these two tables (presets, routes, versions, commands, keep-alive timing)
// are checked by hand.
const pluginsMatrixDoc = "../docs/reference/plugins-matrix.md"

const pluginsMatrixFix = "update docs/reference/plugins-matrix.md (and the Go copy of the plugin filter in config/pluginsmatrix_test.go if a plugin filter changed)"

func hasTag(tags []string, wanted ...string) bool {
	for _, t := range tags {
		for _, w := range wanted {
			if t == w {
				return true
			}
		}
	}
	return false
}

// pluginFilters mirrors the filter of each plugin. The key is the first cell of the table row.
var pluginFilters = map[string]struct {
	// offers is the plugin's rule on one Providers tag list.
	offers func(tags []string) bool
	// defaultName is the strategy the plugin starts with, given the names it offers.
	defaultName func(offered []string) string
}{
	// context-guru-plugin/scripts/settings.py: PLUGIN_PROVIDERS
	"Claude Code (Anthropic)": {
		offers:      func(t []string) bool { return hasTag(t, "anthropic", "any") },
		defaultName: proxyDefault,
	},
	// codex-marketplace/plugins/context-guru/scripts/codex_plugin.py: PROVIDERS, DEFAULT_STRATEGY
	"Codex (OpenAI Responses)": {
		offers: func(t []string) bool {
			if len(t) == 0 {
				return false
			}
			for _, p := range t {
				if p != "openai" && p != "any" {
					return false
				}
			}
			return true
		},
		defaultName: func([]string) string { return "30-min-ping" },
	},
	// opencode-plugin/index.js: offeredStrategies, with the routed providers as dialects
	"OpenCode (Anthropic)": {
		offers:      func(t []string) bool { return hasTag(t, "any", "anthropic") },
		defaultName: proxyDefault,
	},
	"OpenCode (OpenAI)": {
		offers:      func(t []string) bool { return hasTag(t, "any", "openai") },
		defaultName: proxyDefault,
	},
	"OpenCode (Anthropic and OpenAI)": {
		offers:      func(t []string) bool { return hasTag(t, "any", "anthropic", "openai") },
		defaultName: proxyDefault,
	},
	// pi-package/scripts/pi_plugin.py: PROVIDERS and the offer filter
	"Pi (Anthropic and OpenAI)": {
		offers:      func(t []string) bool { return hasTag(t, "any") || (hasTag(t, "anthropic") && hasTag(t, "openai")) },
		defaultName: proxyDefault,
	},
	// bob-plugin/context-guru/scripts/bob_plugin.py: PROVIDERS
	"IBM Bob (OpenAI Chat Completions)": {
		offers:      func(t []string) bool { return hasTag(t, "openai-chat", "any") },
		defaultName: func([]string) string { return "none" },
	},
}

// proxyDefault is the rule of a plugin that starts with the proxy default when it is offered, and
// with none when it is not.
func proxyDefault(offered []string) string {
	if hasTag(offered, DefaultStrategy) {
		return DefaultStrategy
	}
	return "none"
}

// matrixSection returns the lines of the first markdown table after the heading.
func matrixSection(t *testing.T, doc, heading string) [][]string {
	t.Helper()
	i := strings.Index(doc, "\n## "+heading+"\n")
	if i < 0 {
		t.Fatalf("plugins-matrix.md has no section %q: %s", heading, pluginsMatrixFix)
	}
	var rows [][]string
	started := false
	for _, line := range strings.Split(doc[i+1:], "\n")[1:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if started {
				break
			}
			continue
		}
		started = true
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.TrimSpace(c))
		}
		if len(cells) > 0 && (strings.HasPrefix(cells[0], "---") || strings.HasPrefix(cells[0], "Plugin") || strings.HasPrefix(cells[0], "Strategy")) {
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatalf("section %q of plugins-matrix.md has no rows: %s", heading, pluginsMatrixFix)
	}
	return rows
}

var backtickName = regexp.MustCompile("`([a-z0-9-]+)`")

func backticked(cell string) []string {
	var out []string
	for _, m := range backtickName.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !hasTag(b, x) {
			return false
		}
	}
	return true
}

func TestPluginsMatrixStrategyTags(t *testing.T) {
	raw, err := os.ReadFile(pluginsMatrixDoc)
	if err != nil {
		t.Fatal(err)
	}
	rows := matrixSection(t, string(raw), "The proxy's strategy table")
	seen := map[string]bool{}
	for _, r := range rows {
		name := strings.Trim(r[0], "`")
		s, ok := LookupStrategy(name)
		if !ok {
			t.Errorf("plugins-matrix.md lists strategy %q, which is not in config.Strategies: %s", name, pluginsMatrixFix)
			continue
		}
		seen[name] = true
		var tags []string
		for _, p := range strings.Split(r[1], ",") {
			tags = append(tags, strings.TrimSpace(p))
		}
		if !sameSet(tags, s.Providers) {
			t.Errorf("plugins-matrix.md gives %q the providers %v. config.Strategies has %v: %s", name, tags, s.Providers, pluginsMatrixFix)
		}
		spends := strings.EqualFold(r[2], "yes")
		if spends != s.Spends {
			t.Errorf("plugins-matrix.md gives %q Spends=%v. config.Strategies has %v: %s", name, spends, s.Spends, pluginsMatrixFix)
		}
	}
	for _, s := range Strategies {
		if !seen[s.Name] {
			t.Errorf("config.Strategies has %q and plugins-matrix.md has no row for it: %s", s.Name, pluginsMatrixFix)
		}
	}
}

func TestPluginsMatrixOfferedStrategies(t *testing.T) {
	raw, err := os.ReadFile(pluginsMatrixDoc)
	if err != nil {
		t.Fatal(err)
	}
	rows := matrixSection(t, string(raw), "Cache strategies each plugin offers")
	seen := map[string]bool{}
	for _, r := range rows {
		if len(r) < 4 {
			t.Errorf("plugins-matrix.md: row %q has %d cells, want 4: %s", r[0], len(r), pluginsMatrixFix)
			continue
		}
		f, ok := pluginFilters[r[0]]
		if !ok {
			t.Errorf("plugins-matrix.md has the row %q and config/pluginsmatrix_test.go has no filter for it: %s", r[0], pluginsMatrixFix)
			continue
		}
		seen[r[0]] = true
		var want []string
		for _, s := range Strategies {
			if f.offers(s.Providers) {
				want = append(want, s.Name)
			}
		}
		got := backticked(r[2])
		if !sameSet(got, want) {
			t.Errorf("%s: plugins-matrix.md says it offers %v. Its filter on config.Strategies gives %v: %s", r[0], got, want, pluginsMatrixFix)
		}
		wantDefault := f.defaultName(want)
		gotDefault := backticked(r[3])
		if len(gotDefault) != 1 || gotDefault[0] != wantDefault {
			t.Errorf("%s: plugins-matrix.md says the default is %v. The plugin starts with %q: %s", r[0], gotDefault, wantDefault, pluginsMatrixFix)
		}
		if !hasTag(want, wantDefault) {
			t.Errorf("%s: the default %q is not in the offered list %v", r[0], wantDefault, want)
		}
	}
	for name := range pluginFilters {
		if !seen[name] {
			t.Errorf("config/pluginsmatrix_test.go has a filter for %q and plugins-matrix.md has no row for it: %s", name, pluginsMatrixFix)
		}
	}
}
