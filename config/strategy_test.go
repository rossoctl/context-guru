package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestEveryStrategyHasTheSameEffectiveConfigAsItsOldKeys. The Claude Code plugin used to write the
// `cache:` keys of a strategy into the per-port file. It now writes `cache.strategy: <name>`. The
// files in testdata/cc-old-strategy-files are what the old plugin wrote (rendered by the plugin at
// the commit before the change, with preset off). For each one, the Resolved() cache block that the
// proxy runs must be the same for the old file and for the new form. Old files stay valid for users
// who armed a strategy before this change.
func TestEveryStrategyHasTheSameEffectiveConfigAsItsOldKeys(t *testing.T) {
	for _, name := range []string{"auto", "1-ping", "2-ping", "1-hour-head"} {
		t.Run(name, func(t *testing.T) {
			old, err := os.ReadFile("testdata/cc-old-strategy-files/" + name + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(old), "strategy="+name) || strings.Contains(string(old), "strategy:") {
				t.Fatalf("%s: the fixture is not an old-form file", name)
			}
			co, err := LoadBytes(old)
			if err != nil {
				t.Fatalf("old file: %v", err)
			}
			cn, err := LoadBytes([]byte("preset: off\ncache:\n  strategy: " + name + "\n"))
			if err != nil {
				t.Fatalf("new form: %v", err)
			}
			// Resolved() is what the proxy runs on. Strategy is only the name it was filled from.
			ro, rn := co.Cache.Resolved(), cn.Cache.Resolved()
			rn.Strategy = ""
			if !reflect.DeepEqual(ro, rn) {
				t.Errorf("%s: effective config differs\n old: %+v\n new: %+v", name, ro, rn)
			}
			if co.Preset != cn.Preset {
				t.Errorf("%s: preset old %q, new %q", name, co.Preset, cn.Preset)
			}
		})
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
