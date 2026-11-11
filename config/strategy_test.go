package config

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// pyDump reads STRATEGIES, DEFAULT_STRATEGY and STRATEGY_ALIASES from the plugin's settings.py and
// prints them as JSON. It evaluates only those assignments, with ast, so the file's other code
// never runs. The test fails when python3 is missing: a skip would prove nothing.
const pyDump = `
import ast, json, sys
src = open(sys.argv[1]).read()
ns = {}
for n in ast.parse(src).body:
    t = n.targets[0] if isinstance(n, ast.Assign) else getattr(n, "target", None)
    if getattr(t, "id", "") in ("DEFAULT_STRATEGY", "STRATEGY_ALIASES", "STRATEGIES"):
        exec(compile(ast.Module([n], []), "x", "exec"), ns)
print(json.dumps({"default": ns["DEFAULT_STRATEGY"], "aliases": ns["STRATEGY_ALIASES"],
                  "strategies": [dict(name=k, **v) for k, v in ns["STRATEGIES"].items()]}))
`

// TestStrategyTableMatchesPlugin pins the Go table to settings.py, name by name and key by key.
// It stays until the Claude Code plugin reads the table from the proxy.
func TestStrategyTableMatchesPlugin(t *testing.T) {
	out, err := exec.Command("python3", "-I", "-c", pyDump, "../context-guru-plugin/scripts/settings.py").Output()
	if err != nil {
		t.Fatalf("python3 could not read settings.py: %v", err)
	}
	var py struct {
		Default    string            `json:"default"`
		Aliases    map[string]string `json:"aliases"`
		Strategies []struct {
			Name   string         `json:"name"`
			Spends bool           `json:"spends"`
			Desc   string         `json:"desc"`
			Cache  map[string]any `json:"cache"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(out, &py); err != nil {
		t.Fatal(err)
	}
	if py.Default != DefaultStrategy {
		t.Errorf("default: py %q, go %q", py.Default, DefaultStrategy)
	}
	if !reflect.DeepEqual(py.Aliases, StrategyAliases) {
		t.Errorf("aliases: py %v, go %v", py.Aliases, StrategyAliases)
	}
	// The Python table has the Anthropic and "any" rows. The Go table adds the Codex rows after
	// them. strategy_codex_test.go checks the offered set and that the Codex forms
	// (`strategy: <name>` and `keepalive: true|false`) give the same effective config.
	if len(py.Strategies)+1 != len(Strategies) {
		t.Fatalf("py has %d strategies, go has %d (expect py + the Codex row)", len(py.Strategies), len(Strategies))
	}
	for i, p := range py.Strategies {
		g := Strategies[i]
		if g.Name != p.Name || g.Spends != p.Spends || g.Desc != p.Desc {
			t.Errorf("row %d (%s): name/spends/desc differ from py", i, p.Name)
		}
		got := map[string]any{}
		for _, k := range g.Cache {
			got[k.Key] = k.Value
		}
		if len(got) != len(p.Cache) {
			t.Errorf("%s: py has %d keys, go has %d", p.Name, len(p.Cache), len(got))
		}
		for k, pv := range p.Cache {
			gb, _ := json.Marshal(got[k])
			pb, _ := json.Marshal(pv)
			if string(gb) != string(pb) {
				t.Errorf("%s.%s: py %s, go %s", p.Name, k, pb, gb)
			}
		}
	}
}

func TestStrategyFillsKeys(t *testing.T) {
	c, err := LoadBytes([]byte("cache:\n  strategy: 2-ping\n"))
	if err != nil {
		t.Fatal(err)
	}
	k := c.Cache
	if !k.KeepAlive || k.KeepAliveIdleSeconds != 240 || k.KeepAliveMaxPings != 2 ||
		k.KeepAliveMaxUSDPerPing != 0.5 || k.KeepAliveMinPrefixTokens != 80000 ||
		k.KeepAliveIdleSeconds1h != 3540 || k.KeepAliveMaxPings1h != 2 {
		t.Fatalf("2-ping keys wrong: %+v", k)
	}
	c, err = LoadBytes([]byte("cache:\n  strategy: 1-hour-head\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Cache.HeadTTL1h || c.Cache.HeadTTLMinTokens != 50000 || c.Cache.KeepAlive {
		t.Fatalf("1-hour-head keys wrong: %+v", c.Cache)
	}
}

func TestStrategyAliasAndNone(t *testing.T) {
	c, err := LoadBytes([]byte("cache:\n  strategy: 5-min-ping\n"))
	if err != nil || !c.Cache.KeepAlive || c.Cache.KeepAliveMaxPings1h != 1 {
		t.Fatalf("alias: %v %+v", err, c)
	}
	c, err = LoadBytes([]byte("cache:\n  strategy: none\n"))
	if err != nil || c.Cache.KeepAlive {
		t.Fatalf("none must set nothing: %v %+v", err, c)
	}
}

func TestStrategyExplicitKeyWins(t *testing.T) {
	// A key the file sets wins, also when it is the zero value (`keepalive: false`).
	c, err := LoadBytes([]byte("cache:\n  strategy: auto\n  keepalive_max_pings: 5\n  keepalive: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.KeepAliveMaxPings != 5 || c.Cache.KeepAlive || c.Cache.KeepAliveIdleSeconds != 240 {
		t.Fatalf("explicit keys must win, others filled: %+v", c.Cache)
	}
}

func TestStrategyUnknownIsAnError(t *testing.T) {
	_, err := LoadBytes([]byte("cache:\n  strategy: nope\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown strategy "nope"`) {
		t.Fatalf("want unknown-strategy error, got %v", err)
	}
}

func TestNoStrategyChangesNothing(t *testing.T) {
	doc := "cache:\n  keepalive: true\n  keepalive_max_pings: 3\n"
	c, err := LoadBytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.KeepAliveIdleSeconds != 0 || c.Cache.KeepAliveMaxPings != 3 || c.Cache.Strategy != "" {
		t.Fatalf("a file with no strategy must stay as written: %+v", c.Cache)
	}
	c, _ = LoadBytes([]byte("preset: off\n"))
	if !reflect.DeepEqual(c.Cache, CacheConfig{}) {
		t.Fatalf("empty cache block must stay empty: %+v", c.Cache)
	}
}

func TestStrategiesJSON(t *testing.T) {
	b, err := StrategiesJSON()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Default    string `json:"default"`
		Strategies []struct {
			Name  string         `json:"name"`
			Cache map[string]any `json:"cache"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Default != "auto" || len(v.Strategies) != 6 || v.Strategies[1].Cache["keepalive_idle_seconds"] != float64(240) {
		t.Fatalf("bad json: %s", b)
	}
}

func TestStrategiesJSONHasPresetsAndProviders(t *testing.T) {
	b, _ := StrategiesJSON()
	var v struct {
		Presets    []string `json:"presets"`
		Strategies []struct {
			Name      string   `json:"name"`
			Providers []string `json:"providers"`
		} `json:"strategies"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.Presets, PresetNames()) || len(v.Strategies) != 6 {
		t.Fatalf("bad json: %s", b)
	}
	for _, s := range v.Strategies {
		if len(s.Providers) != 1 {
			t.Errorf("%s: providers %v", s.Name, s.Providers)
		}
	}
}

// TestStrategiesPinTheGeneralFloorToTheDefault: the table writes literals on purpose, but its prefix
// floor is the general one. A change of DefaultKeepAliveMinPrefix has to change the table with it, or
// strategy users keep the old floor (an explicit value beats the default). The table does not write the
// tool-wait floor, so that default reaches every strategy file, old or new.
func TestStrategiesPinTheGeneralFloorToTheDefault(t *testing.T) {
	seen := 0
	for _, s := range Strategies {
		for _, k := range s.Cache {
			switch k.Key {
			case "keepalive_min_prefix_tokens":
				seen++
				if k.Value != DefaultKeepAliveMinPrefix {
					t.Errorf("%s: keepalive_min_prefix_tokens = %v, want the default %d", s.Name, k.Value, DefaultKeepAliveMinPrefix)
				}
			case "keepalive_tool_wait_min_prefix_tokens":
				t.Errorf("%s writes keepalive_tool_wait_min_prefix_tokens: a strategy file would pin the tool-wait floor", s.Name)
			}
		}
	}
	if seen < 3 {
		t.Errorf("found the prefix floor in %d strategies, want at least 3 (auto, 1-ping, 2-ping)", seen)
	}
}

func TestToolWaitMinPrefixDefaultAndOverride(t *testing.T) {
	if got := (CacheConfig{}).Resolved().KeepAliveToolWaitMinPrefixTokens; got != 20000 {
		t.Errorf("default tool-wait floor = %d, want 20000", got)
	}
	c, err := LoadBytes([]byte("cache:\n  strategy: auto\n  keepalive_tool_wait_min_prefix_tokens: 30000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.Resolved().KeepAliveToolWaitMinPrefixTokens != 30000 || c.Cache.KeepAliveMinPrefixTokens != 80000 {
		t.Errorf("floors = %d / %d", c.Cache.Resolved().KeepAliveToolWaitMinPrefixTokens, c.Cache.KeepAliveMinPrefixTokens)
	}
	if _, err := LoadBytes([]byte("cache:\n  keepalive_tool_wait_min_prefix_tokens: -1\n")); err == nil {
		t.Error("a negative tool-wait floor was accepted")
	}
}
